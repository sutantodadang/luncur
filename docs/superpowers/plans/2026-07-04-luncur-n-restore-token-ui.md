# luncur Plan N — Restore Command + Token UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `luncur restore <source> --data-dir <path> [--force]` automates the DB/key half of disaster recovery (local archive or S3 key source), and `/ui/tokens` lets every user manage their own tokens from the browser.

**Architecture:** Restore is a host command like `luncur up` — never through the API (a running server can't overwrite its own open SQLite). A testable core (`restoreArchive`) validates the tar.gz manifest, runs a bootstrap guard against a non-empty existing DB, takes a pre-restore copy under `--force`, extracts `luncur.db`/`luncur.key`, and returns the `addons/*` member names so the command can print the guided pg_restore/redis steps and the kubectl scale reminder. S3 sources ride a new `s3.Client.Get` (the Plan K client is Put/Delete-only). The token UI mirrors `luncur token list/revoke` over the existing store methods; the session token appears as `session` and revoking it logs the browser out (session row deleted → next request redirects to login).

**Tech Stack:** Go stdlib only (`archive/tar`, `compress/gzip`); existing `internal/s3`, `internal/store`, cobra, html/template.

**Branch:** `plan-n` off `main`.

## Global Constraints (from Phase 4 spec)

- Single Go module, one binary from `cmd/luncur`. **No new Go module dependencies.**
- Restore runs on the host, NOT through the API. Error handling per spec: non-empty DB without `--force` → refuse with a clear message; corrupt/missing manifest → abort before touching the data dir; S3 download failure → abort. Pre-restore backup copy is taken before any overwrite.
- Restore automates the DB/key half only; addon-data restore and cluster scale-down/up stay guided (printed one-liners).
- Tests must not require a cluster, network, or S3: restore operates on temp dirs; the S3 source is an httptest fake.
- `/ui/tokens` is a `uiPage` (any user); per-row revoke buttons carry CSRF; nav gains a "tokens" link for everyone.
- Conventional commits. Before **every** commit: `go build ./... && go vet ./... && go test ./...` — all green.

## Archive format (from Plan K's createBackup — internal/server/backup.go)

tar.gz members: `luncur.db` (VACUUM INTO snapshot, always), `luncur.key` (sealer key, when configured), `addons/<project>-<name>.pgdump` / `addons/<project>-<name>.rdb` (one per addon), `manifest.json` (`{"created_at": RFC3339, "warnings": [...], "members": [...]}`, written last).

---

### Task 1: `s3.Client.Get`

**Files:**
- Modify: `internal/s3/s3.go`
- Test: `internal/s3/s3_test.go`

**Interfaces:**
- Consumes: existing `Client` fields (`Endpoint, Region, Bucket, AccessKey, SecretKey, HTTPClient, Now`), `c.url(key)`, `sign(req, ...)`, `c.region()`, `c.httpClient()`, `c.now()`.
- Produces: `func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error)` — caller closes the body. Task 2's S3 source path uses it.

- [ ] **Step 1: Create branch**

```bash
git checkout -b plan-n
```

- [ ] **Step 2: Write the failing test**

Append to `internal/s3/s3_test.go`:

```go
func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/b/backups/x.tar.gz" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("request not signed")
		}
		w.Write([]byte("archive-bytes"))
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}

	body, err := c.Get(context.Background(), "backups/x.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "archive-bytes" {
		t.Fatalf("got %q", b)
	}
}

func TestGetSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s"}
	if _, err := c.Get(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
}
```

(`io` may need adding to the test file's imports; `httptest`, `http`, `context`, `strings` are already there.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/s3/ -run TestGet -v`
Expected: FAIL — `c.Get undefined`.

- [ ] **Step 4: Implement**

Append to `internal/s3/s3.go` (after `Delete`):

```go
// Get downloads one object. The caller must close the returned body. Error
// responses (>=300) are drained and surfaced like send's.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(key), nil)
	if err != nil {
		return nil, err
	}
	sign(req, c.AccessKey, c.SecretKey, c.region(), c.now().UTC())
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 GET %s: %d %s", key, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/s3/ -v`
Expected: PASS — both new tests plus the existing SigV4/Put/Delete tests.

- [ ] **Step 6: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/s3/
git commit -m "feat: s3 Get for backup downloads"
```

---

### Task 2: restore core + `luncur restore` command

**Files:**
- Create: `internal/cli/restore.go`
- Modify: `internal/cli/root.go` (register `restoreCmd()`)
- Test: `internal/cli/restore_test.go`

**Interfaces:**
- Consumes: `s3.Client.Get` (Task 1); `store.Open(path) (*store.Store, error)`, `Store.ListProjects() ([]Project, error)`, `Store.Close()`; stdlib tar/gzip.
- Produces: `restoreArchive(archivePath, dataDir string, force bool, now func() time.Time) (addonMembers []string, err error)` (testable core) and the `luncur restore` cobra command with flags `--data-dir` (default `./data`), `--force`, `--s3-endpoint`, `--s3-bucket`, `--s3-access-key`, `--s3-secret-key`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/restore_test.go`:

```go
package cli

import (
	"archive/tar"
	"bytes"
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
	if !strings.Contains(out, "pg_restore") || !strings.Contains(out, "dump.rdb") {
		t.Fatalf("missing guided addon steps:\n%s", out)
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestRestore -v`
Expected: FAIL — `restoreArchive` undefined, `unknown command "restore"`.

- [ ] **Step 3: Implement**

Create `internal/cli/restore.go`:

```go
package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/s3"
	"github.com/sutantodadang/luncur/internal/store"
)

// restoreArchive is the testable core of `luncur restore`: validate the
// archive's manifest, run the bootstrap guard against an existing
// non-empty DB, take a pre-restore copy under force, and extract
// luncur.db (+ luncur.key) into dataDir. Returns the archive's addons/*
// member names for the guided-restore printout. The archive is read fully
// before anything in dataDir is touched.
func restoreArchive(archivePath, dataDir string, force bool, now func() time.Time) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	tr := tar.NewReader(gz)

	var dbBytes, keyBytes, manifestBytes []byte
	var addonMembers []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		switch {
		case hdr.Name == "luncur.db":
			if dbBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case hdr.Name == "luncur.key":
			if keyBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case hdr.Name == "manifest.json":
			if manifestBytes, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		case strings.HasPrefix(hdr.Name, "addons/"):
			addonMembers = append(addonMembers, hdr.Name)
		}
	}

	if manifestBytes == nil {
		return nil, fmt.Errorf("archive has no manifest.json — not a luncur backup")
	}
	var manifest struct {
		CreatedAt string   `json:"created_at"`
		Members   []string `json:"members"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("corrupt manifest.json: %w", err)
	}
	if dbBytes == nil {
		return nil, fmt.Errorf("archive has no luncur.db member")
	}

	// Bootstrap guard: never silently overwrite a live install.
	dbPath := filepath.Join(dataDir, "luncur.db")
	keyPath := filepath.Join(dataDir, "luncur.key")
	if _, err := os.Stat(dbPath); err == nil {
		st, err := store.Open(dbPath)
		if err != nil {
			return nil, fmt.Errorf("open existing %s: %w", dbPath, err)
		}
		projects, err := st.ListProjects()
		st.Close()
		if err != nil {
			return nil, err
		}
		if len(projects) > 0 && !force {
			return nil, fmt.Errorf(
				"%s already has %d project(s); refusing to overwrite — re-run with --force to replace it (a pre-restore copy will be kept)",
				dbPath, len(projects))
		}
		if force {
			preDir := filepath.Join(dataDir, "pre-restore-"+now().UTC().Format("20060102-150405"))
			if err := os.MkdirAll(preDir, 0o700); err != nil {
				return nil, err
			}
			if err := copyFile(dbPath, filepath.Join(preDir, "luncur.db")); err != nil {
				return nil, fmt.Errorf("pre-restore copy: %w", err)
			}
			if _, err := os.Stat(keyPath); err == nil {
				if err := copyFile(keyPath, filepath.Join(preDir, "luncur.key")); err != nil {
					return nil, fmt.Errorf("pre-restore copy: %w", err)
				}
			}
		}
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dbPath, dbBytes, 0o600); err != nil {
		return nil, err
	}
	if keyBytes != nil {
		if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
			return nil, err
		}
	}

	sort.Strings(addonMembers)
	return addonMembers, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// restoreCmd is `luncur restore`: a host command like `luncur up` — the
// server must be scaled down first (a running server holds luncur.db open).
func restoreCmd() *cobra.Command {
	var dataDir string
	var force bool
	var s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey string
	cmd := &cobra.Command{
		Use:   "restore <archive-path-or-s3-key>",
		Short: "Restore luncur.db and luncur.key from a backup archive (host command)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := args[0]

			if s3Endpoint != "" {
				cl := &s3.Client{
					Endpoint: s3Endpoint, Bucket: s3Bucket,
					AccessKey: s3AccessKey, SecretKey: s3SecretKey,
				}
				body, err := cl.Get(context.Background(), src)
				if err != nil {
					return fmt.Errorf("download %s: %w", src, err)
				}
				defer body.Close()
				tmp, err := os.CreateTemp("", "luncur-restore-*.tar.gz")
				if err != nil {
					return err
				}
				defer os.Remove(tmp.Name())
				if _, err := io.Copy(tmp, body); err != nil {
					tmp.Close()
					return fmt.Errorf("download %s: %w", src, err)
				}
				if err := tmp.Close(); err != nil {
					return err
				}
				cmd.Printf("downloaded s3://%s/%s\n", s3Bucket, src)
				src = tmp.Name()
			}

			addons, err := restoreArchive(src, dataDir, force, time.Now)
			if err != nil {
				return err
			}

			cmd.Printf("restored luncur.db and luncur.key into %s\n\n", dataDir)
			cmd.Println("next steps:")
			cmd.Println("  1. start (or restart) the server:")
			cmd.Println("     kubectl -n luncur-system scale deploy/luncur --replicas=0   # if it was running against this data dir")
			cmd.Println("     kubectl -n luncur-system scale deploy/luncur --replicas=1")
			if len(addons) > 0 {
				cmd.Println("  2. re-create each addon (luncur addon create ... with the same names), then restore its data:")
				for _, m := range addons {
					base := strings.TrimPrefix(m, "addons/")
					name := strings.TrimSuffix(strings.TrimSuffix(base, ".pgdump"), ".rdb")
					switch {
					case strings.HasSuffix(m, ".pgdump"):
						cmd.Printf("     # %s (postgres)\n", name)
						cmd.Printf("     kubectl -n <project-ns> exec -i addon-<name>-0 -- sh -c 'PGPASSWORD=\"$POSTGRES_PASSWORD\" pg_restore -U \"$POSTGRES_USER\" -d \"$POSTGRES_DB\" --clean' < %s\n", base)
					case strings.HasSuffix(m, ".rdb"):
						cmd.Printf("     # %s (redis)\n", name)
						cmd.Printf("     # scale the addon StatefulSet to 0, copy %s onto its PVC as dump.rdb, scale back to 1\n", base)
					}
				}
				cmd.Println("     (extract the addon dump files from the archive: tar -xzf <archive> addons/)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "luncur data directory to restore into")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite a non-empty existing install (keeps a pre-restore copy)")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", "", "treat <source> as an S3 key and download it from this endpoint")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket (with --s3-endpoint)")
	cmd.Flags().StringVar(&s3AccessKey, "s3-access-key", "", "S3 access key (with --s3-endpoint)")
	cmd.Flags().StringVar(&s3SecretKey, "s3-secret-key", "", "S3 secret key (with --s3-endpoint)")
	return cmd
}
```

In `internal/cli/root.go`, add to the AddCommand block (beside `upCmd()` — check the exact list in the file):

```go
	root.AddCommand(restoreCmd())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRestore -v`
Expected: PASS — all five restore tests.

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/cli/restore.go internal/cli/restore_test.go internal/cli/root.go
git commit -m "feat: luncur restore — host-side DB/key restore with bootstrap guard"
```

---

### Task 3: `/ui/tokens` page

**Files:**
- Modify: `internal/server/ui.go` (routes + two handlers)
- Create: `internal/server/templates/tokens.html`
- Modify: `internal/server/templates/base.html` (nav link)
- Test: `internal/server/ui_test.go`

**Interfaces:**
- Consumes: `s.st.ListTokens(u.ID) ([]store.TokenInfo, error)` (`TokenInfo{ID, Name, CreatedAt, LastUsedAt, ExpiresAt}`), `s.st.RevokeToken(u.ID, id)`, UI helpers `s.uiPage`, `s.csrf`, `s.renderPage`; test helpers `uiSessionCookie`, `uiCSRF`, `uiPost`, `noRedirectClient`, `seedUserToken`.
- Produces: `GET /ui/tokens` (any user), `POST /ui/tokens/revoke` (form field `id`), nav "tokens" link on every page.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/ui_test.go`:

```go
// TestUITokensPage: any user sees their own tokens (the web session shows
// as "session"), revoking an API token removes it, and revoking the
// session token logs the browser out.
func TestUITokensPage(t *testing.T) {
	srv, st := testServer(t)
	apiTok := seedUserToken(t, st, "u@b.co", "member")
	_ = apiTok

	u, err := st.GetUserByEmail("u@b.co")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	ck := uiSessionCookie(t, st, u.ID)

	tokensPage := func(t *testing.T) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/tokens", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(ck)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}

	code, body := tokensPage(t)
	if code != http.StatusOK {
		t.Fatalf("tokens page: want 200, got %d", code)
	}
	// Both the seeded API token (named "test" by seedUserToken's helper
	// chain) and the web session ("session") are listed.
	if !strings.Contains(body, "session") {
		t.Fatalf("tokens page missing session token:\n%s", body)
	}
	if !strings.Contains(body, `action="/ui/tokens/revoke"`) {
		t.Fatalf("tokens page missing revoke form:\n%s", body)
	}
	// Nav link present.
	if !strings.Contains(body, `href="/ui/tokens"`) {
		t.Fatalf("nav missing tokens link:\n%s", body)
	}

	// Revoke the API token (find its id: list from the store).
	tokens, err := st.ListTokens(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var apiID, sessionID int64
	for _, tk := range tokens {
		if tk.Name == "session" {
			sessionID = tk.ID
		} else {
			apiID = tk.ID
		}
	}
	if apiID == 0 || sessionID == 0 {
		t.Fatalf("tokens = %+v", tokens)
	}

	csrfCk := uiCSRF(t, client, srv.URL)
	resp := uiPost(t, client, srv.URL+"/ui/tokens/revoke", csrfCk, ck,
		url.Values{"id": {strconv.FormatInt(apiID, 10)}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke: want 303, got %d", resp.StatusCode)
	}
	if left, _ := st.ListTokens(u.ID); len(left) != 1 {
		t.Fatalf("tokens after revoke = %+v", left)
	}

	// Revoking the session token logs the browser out: the next page load
	// redirects to /ui/login.
	resp = uiPost(t, client, srv.URL+"/ui/tokens/revoke", csrfCk, ck,
		url.Values{"id": {strconv.FormatInt(sessionID, 10)}})
	resp.Body.Close()
	code, _ = tokensPage(t)
	if code != http.StatusSeeOther {
		t.Fatalf("after session revoke: want 303 to login, got %d", code)
	}
}
```

(`strconv` may need adding to ui_test.go's imports.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestUITokensPage -v`
Expected: FAIL — `GET /ui/tokens` 404.

- [ ] **Step 3: Implement**

1. `internal/server/ui.go`, in `uiRoutes` after the users routes:

```go
	mux.HandleFunc("GET /ui/tokens", s.uiPage(s.handleUITokens))
	mux.HandleFunc("POST /ui/tokens/revoke", s.uiPage(s.handleUITokenRevoke))
```

2. Handlers (near handleUIUsers):

```go
// handleUITokens lists the caller's own tokens — the UI twin of
// GET /v1/tokens. The web session rides the same table (name "session").
func (s *server) handleUITokens(w http.ResponseWriter, r *http.Request, u store.User) {
	tokens, err := s.st.ListTokens(u.ID)
	if err != nil {
		log.Printf("ui tokens: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.renderPage(w, "tokens.html", map[string]any{
		"User": u, "Tokens": tokens,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
	})
}

// handleUITokenRevoke revokes one of the caller's tokens. Revoking the
// current session's token logs the browser out on the next request.
func (s *server) handleUITokenRevoke(w http.ResponseWriter, r *http.Request, u store.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	if err := s.st.RevokeToken(u.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui revoke token: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/ui/tokens", http.StatusSeeOther)
}
```

3. Create `internal/server/templates/tokens.html`:

```html
{{define "tokens.html"}}
{{template "head" .}}{{template "nav" .}}
<h1>API tokens</h1>
<p>Tokens authenticate the CLI and API. Your current browser session is the row named <code>session</code> — revoking it logs you out.</p>
<table><tr><th>Name</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr>
{{range .Tokens}}<tr>
  <td><code>{{.Name}}</code></td>
  <td>{{.CreatedAt}}</td>
  <td>{{if .LastUsedAt}}{{.LastUsedAt}}{{else}}never{{end}}</td>
  <td>{{.ExpiresAt}}</td>
  <td>
    <form class="inline" method="post" action="/ui/tokens/revoke">
      <input type="hidden" name="_csrf" value="{{$.CSRF}}">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit">revoke</button>
    </form>
  </td>
</tr>{{end}}
</table>
{{template "foot" .}}
{{end}}
```

4. `internal/server/templates/base.html`, nav block — add the tokens link before the users link:

```html
{{define "nav"}}
<nav>
  <strong><a href="/ui/">luncur</a></strong>
  <a href="/ui/tokens">tokens</a>
  {{if .IsAdmin}}<a href="/ui/users">users</a>{{end}}
  <form class="inline" method="post" action="/ui/logout">
    <input type="hidden" name="_csrf" value="{{.CSRF}}">
    <span>{{.User.Email}}</span> <button type="submit">log out</button>
  </form>
</nav>
{{end}}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestUITokensPage|TestUI' -v`
Expected: PASS — the new test and every existing UI test (the nav change touches all pages; none assert the absence of a tokens link).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/ui.go internal/server/templates/tokens.html internal/server/templates/base.html internal/server/ui_test.go
git commit -m "feat: /ui/tokens — self-service token list + revoke"
```

---

### Task 4: docs

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README**

1. "Restore runbook" section (~line 340): retitle to **"Restoring"**. Replace the "Restore is deliberately a documented procedure, not a command" framing and steps 2–4 with the command:

```sh
# on the target host, with the server scaled down
luncur restore /path/to/luncur-YYYYMMDD-HHMMSS.tar.gz --data-dir <data-dir> [--force]

# or straight from the backup bucket
luncur restore <prefix>/luncur-....tar.gz --s3-endpoint https://<s3> \
  --s3-bucket my-backups --s3-access-key ... --s3-secret-key ... --data-dir <data-dir>
```

Keep (as prose): the PVC-location tip (`kubectl get pvc luncur-data ...`), the scale-down/up steps, and the addon-data steps 6–7 — note that `luncur restore` prints these same guided commands. Document the bootstrap guard: a data dir whose DB already has projects is refused without `--force`; `--force` keeps a `pre-restore-<timestamp>/` copy of the old `luncur.db`/`luncur.key`.

2. API tokens section (~line 62): mention the web UI equivalent — every user has a `/ui/tokens` page (nav → "tokens") listing their tokens with revoke buttons; the browser session is the `session` row and revoking it logs out.

- [ ] **Step 2: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add README.md
git commit -m "docs: luncur restore command and token UI"
```

---

## Manual verification (owner's VPS, after merge)

Per the Phase 4 test strategy: take a real backup, provision a fresh box (`luncur up`), scale the server down, `luncur restore` the archive (local and S3 paths), scale up, log in with restored credentials. In the browser: open /ui/tokens, revoke a CLI token, revoke the session and confirm logout.

## Self-review notes

- Spec coverage: host command + local/S3 source (Tasks 1–2), manifest validation + bootstrap guard + `--force` pre-restore copy (Task 2, all three error-table rows tested), guided addon printout + kubectl reminder (Task 2 command + S3 test asserts), `/ui/tokens` with session-as-`session` + logout-on-self-revoke + nav link (Task 3), docs (Task 4).
- Type consistency: `restoreArchive(archivePath, dataDir string, force bool, now func() time.Time) ([]string, error)` used by both the command and tests; `s3.Client.Get(ctx, key) (io.ReadCloser, error)` matches the command's download call; `TokenInfo` fields in tokens.html match `internal/store/tokens.go`.
- seedUserToken's API-token name: the test finds the non-`session` row by exclusion, so the exact name doesn't matter.
