package cli

import (
	"archive/tar"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// makeArchive builds a backup-shaped tar.gz on disk from member -> bytes.
func makeArchive(t *testing.T, members map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, b := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// dbBytes builds a real SQLite store file (optionally with one project)
// and returns its raw bytes.
func dbBytes(t *testing.T, withProject bool) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.db")
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if withProject {
		if _, err := st.CreateProject("restored"); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validArchive(t *testing.T) string {
	t.Helper()
	return makeArchive(t, map[string][]byte{
		"luncur.db":              dbBytes(t, true),
		"luncur.key":             []byte("keybytes-32-keybytes-32-keybyte!"),
		"addons/proj-pg1.pgdump": []byte("pgdump"),
		"addons/proj-red1.rdb":   []byte("rdb"),
		"manifest.json":          []byte(`{"created_at":"2026-07-04T00:00:00Z","members":["luncur.db"]}`),
	})
}

func TestRestoreFreshDir(t *testing.T) {
	dataDir := t.TempDir()
	addons, err := restoreArchive(validArchive(t), dataDir, false, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if len(addons) != 2 {
		t.Fatalf("addon members = %v, want 2", addons)
	}

	// The restored DB opens and contains the archived project.
	st, err := store.Open(filepath.Join(dataDir, "luncur.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	projects, err := st.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Name != "restored" {
		t.Fatalf("projects = %+v", projects)
	}

	key, err := os.ReadFile(filepath.Join(dataDir, "luncur.key"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(key), "keybytes") {
		t.Fatalf("key = %q", key)
	}
}

func TestRestoreGuardAndForce(t *testing.T) {
	dataDir := t.TempDir()

	// Existing non-empty install in the data dir.
	st, err := store.Open(filepath.Join(dataDir, "luncur.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateProject("existing"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.WriteFile(filepath.Join(dataDir, "luncur.key"), []byte("oldkey"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without --force: refused, data untouched.
	if _, err := restoreArchive(validArchive(t), dataDir, false, time.Now); err == nil ||
		!strings.Contains(err.Error(), "--force") {
		t.Fatalf("guard: want --force refusal, got %v", err)
	}

	// With --force: pre-restore copy exists, DB replaced.
	if _, err := restoreArchive(validArchive(t), dataDir, true, time.Now); err != nil {
		t.Fatal(err)
	}
	pre, err := filepath.Glob(filepath.Join(dataDir, "pre-restore-*", "luncur.db"))
	if err != nil || len(pre) != 1 {
		t.Fatalf("pre-restore db copy: %v %v", pre, err)
	}
	preKey, err := filepath.Glob(filepath.Join(dataDir, "pre-restore-*", "luncur.key"))
	if err != nil || len(preKey) != 1 {
		t.Fatalf("pre-restore key copy: %v %v", preKey, err)
	}

	st2, err := store.Open(filepath.Join(dataDir, "luncur.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	projects, err := st2.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Name != "restored" {
		t.Fatalf("projects after force restore = %+v", projects)
	}
}

func TestRestoreRejectsBadManifest(t *testing.T) {
	dataDir := t.TempDir()

	// No manifest at all.
	noManifest := makeArchive(t, map[string][]byte{"luncur.db": dbBytes(t, false)})
	if _, err := restoreArchive(noManifest, dataDir, false, time.Now); err == nil ||
		!strings.Contains(err.Error(), "manifest") {
		t.Fatalf("missing manifest: %v", err)
	}

	// Corrupt manifest.
	bad := makeArchive(t, map[string][]byte{
		"luncur.db":     dbBytes(t, false),
		"manifest.json": []byte("{nope"),
	})
	if _, err := restoreArchive(bad, dataDir, false, time.Now); err == nil ||
		!strings.Contains(err.Error(), "manifest") {
		t.Fatalf("corrupt manifest: %v", err)
	}

	// Data dir untouched in both cases.
	if _, err := os.Stat(filepath.Join(dataDir, "luncur.db")); !os.IsNotExist(err) {
		t.Fatalf("data dir touched on manifest failure: %v", err)
	}
}

func TestRestoreCommandS3Source(t *testing.T) {
	archive, err := os.ReadFile(validArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bkt/backups/luncur.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	defer fake.Close()

	dataDir := t.TempDir()
	out, err := run(t, "restore", "backups/luncur.tar.gz",
		"--data-dir", dataDir,
		"--s3-endpoint", fake.URL, "--s3-bucket", "bkt",
		"--s3-access-key", "k", "--s3-secret-key", "s")
	if err != nil {
		t.Fatalf("restore: %v (%s)", err, out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "luncur.db")); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}
	if !strings.Contains(out, "luncur addon restore proj-pg1") || !strings.Contains(out, "luncur addon restore proj-red1") {
		t.Fatalf("missing guided addon restore commands:\n%s", out)
	}
	if !strings.Contains(out, "kubectl -n luncur-system scale deploy/luncur") {
		t.Fatalf("missing scale reminder:\n%s", out)
	}

	// Bad key -> abort, nothing written.
	dataDir2 := t.TempDir()
	if _, err := run(t, "restore", "backups/nope.tar.gz",
		"--data-dir", dataDir2,
		"--s3-endpoint", fake.URL, "--s3-bucket", "bkt",
		"--s3-access-key", "k", "--s3-secret-key", "s"); err == nil {
		t.Fatal("want download error")
	}
	if _, err := os.Stat(filepath.Join(dataDir2, "luncur.db")); !os.IsNotExist(err) {
		t.Fatal("data dir touched after failed download")
	}
}

func TestRestoreCommandLocalSource(t *testing.T) {
	dataDir := t.TempDir()
	out, err := run(t, "restore", validArchive(t), "--data-dir", dataDir)
	if err != nil {
		t.Fatalf("restore: %v (%s)", err, out)
	}
	if !strings.Contains(out, "restored luncur.db") {
		t.Fatalf("summary missing:\n%s", out)
	}
}
