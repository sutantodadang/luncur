package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// fakeExecer serves canned bytes for addon dump commands.
type fakeExecer struct {
	out string
	err error
}

func (f *fakeExecer) ExecPod(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	if f.err != nil {
		return f.err
	}
	joined := strings.Join(cmd, " ")
	if strings.Contains(joined, "pg_dump") || strings.Contains(joined, "redis-cli") {
		fmt.Fprint(stdout, f.out)
		return nil
	}
	return fmt.Errorf("unexpected command %q", joined)
}

// backupServer builds a server with a store, sealer, data dir, secret key
// file, and a fake execer — no kube needed for the archive paths.
func backupServer(t *testing.T, exec *fakeExecer) (*server, *store.Store, string) {
	t.Helper()
	st := newTestStore(t)
	dataDir := t.TempDir()
	keyPath := filepath.Join(t.TempDir(), "luncur.key")
	if err := os.WriteFile(keyPath, []byte("KEYBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{Store: st, Sealer: sealer, DataDir: dataDir, SecretKeyPath: keyPath})
	if exec != nil {
		srv.execer = exec
	}
	return srv, st, dataDir
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = b
	}
	return out
}

func seedBackupAddon(t *testing.T, srv *server, st *store.Store) {
	t.Helper()
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := newAddonCreds("postgres")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := srv.sealCreds(creds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAddon(p.ID, "postgres", "db1", "16", 1, sealed); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBackupArchive(t *testing.T) {
	srv, st, _ := backupServer(t, &fakeExecer{out: "PGDUMPDATA"})
	seedBackupAddon(t, srv, st)

	b, warnings, err := srv.createBackup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none (S3 unset is not a warning)", warnings)
	}
	if b.Uploaded {
		t.Fatal("uploaded without S3 config")
	}
	members := readArchive(t, b.Path)
	if len(members["luncur.db"]) == 0 {
		t.Fatal("luncur.db missing or empty")
	}
	if string(members["luncur.key"]) != "KEYBYTES" {
		t.Fatalf("luncur.key = %q", members["luncur.key"])
	}
	if string(members["addons/proj-db1.pgdump"]) != "PGDUMPDATA" {
		t.Fatalf("addon dump = %q", members["addons/proj-db1.pgdump"])
	}
	if len(members["manifest.json"]) == 0 {
		t.Fatal("manifest.json missing")
	}
	rows, err := st.ListBackups()
	if err != nil || len(rows) != 1 || rows[0].SizeBytes == 0 {
		t.Fatalf("backup rows = %+v err=%v", rows, err)
	}
}

func TestCreateBackupAddonFailureWarns(t *testing.T) {
	srv, st, _ := backupServer(t, &fakeExecer{err: fmt.Errorf("pod gone")})
	seedBackupAddon(t, srv, st)

	b, warnings, err := srv.createBackup(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "db1") {
		t.Fatalf("warnings = %v", warnings)
	}
	members := readArchive(t, b.Path)
	if _, ok := members["addons/proj-db1.pgdump"]; ok {
		t.Fatal("failed dump member present")
	}
	if len(members["luncur.db"]) == 0 {
		t.Fatal("db member missing despite addon failure")
	}
}

func TestBackupUploadAndPrune(t *testing.T) {
	var puts, deletes []string
	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			puts = append(puts, r.URL.Path)
		case http.MethodDelete:
			deletes = append(deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3srv.Close()

	srv, st, _ := backupServer(t, nil)
	for k, v := range map[string]string{
		"backup_s3_endpoint":   s3srv.URL,
		"backup_s3_bucket":     "bk",
		"backup_s3_access_key": "ak",
		"backup_keep":          "2",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	sealed, err := srv.sealer.Seal([]byte("sk"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("backup_s3_secret_key", "sealed:"+fmt.Sprintf("%x", sealed)); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		b, _, err := srv.createBackup(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		if !b.Uploaded {
			t.Fatalf("backup %d not uploaded", i)
		}
	}
	if len(puts) != 3 {
		t.Fatalf("puts = %v", puts)
	}

	removed, err := srv.pruneBackups(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("removed = %d err=%v", removed, err)
	}
	rows, _ := st.ListBackups()
	if len(rows) != 2 {
		t.Fatalf("rows after prune = %+v", rows)
	}
	if len(deletes) != 1 {
		t.Fatalf("remote deletes = %v", deletes)
	}
	// Oldest local file gone.
	for _, r := range rows {
		if _, err := os.Stat(r.Path); err != nil {
			t.Fatalf("kept backup missing on disk: %v", err)
		}
	}
}

func TestBackupAPI(t *testing.T) {
	srv, st, _ := backupServer(t, nil)
	h := srv.handler()
	ts := httptest.NewServer(h)
	defer ts.Close()
	adminTok := seedUserToken(t, st, "bk-admin@b.co", "admin")
	memberTok := seedUserToken(t, st, "bk-member@b.co", "member")

	forbidden := doAuthed(t, "POST", ts.URL+"/v1/backups", memberTok, `{"no_upload":true}`)
	forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member create: want 403, got %d", forbidden.StatusCode)
	}

	created := doAuthed(t, "POST", ts.URL+"/v1/backups", adminTok, `{"no_upload":true}`)
	defer created.Body.Close()
	if created.StatusCode != 201 {
		t.Fatalf("create: want 201, got %d", created.StatusCode)
	}
	var out struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(created.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Path == "" {
		t.Fatalf("no path: %+v", out)
	}

	list := doAuthed(t, "GET", ts.URL+"/v1/backups", adminTok, "")
	defer list.Body.Close()
	var rows []map[string]any
	if err := json.NewDecoder(list.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("list = %+v", rows)
	}

	prune := doAuthed(t, "POST", ts.URL+"/v1/backups/prune", adminTok, "")
	defer prune.Body.Close()
	if prune.StatusCode != 200 {
		t.Fatalf("prune: want 200, got %d", prune.StatusCode)
	}
}

// TestScheduledBackupFailureNotifies drives runBackupSchedule directly (the
// tick body StartBackups calls hourly): with backup_schedule=daily and no
// data dir configured, createBackup fails immediately and that failure must
// notify backup_failed exactly once.
func TestScheduledBackupFailureNotifies(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer}) // no DataDir -> s.src nil -> createBackup fails
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("backup_schedule", "daily"); err != nil {
		t.Fatal(err)
	}

	s.runBackupSchedule(context.Background())

	b := recvNotify(t, ch, 2*time.Second)
	if !strings.Contains(string(b), `"event":"backup_failed"`) {
		t.Fatalf("body = %s, want backup_failed", b)
	}
}
