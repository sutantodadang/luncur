# luncur Plan E — git push receiver (SSH) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `git push ssh://git@<ip>:30022/<project>/<app>.git main` deploys the app, streaming build progress back to the git client.

**Architecture:** An SSH server (`x/crypto/ssh`) inside `luncur serve` accepts only `git-receive-pack`. Each push lands in a throwaway bare repo whose `post-receive` hook (a tiny hidden CLI command in the same binary) relays ref info and build progress over a unix socket to the server, which archives the pushed branch into the existing tarball → Build Job pipeline and tails the build log back to the client. Public-key auth maps SSH key fingerprints to luncur users via a new `ssh_keys` table.

**Tech Stack:** Go stdlib, `golang.org/x/crypto/ssh` (already in go.mod), `git` binary (added to the server image; already present on dev machines and CI), cobra, modernc.org/sqlite.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. New import allowed: `golang.org/x/crypto/ssh` (go.mod already requires `golang.org/x/crypto v0.53.0` — no go.mod change needed). **No go-git.** Git operations shell out to `git`.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope `{"error":{"code":"...","message":"..."}}` via `writeError`.
- All commits conventional style; run `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster or root. SSH tested with an in-process `x/crypto/ssh` client; git plumbing tested with the real `git` binary (available on the dev machine and CI). Guard git-dependent tests with `exec.LookPath("git")` skip.
- **Approved deviations from the Phase 2 spec (record in README):**
  - SSH host key persists as a file on the data PVC (`--ssh-hostkey-file`, default beside `--db`), not a K8s Secret — same durability (PVC survives pod restarts), no kube dependency at SSH boot, mirrors the existing `luncur.key` sealer pattern.
  - Progress streaming uses a `post-receive` hook, so the git exit status cannot reflect build failure (refs land before the hook runs). The client still sees the full build log and a final `BUILD FAILED`/`app live` line as `remote:` output. `pre-receive` (which could reject) runs inside git's object quarantine, which breaks out-of-process `git archive`; not worth the complexity.

---

### Task 1: store — `ssh_keys` table + CRUD

**Files:**
- Modify: `internal/store/schema.sql`
- Create: `internal/store/sshkeys.go`
- Test: `internal/store/sshkeys_test.go`

**Interfaces:**
- Consumes: existing `Store` (`s.db`), `ErrNotFound`, `ErrAuthFailed` sentinels (both exist in the store package), `User` struct (`ID, Email, Role`).
- Produces:
  - `type SSHKey struct { ID, UserID int64; Name, PublicKey, Fingerprint, CreatedAt string }`
  - `Store.AddSSHKey(userID int64, name, publicKey string) (SSHKey, error)` — parses/validates the authorized_keys-format public key, computes the SHA256 fingerprint, rejects duplicates.
  - `Store.ListSSHKeys(userID int64) ([]SSHKey, error)`
  - `Store.DeleteSSHKey(userID, id int64) error` — `ErrNotFound` when the row doesn't exist or belongs to another user.
  - `Store.UserForSSHFingerprint(fp string) (User, error)` — `ErrAuthFailed` when unknown.

- [ ] **Step 1: Write the failing test** (`internal/store/sshkeys_test.go`; reuse the package's `openTest(t)` helper from `store_test.go`):

```go
package store

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testPubKey generates a fresh ed25519 key in authorized_keys format.
func testPubKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestSSHKeyRoundTrip(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("dev@example.com", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	pub := testPubKey(t)

	k, err := s.AddSSHKey(u.ID, "laptop", pub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Fingerprint, "SHA256:") {
		t.Fatalf("fingerprint = %q, want SHA256:...", k.Fingerprint)
	}

	got, err := s.UserForSSHFingerprint(k.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID {
		t.Fatalf("user = %d, want %d", got.ID, u.ID)
	}

	list, err := s.ListSSHKeys(u.ID)
	if err != nil || len(list) != 1 || list[0].Name != "laptop" {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// Duplicate key rejected.
	if _, err := s.AddSSHKey(u.ID, "again", pub); err == nil {
		t.Fatal("duplicate public key accepted")
	}
	// Garbage rejected.
	if _, err := s.AddSSHKey(u.ID, "bad", "not a key"); err == nil {
		t.Fatal("garbage public key accepted")
	}

	if err := s.DeleteSSHKey(u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForSSHFingerprint(k.Fingerprint); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("deleted key: got %v, want ErrAuthFailed", err)
	}
	// Deleting someone else's key id → ErrNotFound.
	if err := s.DeleteSSHKey(u.ID+1, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: got %v, want ErrNotFound", err)
	}
}
```

`ed25519GenerateKey` is a two-line helper in the test file:

```go
import "crypto/ed25519"
import "crypto/rand"

func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
```

(Fold the imports into the file's import block; shown separately here for clarity.)

- [ ] **Step 2: Run** `go test ./internal/store/ -run TestSSHKeyRoundTrip -v` — compile failure (`AddSSHKey` undefined).

- [ ] **Step 3: Implement.**

`internal/store/schema.sql` — append:

```sql
CREATE TABLE IF NOT EXISTS ssh_keys (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  public_key  TEXT NOT NULL,
  fingerprint TEXT NOT NULL UNIQUE,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

(`CREATE TABLE IF NOT EXISTS` means existing DBs pick the table up on next `Open` — no `migrate()` ALTER needed.)

`internal/store/sshkeys.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHKey is a user's public key for git-push auth.
type SSHKey struct {
	ID          int64
	UserID      int64
	Name        string
	PublicKey   string
	Fingerprint string
	CreatedAt   string
}

// AddSSHKey validates an authorized_keys-format public key and stores it
// with its SHA256 fingerprint. The fingerprint is the auth lookup key, so
// duplicates are rejected by the UNIQUE constraint.
func (s *Store) AddSSHKey(userID int64, name, publicKey string) (SSHKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil {
		return SSHKey{}, fmt.Errorf("invalid public key: %w", err)
	}
	fp := ssh.FingerprintSHA256(pk)
	norm := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	res, err := s.db.Exec(
		`INSERT INTO ssh_keys (user_id, name, public_key, fingerprint) VALUES (?, ?, ?, ?)`,
		userID, name, norm, fp,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return SSHKey{}, fmt.Errorf("this public key is already registered")
		}
		return SSHKey{}, err
	}
	id, _ := res.LastInsertId()
	return SSHKey{ID: id, UserID: userID, Name: name, PublicKey: norm, Fingerprint: fp}, nil
}

func (s *Store) ListSSHKeys(userID int64) ([]SSHKey, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, public_key, fingerprint, created_at
		 FROM ssh_keys WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var k SSHKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.PublicKey, &k.Fingerprint, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteSSHKey removes one of userID's keys. ErrNotFound covers both a
// missing id and an id owned by someone else, so the API can't leak key
// existence across users.
func (s *Store) DeleteSSHKey(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM ssh_keys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UserForSSHFingerprint resolves a public-key fingerprint to its user.
func (s *Store) UserForSSHFingerprint(fp string) (User, error) {
	var u User
	err := s.db.QueryRow(
		`SELECT u.id, u.email, u.role FROM ssh_keys k
		 JOIN users u ON u.id = k.user_id WHERE k.fingerprint = ?`, fp,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	return u, nil
}
```

(Before writing, open `internal/store/users.go` or `tokens.go` to confirm the exact names of `ErrNotFound`/`ErrAuthFailed` and the `User` fields; adjust if they differ.)

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: ssh_keys store — add/list/delete + fingerprint auth lookup`

---

### Task 2: API — `/v1/ssh-keys` endpoints

**Files:**
- Create: `internal/server/sshkeys.go`
- Modify: `internal/server/server.go` (routes)
- Test: `internal/server/sshkeys_test.go`

**Interfaces:**
- Consumes: Task 1 store methods; existing `s.authed`, `writeJSON`, `writeError` helpers.
- Produces:
  - `POST /v1/ssh-keys` body `{"name":"...","public_key":"ssh-ed25519 AAAA..."}` → 201 `{"id":1,"name":"...","fingerprint":"SHA256:..."}`; 400 `bad_request` on invalid key.
  - `GET /v1/ssh-keys` → 200 `[{"id":,"name":,"fingerprint":,"created_at":}]` (own keys only).
  - `DELETE /v1/ssh-keys/{id}` → 204; 404 `not_found`.

- [ ] **Step 1: Write the failing test** (`internal/server/sshkeys_test.go`; mirror the arrange pattern in `users_test.go` — build `Deps` with a temp store, create a user + token, use its `doAuthed` helper if exported within the package):

```go
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestSSHKeyEndpoints(t *testing.T) {
	// Arrange exactly like users_test.go: temp store, New(Deps{Store: st}),
	// httptest server, create user + token.
	// pub := a valid test public key — generate like Task 1's testPubKey
	// (ed25519 + ssh.MarshalAuthorizedKey).

	// 1. POST /v1/ssh-keys with {"name":"laptop","public_key":pub} → 201,
	//    response contains "SHA256:".
	// 2. GET /v1/ssh-keys → 200, exactly one entry, name "laptop",
	//    response does NOT contain the raw public key? (it may; assert name+fingerprint present).
	// 3. DELETE /v1/ssh-keys/{id} → 204.
	// 4. GET → empty list.
	// 5. POST with public_key "garbage" → 400, code bad_request.
	// Write these as real assertions with the package's http helpers.
	_ = json.Marshal
	_ = fmt.Sprintf
	_ = strings.Contains
	_ = http.StatusCreated
}
```

Replace the placeholder body with real code following `users_test.go`'s existing style — the file shows how to build the server, mint a token, and make authed requests. The five numbered behaviors above are the required assertions.

- [ ] **Step 2: Run** `go test ./internal/server/ -run TestSSHKeyEndpoints -v` — fails (404 routes missing).

- [ ] **Step 3: Implement.** `internal/server/sshkeys.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleAddSSHKey(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Name == "" {
		req.Name = "key"
	}
	k, err := s.st.AddSSHKey(u.ID, req.Name, req.PublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "name": k.Name, "fingerprint": k.Fingerprint,
	})
}

func (s *server) handleListSSHKeys(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.st.ListSSHKeys(u.ID)
	if err != nil {
		log.Printf("list ssh keys: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, k := range list {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "fingerprint": k.Fingerprint, "created_at": k.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteSSHKey(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid key id")
		return
	}
	if err := s.st.DeleteSSHKey(u.ID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such key")
		return
	} else if err != nil {
		log.Printf("delete ssh key: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Routes in `server.go` `handler()` (after the `/v1/me` line):

```go
	mux.HandleFunc("POST /v1/ssh-keys", s.authed(s.handleAddSSHKey))
	mux.HandleFunc("GET /v1/ssh-keys", s.authed(s.handleListSSHKeys))
	mux.HandleFunc("DELETE /v1/ssh-keys/{id}", s.authed(s.handleDeleteSSHKey))
```

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: /v1/ssh-keys API (add, list, delete)`

---

### Task 3: CLI — `luncur ssh-key add/list/remove`

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/sshkey.go`
- Modify: `internal/cli/root.go` (register)
- Test: `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: Task 2 endpoints; existing `client.Client` request helpers (open `internal/client/client.go` first and reuse its existing `do`/request pattern exactly — do not invent a new one); existing `apiClient()` helper in the cli package.
- Produces:
  - `Client.AddSSHKey(name, publicKey string) (fingerprint string, err error)`
  - `Client.ListSSHKeys() ([]SSHKeyInfo, error)` with `type SSHKeyInfo struct { ID int64 \`json:"id"\`; Name string \`json:"name"\`; Fingerprint string \`json:"fingerprint"\`; CreatedAt string \`json:"created_at"\` }`
  - `Client.DeleteSSHKey(id int64) error`
  - CLI: `luncur ssh-key add [path]` (default: first existing of `~/.ssh/id_ed25519.pub`, `~/.ssh/id_rsa.pub`, `~/.ssh/id_ecdsa.pub`; `--name` defaults to hostname), `ssh-key list`, `ssh-key remove <id>`.

- [ ] **Step 1: Write the failing CLI test** (append to `internal/cli/commands_test.go`, using its `testEnv(t)` + `run(...)` helpers):

```go
func TestSSHKeyCommands(t *testing.T) {
	srv := testEnv(t)
	// login exactly the way the file's other tests do (run login with the
	// seeded admin credentials against srv.URL).

	pubPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	// write a valid test public key to pubPath (generate ed25519 +
	// ssh.MarshalAuthorizedKey as in the store test).

	out := run(t, "ssh-key", "add", pubPath, "--name", "laptop")
	if !strings.Contains(out, "SHA256:") {
		t.Fatalf("add output missing fingerprint: %s", out)
	}
	out = run(t, "ssh-key", "list")
	if !strings.Contains(out, "laptop") {
		t.Fatalf("list missing key: %s", out)
	}
	out = run(t, "ssh-key", "remove", "1")
	_ = out
	out = run(t, "ssh-key", "list")
	if strings.Contains(out, "laptop") {
		t.Fatalf("key not removed: %s", out)
	}
}
```

Adapt the login/run invocation details to what `commands_test.go` actually does (read it first) — the helpers may return errors differently.

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement.**

`internal/client/client.go` — add (matching the file's existing request style; the shapes below assume a `c.do(method, path, body, &out)` style helper — adapt to the real one):

```go
type SSHKeyInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

func (c *Client) AddSSHKey(name, publicKey string) (string, error) {
	var out struct {
		Fingerprint string `json:"fingerprint"`
	}
	err := c.do("POST", "/v1/ssh-keys",
		map[string]string{"name": name, "public_key": publicKey}, &out)
	return out.Fingerprint, err
}

func (c *Client) ListSSHKeys() ([]SSHKeyInfo, error) {
	var out []SSHKeyInfo
	err := c.do("GET", "/v1/ssh-keys", nil, &out)
	return out, err
}

func (c *Client) DeleteSSHKey(id int64) error {
	return c.do("DELETE", fmt.Sprintf("/v1/ssh-keys/%d", id), nil, nil)
}
```

`internal/cli/sshkey.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// defaultPubKeyPath returns the first standard public key found in ~/.ssh.
func defaultPubKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub", "id_ecdsa.pub"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no public key found in ~/.ssh (looked for id_ed25519.pub, id_rsa.pub, id_ecdsa.pub); pass a path explicitly")
}

func sshKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh-key",
		Short: "Manage SSH public keys for git push",
	}

	var name string
	add := &cobra.Command{
		Use:   "add [path-to-public-key]",
		Short: "Register a public key",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			path := ""
			if len(args) == 1 {
				path = args[0]
			} else if path, err = defaultPubKeyPath(); err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if name == "" {
				if h, err := os.Hostname(); err == nil {
					name = h
				} else {
					name = "key"
				}
			}
			fp, err := c.AddSSHKey(name, string(b))
			if err != nil {
				return err
			}
			cmd.Printf("added %s (%s)\n", name, fp)
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "key name (default: hostname)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List registered keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			keys, err := c.ListSSHKeys()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tFINGERPRINT\tADDED")
			for _, k := range keys {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", k.ID, k.Name, k.Fingerprint, k.CreatedAt)
			}
			return tw.Flush()
		},
	}

	remove := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid id %q", args[0])
			}
			return c.DeleteSSHKey(id)
		},
	}

	cmd.AddCommand(add, list, remove)
	return cmd
}
```

Register in `root.go` alongside the other `root.AddCommand(...)` calls: `sshKeyCmd()`.

- [ ] **Step 4: Run** `go test ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: luncur ssh-key add/list/remove`

---

### Task 4: `internal/gitssh` — SSH server + git receive plumbing

**Files:**
- Create: `internal/gitssh/gitssh.go`
- Create: `internal/gitssh/hostkey.go`
- Create: `internal/gitssh/receive.go`
- Test: `internal/gitssh/gitssh_test.go`

**Interfaces:**
- Consumes: `golang.org/x/crypto/ssh`, `os/exec` (`git`), `store.User` (import `internal/store` for the type only).
- Produces:
  - `type Backend interface { Authorize(fingerprint string) (store.User, error); Branch(u store.User, project, app string) (string, error); Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error }`
    - `Authorize`: fingerprint → user (or error → auth denied).
    - `Branch`: validates the user may push to project/app (membership + existence) and returns the branch that triggers a deploy. Error text is shown to the git client.
    - `Push`: consumes the archived source tarball, blocks until the deploy finishes, writes human progress lines to `progress`. Error text is shown to the client after the log.
  - `LoadOrCreateHostKey(path string) (ssh.Signer, error)` — ed25519, PEM (OpenSSH format) on disk, 0600.
  - `type Server struct{ ... }`, `New(hostKey ssh.Signer, backend Backend) *Server`, `(*Server).Serve(l net.Listener) error`, `(*Server).Close() error`.
  - Exec request accepted: `git-receive-pack '/<project>/<app>.git'` (also without leading slash or quotes). Everything else → error reply `luncur is push-only`.

- [ ] **Step 1: Write the failing tests** (`internal/gitssh/gitssh_test.go`):

```go
package gitssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

type fakeBackend struct {
	user    store.User
	branch  string
	pushed  []byte // tarball bytes received
	pushErr error
}

func (f *fakeBackend) Authorize(fp string) (store.User, error) {
	if f.user.ID == 0 {
		return store.User{}, fmt.Errorf("unknown key")
	}
	return f.user, nil
}
func (f *fakeBackend) Branch(u store.User, project, app string) (string, error) {
	if f.branch == "" {
		return "", fmt.Errorf("no such app")
	}
	return f.branch, nil
}
func (f *fakeBackend) Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error {
	b, _ := io.ReadAll(tarball)
	f.pushed = b
	fmt.Fprintln(progress, "building...")
	fmt.Fprintln(progress, "app live")
	return f.pushErr
}

func newTestServer(t *testing.T, b Backend) (addr string) {
	t.Helper()
	hk, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "hostkey"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(hk, b)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}

func TestHostKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hostkey")
	k1, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreateHostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(k1.PublicKey()) != ssh.FingerprintSHA256(k2.PublicKey()) {
		t.Fatal("host key changed between loads")
	}
}

func TestRejectsNonReceivePack(t *testing.T) {
	b := &fakeBackend{user: store.User{ID: 1}, branch: "main"}
	addr := newTestServer(t, b)

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sess, err := conn.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Run("git-upload-pack '/p/a.git'"); err == nil {
		t.Fatal("upload-pack accepted")
	}
	if !strings.Contains(stderr.String(), "push-only") {
		t.Fatalf("stderr = %q, want push-only message", stderr.String())
	}
}

// TestGitPushEndToEnd drives a REAL `git push` against the server and
// asserts the backend received a tarball containing the committed file and
// the client saw the progress lines.
func TestGitPushEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	b := &fakeBackend{user: store.User{ID: 1}, branch: "main"}
	addr := newTestServer(t, b)

	// Client key: write a throwaway ed25519 keypair for git's ssh to use.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id")
	writeTestClientKey(t, keyPath) // helper below

	// Work repo with one commit on main.
	repo := filepath.Join(dir, "repo")
	runGit(t, "", "init", "-b", "main", repo)
	os.WriteFile(filepath.Join(repo, "hello.txt"), []byte("hi\n"), 0o644)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "one")

	host, port, _ := net.SplitHostPort(addr)
	url := fmt.Sprintf("ssh://git@%s:%s/proj/app.git", host, port)
	cmd := exec.Command("git", "push", url, "main")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh -i "+keyPath+" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "app live") {
		t.Fatalf("push output missing progress:\n%s", out)
	}
	if len(b.pushed) == 0 {
		t.Fatal("backend received no tarball")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeTestClientKey writes an OpenSSH-format ed25519 private key.
func writeTestClientKey(t *testing.T, path string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pemEncode(blk), 0o600); err != nil {
		t.Fatal(err)
	}
}
```

`pemEncode` = `pem.EncodeToMemory(blk)` with `encoding/pem` imported; fold in.

- [ ] **Step 2: Run** `go test ./internal/gitssh/ -v` — package doesn't exist.

- [ ] **Step 3: Implement.**

`internal/gitssh/hostkey.go`:

```go
package gitssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadOrCreateHostKey loads the server host key, generating an ed25519 key
// on first boot. Stored beside the DB (on the PVC in production) so clients
// never see the key change across restarts.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, fmt.Errorf("write host key: %w", err)
	}
	return ssh.NewSignerFromKey(priv)
}
```

`internal/gitssh/gitssh.go` — connection/auth/session plumbing:

```go
// Package gitssh implements luncur's push-only git-over-SSH receiver.
package gitssh

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

// Backend is what the SSH layer needs from the rest of luncur.
type Backend interface {
	Authorize(fingerprint string) (store.User, error)
	Branch(u store.User, project, app string) (string, error)
	Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error
}

type Server struct {
	cfg     *ssh.ServerConfig
	backend Backend

	// HookExe is the binary the post-receive hook execs. Defaults to
	// os.Executable() (the running luncur binary); tests point it at a
	// freshly built cmd/luncur.
	HookExe string

	mu        sync.Mutex
	listeners map[net.Listener]struct{}
	closed    bool
}

const userKey = "luncur-user-id"
const fpKey = "luncur-fingerprint"

func New(hostKey ssh.Signer, backend Backend) *Server {
	s := &Server{backend: backend, listeners: map[net.Listener]struct{}{}}
	s.cfg = &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fp := ssh.FingerprintSHA256(key)
			if _, err := backend.Authorize(fp); err != nil {
				return nil, fmt.Errorf("unknown key")
			}
			return &ssh.Permissions{Extensions: map[string]string{fpKey: fp}}, nil
		},
	}
	s.cfg.AddHostKey(hostKey)
	return s
}

func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("server closed")
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for l := range s.listeners {
		l.Close()
	}
	return nil
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	conn, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	fp := conn.Permissions.Extensions[fpKey]
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs, fp)
	}
}

// receivePackRe extracts project/app from the exec command line, tolerating
// optional quotes and leading slash: git-receive-pack '/proj/app.git'
var receivePackRe = regexp.MustCompile(`^git-receive-pack '?/?([a-z0-9-]+)/([a-z0-9-]+)\.git'?$`)

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, fp string) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			// payload: uint32 length + command string
			if len(req.Payload) < 4 {
				req.Reply(false, nil)
				return
			}
			cmdline := string(req.Payload[4:])
			m := receivePackRe.FindStringSubmatch(strings.TrimSpace(cmdline))
			if m == nil {
				req.Reply(true, nil)
				fmt.Fprintf(ch.Stderr(), "luncur is push-only: use git push (repo path must be /<project>/<app>.git)\n")
				exitSession(ch, 1)
				return
			}
			req.Reply(true, nil)
			code := s.runReceive(ch, fp, m[1], m[2])
			exitSession(ch, code)
			return
		case "env", "shell", "pty-req":
			// env is harmless; shells/ptys are not offered.
			req.Reply(req.Type == "env", nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// runReceive authorizes and hands off to the git plumbing in receive.go.
func (s *Server) runReceive(ch ssh.Channel, fp, project, app string) int {
	u, err := s.backend.Authorize(fp)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "access denied\n")
		return 1
	}
	branch, err := s.backend.Branch(u, project, app)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "%v\n", err)
		return 1
	}
	if err := s.receive(ch, u, project, app, branch); err != nil {
		fmt.Fprintf(ch.Stderr(), "push failed: %v\n", err)
		return 1
	}
	return 0
}

func exitSession(ch ssh.Channel, code int) {
	payload := make([]byte, 4)
	payload[3] = byte(code)
	ch.SendRequest("exit-status", false, payload)
}
```

(Drop the `"log"` import if nothing in the final file uses it — `go vet`/compiler will flag it.)

`internal/gitssh/receive.go` — the temp-repo + hook + socket dance:

```go
package gitssh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/store"
)

// receive runs git-receive-pack into a throwaway bare repo. A post-receive
// hook (this same binary, hidden command `_push-hook`) relays the pushed
// refs to us over a unix socket; we archive the deploy branch, run the
// backend push synchronously, and stream progress lines back through the
// hook so the git client prints them as "remote: ...".
func (s *Server) receive(ch ssh.Channel, u store.User, project, app, branch string) error {
	tmp, err := os.MkdirTemp("", "luncur-push-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	repo := filepath.Join(tmp, "repo.git")

	if out, err := exec.Command("git", "init", "--bare", "--quiet", repo).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v\n%s", err, out)
	}

	sock := filepath.Join(tmp, "hook.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer l.Close()

	hookErr := make(chan error, 1)
	go func() { hookErr <- s.serveHook(l, u, project, app, branch, repo) }()

	exe := s.HookExe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	hook := fmt.Sprintf("#!/bin/sh\nexec %q _push-hook\n", exe)
	hooksDir := filepath.Join(repo, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(hook), 0o755); err != nil {
		return err
	}

	cmd := exec.Command("git-receive-pack", repo)
	cmd.Env = append(os.Environ(), "LUNCUR_PUSH_SOCK="+sock)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git-receive-pack: %w", err)
	}

	select {
	case err := <-hookErr:
		return err
	case <-time.After(time.Second):
		// No hook connection: nothing was pushed (e.g. everything up to
		// date). Not an error.
		return nil
	}
}

// serveHook accepts the single post-receive connection. Protocol: hook
// sends the ref-update lines ("<old> <new> <refname>\n") followed by a
// blank line; we stream progress lines back; final line "__luncur_exit__ N".
func (s *Server) serveHook(l net.Listener, u store.User, project, app, branch, repo string) error {
	conn, err := l.Accept()
	if err != nil {
		return nil // receive-pack finished without invoking the hook
	}
	defer conn.Close()

	sc := bufio.NewScanner(conn)
	var pushedSHA string
	want := "refs/heads/" + branch
	var refs []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		refs = append(refs, line)
		parts := strings.Fields(line)
		if len(parts) == 3 && parts[2] == want {
			pushedSHA = parts[1]
		}
	}

	fail := func(format string, a ...any) error {
		fmt.Fprintf(conn, format+"\n", a...)
		fmt.Fprintln(conn, "__luncur_exit__ 1")
		return fmt.Errorf(format, a...)
	}

	if pushedSHA == "" || pushedSHA == strings.Repeat("0", 40) {
		return fail("nothing deployed: push the %q branch (got: %s)", branch, strings.Join(refs, ", "))
	}

	// Archive the pushed commit as tar.gz — same format tarball deploys use.
	fmt.Fprintf(conn, "-----> archiving %s\n", pushedSHA[:8])
	archive := exec.Command("git", "-C", repo, "archive", "--format=tar.gz", pushedSHA)
	tarball, err := archive.StdoutPipe()
	if err != nil {
		return fail("archive: %v", err)
	}
	var archErr strings.Builder
	archive.Stderr = &archErr
	if err := archive.Start(); err != nil {
		return fail("archive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	pushErr := s.backend.Push(ctx, u, project, app, tarball, conn)
	if werr := archive.Wait(); werr != nil && pushErr == nil {
		pushErr = fmt.Errorf("git archive: %v\n%s", werr, archErr.String())
	}
	if pushErr != nil {
		return fail("BUILD FAILED: %v", pushErr)
	}
	fmt.Fprintln(conn, "__luncur_exit__ 0")
	return nil
}
```

(`receive.go` imports `golang.org/x/crypto/ssh` only for the `ssh.Channel` parameter type — keep imports minimal; the compiler will flag anything unused.)

- [ ] **Step 4: Add the hidden `_push-hook` command** — without it the e2e test's hook can't run. Create `internal/cli/pushhook.go`:

```go
package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// pushHookCmd is the hidden post-receive hook entrypoint. git runs it inside
// the throwaway push repo; it relays stdin (ref updates) to the gitssh
// server over the unix socket in $LUNCUR_PUSH_SOCK, then streams progress
// lines back to its own stderr (which git shows the pusher as "remote: ").
// Exit code comes from the final "__luncur_exit__ N" line.
func pushHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "_push-hook",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sock := os.Getenv("LUNCUR_PUSH_SOCK")
			if sock == "" {
				return fmt.Errorf("_push-hook: LUNCUR_PUSH_SOCK not set")
			}
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return err
			}
			defer conn.Close()
			if _, err := io.Copy(conn, os.Stdin); err != nil {
				return err
			}
			fmt.Fprintln(conn) // blank line = end of refs
			rd := bufio.NewReader(conn)
			for {
				line, err := rd.ReadString('\n')
				if trimmed := strings.TrimRight(line, "\n"); trimmed != "" {
					if code, ok := strings.CutPrefix(trimmed, "__luncur_exit__ "); ok {
						if code != "0" {
							os.Exit(1)
						}
						return nil
					}
					fmt.Fprintln(os.Stderr, trimmed)
				}
				if err != nil {
					return nil
				}
			}
		},
	}
}
```

(Imports: `bufio`, `fmt`, `io`, `net`, `os`, `strings`, cobra.) Register in `root.go`: `pushHookCmd()`.

- [ ] **Step 5: Run** `go test ./internal/gitssh/ -v` — all pass, including `TestGitPushEndToEnd` (it needs the `_push-hook` command? **No** — note: the hook execs the TEST binary, not the luncur binary, because `os.Executable()` inside the test process is the test binary. Fix: `Server` gets a `HookExe string` field (default `os.Executable()`), and the test builds the real luncur binary once via `go build -o <tmp>/luncur ./cmd/luncur` in a `TestMain` and sets `srv.HookExe` to it. Implement exactly that: add `HookExe` to `Server`, use it in `receive()`, and in the test's `newTestServer` build the binary with `exec.Command("go", "build", "-o", bin, "github.com/sutantodadang/luncur/cmd/luncur")` — skip the e2e test if the build fails.)
- [ ] **Step 6: Run** `go build ./... && go vet ./... && go test ./...` — green.
- [ ] **Step 7: Commit** — `feat: gitssh — push-only SSH server with post-receive build relay`

---

### Task 5: server — Backend implementation (push → existing build pipeline)

**Files:**
- Create: `internal/server/push.go`
- Modify: `internal/server/server.go` (exported constructor)
- Test: `internal/server/push_test.go`

**Interfaces:**
- Consumes: `gitssh.Backend` (Task 4), existing `s.st` store methods (`GetProject`, `IsMember`, `GetApp`, `CreateDeployment`, `GetDeployment`), `s.src.Save/LogPath`, `s.runBuild` (synchronous build), `hostFor` (exists in the render/sync path — check `internal/server/sync.go` for the exact helper that builds app URLs; reuse it).
- Produces:
  - `type PushBackend struct{ s *server }` implementing `gitssh.Backend`.
  - `func NewWithBackend(d Deps) (http.Handler, *PushBackend)` — same wiring as `New`, also returns the backend bound to the same server instance. `New(d)` becomes `h, _ := NewWithBackend(d); return h`.

- [ ] **Step 1: Write the failing test** (`internal/server/push_test.go`). Reuse the fake-kube arrange from `build_test.go` (`TestRunBuildSuccess`) — it shows how to build a server whose `runBuild` succeeds against a fake dynamic client with a pre-completed Job. Assertions:

```go
func TestPushBackendHappyPath(t *testing.T) {
	// Arrange: server via the same fixture TestRunBuildSuccess uses
	// (temp store + DataDir, fake kube where the build Job completes).
	// Seed: user (member), project, app (branch main via default),
	// register an ssh key for the user (store.AddSSHKey) and keep its
	// fingerprint.
	// backend := &PushBackend{s: srv} — or via NewWithBackend.

	// 1. Authorize(fp) returns the user; Authorize("SHA256:nope") errors.
	// 2. Branch(user, project, app) == "main"; Branch on a project the
	//    user is not a member of errors with "not a member"; unknown app
	//    errors with "no such app".
	// 3. Push(ctx, user, project, app, tarballReader, &progressBuf):
	//    - creates a deployment row that ends status "live"
	//    - the tarball bytes land at src.TarballPath(deployID)
	//    - progressBuf contains "live"
	// Write with real assertions following build_test.go's arrange code.
}

func TestPushBackendBuildFailure(t *testing.T) {
	// Same arrange but fake kube Job fails (build_test.go shows the
	// failing-Job variant or construct one): Push returns an error and the
	// deployment row ends status "failed".
}
```

Replace comment scaffolding with real code — `build_test.go` contains the exact fake-kube setup to copy.

- [ ] **Step 2: Run** — compile failure.

- [ ] **Step 3: Implement.** `internal/server/push.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// PushBackend adapts the server's deploy pipeline to gitssh.Backend.
type PushBackend struct{ s *server }

func (b *PushBackend) Authorize(fingerprint string) (store.User, error) {
	return b.s.st.UserForSSHFingerprint(fingerprint)
}

// Branch validates access and returns the deploy branch: the app's
// configured git branch when set, else "main".
func (b *PushBackend) Branch(u store.User, project, app string) (string, error) {
	p, err := b.s.st.GetProject(project)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no such project %q", project)
	}
	if err != nil {
		return "", fmt.Errorf("internal error")
	}
	if u.Role != "admin" {
		ok, err := b.s.st.IsMember(p.ID, u.ID)
		if err != nil || !ok {
			return "", fmt.Errorf("not a member of project %q", project)
		}
	}
	a, err := b.s.st.GetApp(p.ID, app)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no such app %q in project %q", app, project)
	}
	if err != nil {
		return "", fmt.Errorf("internal error")
	}
	if a.GitBranch != "" {
		return a.GitBranch, nil
	}
	return "main", nil
}

// Push saves the tarball as a new deployment and runs the build
// synchronously, tailing the build log into progress while it runs.
func (b *PushBackend) Push(ctx context.Context, u store.User, project, app string, tarball io.Reader, progress io.Writer) error {
	s := b.s
	if s.kube == nil {
		return fmt.Errorf("kubernetes unavailable on the server")
	}
	if s.src == nil {
		return fmt.Errorf("server has no data dir configured")
	}
	p, err := s.st.GetProject(project)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	a, err := s.st.GetApp(p.ID, app)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}

	d, err := s.st.CreateDeployment(a.ID, "building", "", u.ID)
	if err != nil {
		return fmt.Errorf("create deployment: %w", err)
	}
	if _, err := s.src.Save(d.ID, tarball); err != nil {
		s.st.SetDeploymentStatus(d.ID, "failed")
		return fmt.Errorf("save source: %w", err)
	}

	fmt.Fprintf(progress, "-----> deploy %d building\n", d.ID)

	// Tail the build log into the pusher's terminal while runBuild runs.
	done := make(chan struct{})
	go tailFile(done, s.src.LogPath(d.ID), progress)

	err = s.runBuild(ctx, p, a, d)
	close(done)

	if err != nil {
		return fmt.Errorf("deploy %d failed (luncur logs %s --project %s --deploy %d)", d.ID, app, project, d.ID)
	}
	fmt.Fprintf(progress, "-----> app live: http://%s\n", hostFor(a.Name, s.externalIP))
	return nil
}

// tailFile streams appended lines of path to w until done closes, then
// drains whatever remains. Missing file = keep polling (the build Job
// creates it).
func tailFile(done <-chan struct{}, path string, w io.Writer) {
	var off int64
	flush := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return
		}
		b, _ := io.ReadAll(f)
		if len(b) == 0 {
			return
		}
		off += int64(len(b))
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			fmt.Fprintln(w, line)
		}
	}
	for {
		select {
		case <-done:
			flush()
			return
		case <-time.After(500 * time.Millisecond):
			flush()
		}
	}
}

// NewWithBackend builds the HTTP handler plus the push backend bound to the
// same server instance, so `luncur serve` can wire both from one Deps.
func NewWithBackend(d Deps) (http.Handler, *PushBackend) {
	s := newServer(d)
	return s.handler(), &PushBackend{s: s}
}
```

And in `server.go`, replace `New`'s body:

```go
// New builds the full API handler. Later plans add their routes here.
func New(d Deps) http.Handler {
	h, _ := NewWithBackend(d)
	return h
}
```

Check `hostFor`'s real signature in `internal/server/sync.go` before using it (Plan D's UI code calls `hostFor(a.Name, s.externalIP)` — confirm and match). The error message in `Push` build-failure path has a format-arg mismatch as written — fix to `fmt.Errorf("deploy %d failed — see: luncur logs %s --project %s --deploy %d", d.ID, app, project, d.ID)`.

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: push backend — git push drives the tarball build pipeline`

---

### Task 6: serve wiring — `--ssh-listen` + `--ssh-hostkey-file`

**Files:**
- Modify: `internal/cli/serve.go`
- Test: `internal/cli/serve_test.go` (append)

**Interfaces:**
- Consumes: `gitssh.New`, `gitssh.LoadOrCreateHostKey`, `server.NewWithBackend` (Tasks 4-5).
- Produces: `luncur serve --ssh-listen :2222 --ssh-hostkey-file <path>`; empty `--ssh-listen` disables SSH. Host key default: `luncur_host_key` beside `--db`.

- [ ] **Step 1: Write the failing test** (append to `internal/cli/serve_test.go`, following its existing style — it likely tests flag defaults; read it first):

```go
func TestServeSSHFlags(t *testing.T) {
	cmd := serveCmd()
	sshListen, err := cmd.Flags().GetString("ssh-listen")
	if err != nil || sshListen != ":2222" {
		t.Fatalf("ssh-listen default = %q err=%v, want :2222", sshListen, err)
	}
	if _, err := cmd.Flags().GetString("ssh-hostkey-file"); err != nil {
		t.Fatalf("ssh-hostkey-file flag missing: %v", err)
	}
}
```

- [ ] **Step 2: Run** — fails.

- [ ] **Step 3: Implement** in `serve.go`:

Flag declarations (with the others):

```go
	var sshListen, sshHostKeyFile string
	// ... in the flag block:
	cmd.Flags().StringVar(&sshListen, "ssh-listen", ":2222", "git-push SSH listen address (empty disables)")
	cmd.Flags().StringVar(&sshHostKeyFile, "ssh-hostkey-file", "", "SSH host key path (default luncur_host_key beside --db)")
```

Handler construction switches to `NewWithBackend`; SSH startup after the HTTP server goroutine (before the signal wait):

```go
	handler, pushBackend := server.NewWithBackend(server.Deps{ /* same fields as today */ })
	srv := &http.Server{ Addr: listen, Handler: handler, /* same timeouts */ }
```

```go
	var sshSrv *gitssh.Server
	if sshListen != "" {
		hostKeyPath := sshHostKeyFile
		if hostKeyPath == "" {
			hostKeyPath = filepath.Join(filepath.Dir(dbPath), "luncur_host_key")
		}
		hostKey, err := gitssh.LoadOrCreateHostKey(hostKeyPath)
		if err != nil {
			return fmt.Errorf("ssh host key: %w", err)
		}
		l, err := net.Listen("tcp", sshListen)
		if err != nil {
			return fmt.Errorf("ssh listen: %w", err)
		}
		sshSrv = gitssh.New(hostKey, pushBackend)
		log.Printf("luncur git-ssh listening on %s", sshListen)
		go func() {
			if err := sshSrv.Serve(l); err != nil {
				log.Printf("ssh server: %v", err)
			}
		}()
	}
```

Shutdown path gains (in the `<-ctx.Done()` branch, before returning):

```go
	if sshSrv != nil {
		sshSrv.Close()
	}
```

Add `"net"` and the gitssh import.

- [ ] **Step 4: Run** `go test ./internal/cli/ -v && go build ./...` — pass.
- [ ] **Step 5: Commit** — `feat: serve — git-push SSH listener (--ssh-listen, --ssh-hostkey-file)`

---

### Task 7: manifests — SSH port + NodePort Service

**Files:**
- Modify: `internal/up/manifests.go`
- Test: `internal/up/manifests_test.go` (extend)

**Interfaces:**
- Consumes: existing `LuncurObjects`, `Params`.
- Produces: luncur Deployment container gets port 2222 + `--ssh-listen :2222` arg (host key lands on the PVC automatically — it's beside `--db` which is on `/var/lib/luncur`); new Service `luncur-ssh`, type NodePort, port 2222 → nodePort **30022**. `SSHNodePort = 30022` exported const.

- [ ] **Step 1: Extend the failing test** — in `manifests_test.go`'s `TestLuncurObjects`, add to the `kinds` expectation nothing new (Service kind already asserted), but add content assertions:

```go
	for _, want := range []string{
		`"--ssh-listen"`,
		`"nodePort":30022`,
		`"luncur-ssh"`,
		`"containerPort":2222`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
```

- [ ] **Step 2: Run** `go test ./internal/up/ -v` — fails.

- [ ] **Step 3: Implement** in `manifests.go`:

```go
// SSHNodePort is where git push reaches the in-cluster SSH receiver:
// ssh://git@<ip>:30022/<project>/<app>.git
const SSHNodePort = 30022
```

Deployment container: append to `Args`:

```go
	"--ssh-listen", ":2222",
```

and to `Ports`:

```go
	Ports: []corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 2222}},
```

New Service after the existing `luncur` Service (same `add` pattern):

```go
	sshSvc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "luncur-ssh",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app.kubernetes.io/name": "luncur"},
			Ports: []corev1.ServicePort{{
				Port:       2222,
				TargetPort: intstr.FromInt32(2222),
				NodePort:   SSHNodePort,
			}},
		},
	}
	if err := add("Service", sshSvc); err != nil {
		return nil, err
	}
```

- [ ] **Step 4: Run** `go test ./internal/up/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: expose git-ssh via NodePort 30022 in self-deploy manifests`

---

### Task 8: Dockerfile + README + push URL hint

**Files:**
- Create: `Dockerfile`
- Modify: `README.md`
- Modify: `internal/cli/init.go` — ONLY IF this file exists; check first with `ls internal/cli/`. If there is no init command, skip the hint and note it in the README instead.
- Test: none new (docs/infra); `docker build` is manual.

**Interfaces:** none — packaging and docs.

- [ ] **Step 1: Create `Dockerfile`** (the server image the release pipeline publishes; git is required by the push receiver):

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /luncur ./cmd/luncur

FROM alpine:3.21
# git: required by the git-push receiver (git-receive-pack, git archive).
RUN apk add --no-cache git ca-certificates
COPY --from=build /luncur /usr/local/bin/luncur
ENTRYPOINT ["/usr/local/bin/luncur"]
CMD ["serve"]
```

- [ ] **Step 2: Verify it builds** (skip if docker unavailable locally; note in the commit message): `docker build -t luncur:dev .` — expect success.

- [ ] **Step 3: README** — add a "Deploy with git push" section after the existing deploy docs:
  - `luncur ssh-key add` once per machine.
  - `git remote add luncur ssh://git@<ip>:30022/<project>/<app>.git`
  - `git push luncur main` → build streams into the push output; app URL printed at the end.
  - Note the two Plan E deviations from the Global Constraints section (host key on PVC; post-receive streaming means push exit code doesn't reflect build failure).
  - Update the status line to mention Plan E shipped.

- [ ] **Step 4: init hint** — if `internal/cli/init.go` exists, after it writes `luncur.toml` print:

```go
	cmd.Printf("tip: git remote add luncur ssh://git@%s:30022/%s/%s.git\n", host, project, appName)
```

adapted to the variables actually in scope there (read the file; if the server host isn't known at init time, print the placeholder form with `<server-ip>`).

- [ ] **Step 5: Run** `go build ./... && go vet ./... && go test ./...` — green.
- [ ] **Step 6: Commit** — `feat: server image with git + git-push docs`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green.
- [ ] `gofmt -l internal/ cmd/` — clean.
- [ ] Manual smoke (optional, owner's VPS): `luncur up`, `luncur ssh-key add`, push a sample app, watch build stream, app live.
- [ ] Push branch `plan-e`, open PR against `main`.

## Spec-coverage self-check (Plan E section of 2026-07-03-luncur-phase2-design.md)

- SSH listener in serve, port 2222, NodePort 30022 ✅ (T6/T7)
- Host key persists across restarts ✅ (T4 file-on-PVC; deviation recorded)
- Public-key-only auth, fingerprint → user ✅ (T1/T4)
- `ssh_keys` table + CLI add/list/remove ✅ (T1/T2/T3)
- push-only (`git-upload-pack` rejected with message) ✅ (T4)
- Temp bare repo, no server-side repo storage ✅ (T4)
- Branch rule: app's configured branch, default main; other refs → helpful rejection ✅ (T4/T5)
- `git archive` → existing tarball pipeline ✅ (T4/T5)
- Build progress streamed to git client, Heroku-style ✅ (T4 hook relay + T5 log tail)
- Membership enforced before receive ✅ (T4 Branch() ordering)
- git binary in server image ✅ (T8)
- `luncur init` remote hint ✅ (T8, conditional)
