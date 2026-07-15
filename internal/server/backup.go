package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/s3"
	"github.com/sutantodadang/luncur/internal/store"
)

// createBackup assembles a tar.gz of luncur's state on the data dir:
// a consistent SQLite snapshot (VACUUM INTO), the sealer key file, and one
// logical dump per addon. Individual addon-dump failures degrade to
// warnings — a partial backup beats none. upload=true additionally PUTs
// the archive to the configured S3-compatible bucket (skipped silently
// when no endpoint is configured; that's a valid steady state).
func (s *server) createBackup(ctx context.Context, upload bool) (store.Backup, []string, error) {
	if s.src == nil {
		return store.Backup{}, nil, fmt.Errorf("no data directory configured")
	}
	dir := filepath.Join(s.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return store.Backup{}, nil, err
	}
	// Second-resolution timestamps collide under rapid manual creates; a
	// numeric suffix keeps every archive (and its DB row) unique on disk.
	stamp := "luncur-" + s.nowFn().UTC().Format("20060102-150405")
	name := stamp + ".tar.gz"
	path := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s-%d.tar.gz", stamp, i)
		path = filepath.Join(dir, name)
	}

	var warnings []string

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return store.Backup{}, nil, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var members []string
	addMember := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o600, Size: int64(len(b)),
			ModTime: s.nowFn().UTC(),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
		members = append(members, name)
		return nil
	}
	fail := func(err error) (store.Backup, []string, error) {
		// Best-effort teardown: the archive is being discarded anyway.
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		_ = os.Remove(path)
		return store.Backup{}, warnings, err
	}

	// 1. Consistent DB snapshot.
	dbTmp := filepath.Join(dir, ".snapshot-"+name+".db")
	defer os.Remove(dbTmp)
	if _, err := s.st.DB().Exec(`VACUUM INTO ?`, dbTmp); err != nil {
		// Some drivers reject binds in VACUUM; the path is server-generated.
		if _, err2 := s.st.DB().Exec(
			`VACUUM INTO '` + strings.ReplaceAll(dbTmp, `'`, `''`) + `'`); err2 != nil {
			return fail(fmt.Errorf("db snapshot: %v (bind), %v (inline)", err, err2))
		}
	}
	dbBytes, err := os.ReadFile(dbTmp)
	if err != nil {
		return fail(err)
	}
	if err := addMember("luncur.db", dbBytes); err != nil {
		return fail(err)
	}

	// 2. Sealer key file.
	if s.secretKeyPath != "" {
		if kb, err := os.ReadFile(s.secretKeyPath); err == nil {
			if err := addMember("luncur.key", kb); err != nil {
				return fail(err)
			}
		} else {
			warnings = append(warnings, fmt.Sprintf("sealer key unreadable: %v", err))
		}
	}

	// 3. Addon dumps.
	if addons, err := s.st.AllAddons(); err != nil {
		warnings = append(warnings, fmt.Sprintf("list addons: %v", err))
	} else if len(addons) > 0 && s.execer == nil {
		warnings = append(warnings, "addon dumps skipped: kubernetes exec unavailable")
	} else {
		for _, ad := range addons {
			data, member, err := s.dumpAddon(ctx, ad)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("addon %s: %v", ad.Name, err))
				continue
			}
			if err := addMember(member, data); err != nil {
				return fail(err)
			}
		}
	}

	// 4. Manifest.
	manifest, err := json.MarshalIndent(map[string]any{
		"created_at": s.nowFn().UTC().Format(time.RFC3339),
		"warnings":   warnings,
		"members":    members,
	}, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := addMember("manifest.json", manifest); err != nil {
		return fail(err)
	}

	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return store.Backup{}, warnings, err
	}

	uploaded := false
	if upload {
		switch err := s.uploadBackup(ctx, path); {
		case err == nil:
			uploaded = true
		case errors.Is(err, errNoS3):
			// Not configured — a valid steady state, not a warning.
		default:
			warnings = append(warnings, fmt.Sprintf("s3 upload: %v", err))
		}
	}

	b, err := s.st.CreateBackup(path, info.Size(), uploaded)
	if err != nil {
		return store.Backup{}, warnings, err
	}
	return b, warnings, nil
}

// addonNamespace resolves the Kubernetes namespace an addon's pod runs in —
// its environment's namespace (luncur-<project>-<env>). Falls back to the
// project namespace only for pre-environments addon rows (environment_id 0).
func (s *server) addonNamespace(ad store.Addon) (string, error) {
	if ad.EnvironmentID != 0 {
		env, err := s.st.GetEnvironmentByID(ad.EnvironmentID)
		if err != nil {
			return "", err
		}
		return env.Namespace, nil
	}
	p, err := s.st.GetProjectByID(ad.ProjectID)
	if err != nil {
		return "", err
	}
	return p.Namespace, nil
}

// appNamespace resolves the Kubernetes namespace an app's objects live in —
// its environment's namespace (luncur-<project>-<env>). Falls back to the
// project namespace only for pre-environments app rows (environment_id 0),
// mirroring addonNamespace above.
func (s *server) appNamespace(a store.App) (string, error) {
	if a.EnvironmentID != 0 {
		env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
		if err != nil {
			return "", err
		}
		return env.Namespace, nil
	}
	p, err := s.st.GetProjectByID(a.ProjectID)
	if err != nil {
		return "", err
	}
	return p.Namespace, nil
}

// dumpAddon streams one addon's logical dump via pods/exec. Credentials are
// referenced from the pod's own environment — never placed on the command
// line.
func (s *server) dumpAddon(ctx context.Context, ad store.Addon) ([]byte, string, error) {
	p, err := s.st.GetProjectByID(ad.ProjectID)
	if err != nil {
		return nil, "", err
	}
	ns, err := s.addonNamespace(ad)
	if err != nil {
		return nil, "", err
	}
	pod := "addon-" + ad.Name + "-0"
	var cmd []string
	var member string
	switch ad.Type {
	case "postgres":
		cmd = []string{"sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U "$POSTGRES_USER" -Fc "$POSTGRES_DB"`}
		member = fmt.Sprintf("addons/%s-%s.pgdump", p.Name, ad.Name)
	case "redis":
		cmd = []string{"sh", "-c", `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning SAVE >/dev/null && cat /data/dump.rdb`}
		member = fmt.Sprintf("addons/%s-%s.rdb", p.Name, ad.Name)
	default:
		return nil, "", fmt.Errorf("unknown addon type %q", ad.Type)
	}
	var out, errBuf bytes.Buffer
	dctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := s.execer.ExecPod(dctx, ns, pod, ad.Type, cmd, nil, &out, &errBuf); err != nil {
		return nil, "", fmt.Errorf("%v: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), member, nil
}

var errNoS3 = errors.New("s3 not configured")

// s3ClientFromSettings builds the uploader; errNoS3 when no endpoint is set.
func (s *server) s3ClientFromSettings() (*s3.Client, string, error) {
	endpoint, err := s.st.GetSetting("backup_s3_endpoint")
	if errors.Is(err, store.ErrNotFound) || (err == nil && endpoint == "") {
		return nil, "", errNoS3
	}
	if err != nil {
		return nil, "", err
	}
	bucket, err := s.st.GetSetting("backup_s3_bucket")
	if err != nil {
		return nil, "", fmt.Errorf("backup_s3_bucket not set")
	}
	access, err := s.st.GetSetting("backup_s3_access_key")
	if err != nil {
		return nil, "", fmt.Errorf("backup_s3_access_key not set")
	}
	secretKey, err := s.s3SecretKey()
	if err != nil {
		return nil, "", fmt.Errorf("backup_s3_secret_key: %w", err)
	}
	prefix := "luncur"
	if v, err := s.st.GetSetting("backup_s3_prefix"); err == nil && v != "" {
		prefix = v
	}
	return &s3.Client{
		Endpoint: endpoint, Bucket: bucket,
		AccessKey: access, SecretKey: secretKey,
	}, prefix, nil
}

func (s *server) uploadBackup(ctx context.Context, path string) error {
	cl, prefix, err := s.s3ClientFromSettings()
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return cl.Put(ctx, prefix+"/"+filepath.Base(path), f, info.Size())
}

// pruneBackups keeps the newest backup_keep (default 7) backups, removing
// local files, DB rows, and (best-effort) remote objects for the rest.
func (s *server) pruneBackups(ctx context.Context) (int, error) {
	keep := 7
	if v, err := s.st.GetSetting("backup_keep"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keep = n
		}
	}
	rows, err := s.st.ListBackups() // newest first
	if err != nil {
		return 0, err
	}
	if len(rows) <= keep {
		return 0, nil
	}
	cl, prefix, s3Err := s.s3ClientFromSettings()
	removed := 0
	for _, b := range rows[keep:] {
		if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
			log.Printf("prune backup %d: %v", b.ID, err)
		}
		if b.Uploaded && s3Err == nil {
			if err := cl.Delete(ctx, prefix+"/"+filepath.Base(b.Path)); err != nil {
				log.Printf("prune remote backup %d: %v", b.ID, err)
			}
		}
		if err := s.st.DeleteBackup(b.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func backupJSON(b store.Backup, warnings []string) map[string]any {
	out := map[string]any{
		"id": b.ID, "path": b.Path, "size_bytes": b.SizeBytes,
		"uploaded": b.Uploaded, "created_at": b.CreatedAt,
	}
	if warnings != nil {
		out["warnings"] = warnings
	}
	return out
}

func (s *server) handleCreateBackup(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req struct {
		NoUpload bool `json:"no_upload"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
	}
	b, warnings, err := s.createBackup(r.Context(), !req.NoUpload)
	if err != nil {
		log.Printf("create backup: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusCreated, backupJSON(b, warnings))
}

func (s *server) handleListBackups(w http.ResponseWriter, r *http.Request, _ store.User) {
	rows, err := s.st.ListBackups()
	if err != nil {
		log.Printf("list backups: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		out = append(out, backupJSON(b, nil))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handlePruneBackups(w http.ResponseWriter, r *http.Request, _ store.User) {
	removed, err := s.pruneBackups(r.Context())
	if err != nil {
		log.Printf("prune backups: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// runBackupSchedule performs one scheduled-backup tick: skip unless the
// backup_schedule setting is "daily" and the newest backup is older than 24h
// (or absent); otherwise take one, notify on failure, and prune. Split from
// StartBackups so tests can drive a single tick directly.
func (s *server) runBackupSchedule(ctx context.Context) {
	if v, err := s.st.GetSetting("backup_schedule"); err != nil || v != "daily" {
		return
	}
	rows, err := s.st.ListBackups()
	if err != nil {
		log.Printf("backup schedule: %v", err)
		return
	}
	if len(rows) > 0 {
		if last, err := time.Parse("2006-01-02 15:04:05", rows[0].CreatedAt); err == nil &&
			s.nowFn().UTC().Sub(last) < 24*time.Hour {
			return
		}
	}
	if _, warnings, err := s.createBackup(ctx, true); err != nil {
		log.Printf("scheduled backup: %v", err)
		s.notify(notifyEvent{Event: "backup_failed", Project: "system", App: "backup", Err: err.Error()})
	} else if len(warnings) > 0 {
		log.Printf("scheduled backup warnings: %v", warnings)
	}
	if _, err := s.pruneBackups(ctx); err != nil {
		log.Printf("scheduled prune: %v", err)
	}
}

// StartBackups runs the daily backup loop: every hour, delegate to
// runBackupSchedule. Mirrors the cert-manager loop's lifecycle.
func (s *server) StartBackups(ctx context.Context) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.runBackupSchedule(ctx)
		}
	}
}
