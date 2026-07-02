# luncur Plan A — Control-Plane Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The `luncur` binary with SQLite store, multi-user auth, a REST API server, and a CLI that can log in and manage users — the foundation every later plan builds on.

**Architecture:** One Go module, one binary (`cmd/luncur`). `luncur serve` runs the HTTP API backed by SQLite (pure Go driver, WAL). CLI subcommands talk to the API over HTTP with a bearer token stored in the user config dir. Auth: bcrypt passwords, opaque API tokens stored as SHA-256 hashes.

**Tech Stack:** Go 1.22+, spf13/cobra (CLI), modernc.org/sqlite (no CGO), stdlib `net/http` mux (1.22 method patterns), golang.org/x/crypto/bcrypt, golang.org/x/term.

**Plan sequence (Phase 1 = A→D):** A: core (this plan) · B: app model + K8s apply + escape hatch · C: build pipeline + registry · D: web UI + `luncur up`.

## Global Constraints

- Go ≥ 1.22 (stdlib mux method patterns). Module path: `github.com/sutantodadang/luncur`.
- No CGO anywhere — SQLite via `modernc.org/sqlite` only. `CGO_ENABLED=0` builds must pass.
- Single binary: server and CLI are subcommands of the same `cmd/luncur`.
- Dependencies limited to: cobra, modernc.org/sqlite, x/crypto, x/term (this plan). Nothing else without a plan change.
- API error envelope, always: `{"error":{"code":"<snake_case>","message":"<human text>"}}`.
- All API routes under `/v1/`. Auth via `Authorization: Bearer <token>`.
- Tokens: prefix `lcr_` + 32 random bytes hex; DB stores only SHA-256 hex of the full string.
- Roles: exactly `admin` and `member`.
- Tests: standard `go test`, no test frameworks. Temp DBs via `t.TempDir()`.

## File Structure

```
cmd/luncur/main.go              entry point, delegates to internal/cli
internal/cli/root.go            cobra root, Execute(), version
internal/cli/serve.go           `luncur serve`
internal/cli/login.go           `luncur login`
internal/cli/user.go            `luncur user add`
internal/cli/whoami.go          `luncur whoami`
internal/cli/config.go          CLI config file load/save (~/.config/luncur/config.json)
internal/client/client.go       REST client used by CLI commands
internal/store/store.go         Open, migrate, Close
internal/store/schema.sql       full Phase 1 schema (embedded)
internal/store/users.go         user CRUD + password auth
internal/store/tokens.go        API token create/lookup
internal/server/server.go       http.Handler wiring, health route
internal/server/respond.go      JSON + error envelope helpers
internal/server/auth.go         login handler, bearer middleware, admin guard
internal/server/users.go        POST /v1/users, GET /v1/me
```

---

### Task 1: Module scaffold + cobra root

**Files:**
- Create: `go.mod` (via `go mod init`)
- Create: `cmd/luncur/main.go`
- Create: `internal/cli/root.go`
- Test: `internal/cli/root_test.go`

**Interfaces:**
- Produces: `cli.Execute() error` (called by main); `cli.version` string var (set via ldflags later).

- [ ] **Step 1: Init module and get deps**

```bash
go mod init github.com/sutantodadang/luncur
go get github.com/spf13/cobra@latest
```

- [ ] **Step 2: Write the failing test**

`internal/cli/root_test.go`:

```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "dev") {
		t.Fatalf("want version output containing 'dev', got %q", out.String())
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestVersionCommand -v`
Expected: FAIL — `undefined: newRoot`

- [ ] **Step 4: Write minimal implementation**

`internal/cli/root.go`:

```go
package cli

import "github.com/spf13/cobra"

// version is overridden at release time via
// -ldflags "-X github.com/sutantodadang/luncur/internal/cli.version=v0.x.y".
var version = "dev"

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "luncur",
		Short:         "luncur — tiny self-hosted PaaS on K3s",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the luncur version",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Println(version)
		},
	})
	return root
}

// Execute runs the CLI. It is the only symbol main needs.
func Execute() error {
	return newRoot().Execute()
}
```

`cmd/luncur/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"github.com/sutantodadang/luncur/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestVersionCommand -v` → PASS
Run: `CGO_ENABLED=0 go build ./...` → exit 0

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd internal
git commit -m "feat: scaffold luncur binary with cobra root and version command"
```

---

### Task 2: SQLite store with full Phase 1 schema

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/schema.sql`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `store.Open(path string) (*Store, error)`, `(*Store) Close() error`, `(*Store) DB() *sql.DB` (escape hatch for later plans' queries in the same package family).

- [ ] **Step 1: Get the driver**

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Write the failing test**

`internal/store/store_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigratesSchema(t *testing.T) {
	s := openTest(t)
	for _, table := range []string{
		"users", "api_tokens", "projects", "project_members",
		"apps", "deployments", "env_vars", "domains", "overrides", "invites",
	} {
		var n int
		err := s.DB().QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		s.Close()
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: Open`

- [ ] **Step 4: Write the schema**

`internal/store/schema.sql`:

```sql
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin','member')),
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  last_used_at TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS projects (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  k8s_namespace TEXT NOT NULL UNIQUE,
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('admin','member')),
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE IF NOT EXISTS apps (
  id            INTEGER PRIMARY KEY,
  project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  source_type   TEXT NOT NULL CHECK (source_type IN ('tarball','git')),
  git_url       TEXT,
  git_branch    TEXT,
  git_token_enc BLOB,
  port          INTEGER NOT NULL DEFAULT 8080,
  replicas      INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS deployments (
  id         INTEGER PRIMARY KEY,
  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  status     TEXT NOT NULL CHECK (status IN ('building','deploying','live','failed')),
  image_ref  TEXT,
  log_path   TEXT,
  created_by INTEGER REFERENCES users(id),
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS env_vars (
  app_id    INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  value_enc BLOB NOT NULL,
  PRIMARY KEY (app_id, key)
);

CREATE TABLE IF NOT EXISTS domains (
  id       INTEGER PRIMARY KEY,
  app_id   INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  tls      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS overrides (
  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  patch_json TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (app_id, kind)
);

CREATE TABLE IF NOT EXISTS invites (
  token      TEXT PRIMARY KEY,
  role       TEXT NOT NULL CHECK (role IN ('admin','member')),
  expires_at TEXT NOT NULL
);
```

- [ ] **Step 5: Write minimal implementation**

`internal/store/store.go`:

```go
// Package store persists luncur's control-plane metadata in SQLite.
// Cluster state itself lives in K3s (etcd); this DB holds users, apps,
// deploy history, and overrides.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite DB at path and applies the
// schema. Safe to call repeatedly on the same file.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc sqlite is single-writer; avoid SQLITE_BUSY churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for queries owned by sibling files/plans.
func (s *Store) DB() *sql.DB { return s.db }
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (both tests)

- [ ] **Step 7: Commit**

```bash
git add internal/store go.mod go.sum
git commit -m "feat: SQLite store with full phase-1 schema (pure Go, WAL)"
```

---

### Task 3: Users — create + password authentication

**Files:**
- Create: `internal/store/users.go`
- Test: `internal/store/users_test.go`

**Interfaces:**
- Consumes: `Store` from Task 2.
- Produces:
  - `type User struct { ID int64; Email string; Role string }`
  - `(*Store) CreateUser(email, password, role string) (User, error)` — validates role, bcrypt-hashes password.
  - `(*Store) Authenticate(email, password string) (User, error)` — returns `ErrAuthFailed` on unknown email or bad password.
  - `var ErrAuthFailed = errors.New("authentication failed")`

- [ ] **Step 1: Get bcrypt**

```bash
go get golang.org/x/crypto@latest
```

- [ ] **Step 2: Write the failing test**

`internal/store/users_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestCreateAndAuthenticateUser(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("a@b.co", "s3cret-pw", "admin")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "a@b.co" || u.Role != "admin" || u.ID == 0 {
		t.Fatalf("bad user: %+v", u)
	}

	got, err := s.Authenticate("a@b.co", "s3cret-pw")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("want id %d, got %d", u.ID, got.ID)
	}

	if _, err := s.Authenticate("a@b.co", "wrong"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed, got %v", err)
	}
	if _, err := s.Authenticate("nobody@b.co", "s3cret-pw"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed for unknown email, got %v", err)
	}
}

func TestCreateUserRejectsBadInput(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateUser("a@b.co", "pw", "superuser"); err == nil {
		t.Fatal("want error for invalid role")
	}
	if _, err := s.CreateUser("", "pw", "member"); err == nil {
		t.Fatal("want error for empty email")
	}
	if _, _ = s.CreateUser("dup@b.co", "pw", "member"); true {
		if _, err := s.CreateUser("dup@b.co", "pw", "member"); err == nil {
			t.Fatal("want error for duplicate email")
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/ -run User -v`
Expected: FAIL — `undefined: ErrAuthFailed` / `s.CreateUser undefined`

- [ ] **Step 4: Write minimal implementation**

`internal/store/users.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

var ErrAuthFailed = errors.New("authentication failed")

type User struct {
	ID    int64
	Email string
	Role  string
}

func (s *Store) CreateUser(email, password, role string) (User, error) {
	if email == "" {
		return User{}, errors.New("email required")
	}
	if role != "admin" && role != "member" {
		return User{}, fmt.Errorf("invalid role %q", role)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO users (email, password_hash, role) VALUES (?, ?, ?)`,
		email, string(hash), role,
	)
	if err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, Role: role}, nil
}

func (s *Store) Authenticate(email, password string) (User, error) {
	var u User
	var hash string
	err := s.db.QueryRow(
		`SELECT id, email, role, password_hash FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Email, &u.Role, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, ErrAuthFailed
	}
	return u, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (all)

- [ ] **Step 6: Commit**

```bash
git add internal/store go.mod go.sum
git commit -m "feat: user creation and bcrypt password authentication"
```

---

### Task 4: API tokens

**Files:**
- Create: `internal/store/tokens.go`
- Test: `internal/store/tokens_test.go`

**Interfaces:**
- Consumes: `Store`, `User` from Tasks 2–3.
- Produces:
  - `(*Store) CreateToken(userID int64, name string) (plaintext string, err error)` — plaintext format `lcr_<64 hex chars>`; only SHA-256 hex stored.
  - `(*Store) UserForToken(plaintext string) (User, error)` — returns `ErrAuthFailed` for unknown/malformed tokens; touches `last_used_at`.

- [ ] **Step 1: Write the failing test**

`internal/store/tokens_test.go`:

```go
package store

import (
	"errors"
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("t@b.co", "pw", "member")
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.CreateToken(u.ID, "cli")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if !strings.HasPrefix(tok, "lcr_") || len(tok) != 4+64 {
		t.Fatalf("bad token format: %q", tok)
	}

	got, err := s.UserForToken(tok)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("want user %d, got %d", u.ID, got.ID)
	}

	// Plaintext must not be stored anywhere.
	var n int
	if err := s.DB().QueryRow(
		`SELECT count(*) FROM api_tokens WHERE hash = ?`, tok,
	).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext token stored in DB (n=%d err=%v)", n, err)
	}
}

func TestUserForTokenRejectsUnknown(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{"", "lcr_deadbeef", "not-a-token"} {
		if _, err := s.UserForToken(bad); !errors.Is(err, ErrAuthFailed) {
			t.Errorf("token %q: want ErrAuthFailed, got %v", bad, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run Token -v`
Expected: FAIL — `s.CreateToken undefined`

- [ ] **Step 3: Write minimal implementation**

`internal/store/tokens.go`:

```go
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// CreateToken mints an opaque API token for a user. The plaintext is
// returned exactly once; the DB keeps only its SHA-256.
func (s *Store) CreateToken(userID int64, name string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plaintext := "lcr_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	_, err := s.db.Exec(
		`INSERT INTO api_tokens (user_id, hash, name) VALUES (?, ?, ?)`,
		userID, hex.EncodeToString(sum[:]), name,
	)
	if err != nil {
		return "", fmt.Errorf("insert token: %w", err)
	}
	return plaintext, nil
}

func (s *Store) UserForToken(plaintext string) (User, error) {
	sum := sha256.Sum256([]byte(plaintext))
	h := hex.EncodeToString(sum[:])
	var u User
	err := s.db.QueryRow(
		`SELECT u.id, u.email, u.role FROM api_tokens t
		 JOIN users u ON u.id = t.user_id WHERE t.hash = ?`, h,
	).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAuthFailed
	}
	if err != nil {
		return User{}, err
	}
	s.db.Exec(`UPDATE api_tokens SET last_used_at = datetime('now') WHERE hash = ?`, h)
	return u, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: opaque API tokens stored as SHA-256 hashes"
```

---

### Task 5: HTTP server skeleton — health, respond helpers, login

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/respond.go`
- Create: `internal/server/auth.go`
- Test: `internal/server/server_test.go`

**Interfaces:**
- Consumes: `store.Store`, `store.User`, `store.ErrAuthFailed` (Tasks 2–4).
- Produces:
  - `server.New(st *store.Store) http.Handler` — the full API handler; later plans register more routes inside `New`.
  - `respond.writeJSON(w, status, v)` / `writeError(w, status, code, msg)` (package-private helpers reused by every handler).
  - Route `GET /v1/health` → `{"status":"ok"}`.
  - Route `POST /v1/login` body `{"email","password"}` → 200 `{"token":"lcr_..."}` or 401 envelope.

- [ ] **Step 1: Write the failing test**

`internal/server/server_test.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func testServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealth(t *testing.T) {
	srv, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
}

func TestLogin(t *testing.T) {
	srv, st := testServer(t)
	if _, err := st.CreateUser("a@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, srv.URL+"/v1/login", `{"email":"a@b.co","password":"pw123456"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out struct{ Token string `json:"token"` }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, "lcr_") {
		t.Fatalf("bad token: %q", out.Token)
	}

	bad := postJSON(t, srv.URL+"/v1/login", `{"email":"a@b.co","password":"nope"}`)
	defer bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Fatalf("want 401, got %d", bad.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bad.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "auth_failed" {
		t.Fatalf("want code auth_failed, got %q", env.Error.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write minimal implementation**

`internal/server/respond.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError emits the API-wide error envelope:
// {"error":{"code":"...","message":"..."}}
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
```

`internal/server/server.go`:

```go
// Package server implements luncur's REST API.
package server

import (
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

type server struct {
	st *store.Store
}

// New builds the full API handler. Later plans add their routes here.
func New(st *store.Store) http.Handler {
	s := &server{st: st}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/login", s.handleLogin)

	return mux
}
```

`internal/server/auth.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	u, err := s.st.Authenticate(req.Email, req.Password)
	if errors.Is(err, store.ErrAuthFailed) {
		writeError(w, http.StatusUnauthorized, "auth_failed", "wrong email or password")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tok, err := s.st.CreateToken(u.ID, "login")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -v` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat: API server skeleton with health and login endpoints"
```

---

### Task 6: Bearer auth middleware, /v1/me, admin-only user creation

**Files:**
- Modify: `internal/server/auth.go` (add middleware)
- Modify: `internal/server/server.go` (register routes)
- Create: `internal/server/users.go`
- Test: `internal/server/users_test.go`

**Interfaces:**
- Consumes: `store.UserForToken` (Task 4), `writeError`/`writeJSON` (Task 5).
- Produces:
  - `(*server) authed(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc` — bearer-token middleware.
  - `(*server) adminOnly(...)` — same signature, additionally requires role `admin` (403 `forbidden` otherwise).
  - Route `GET /v1/me` → `{"id":1,"email":"...","role":"..."}`.
  - Route `POST /v1/users` (admin) body `{"email","password","role"}` → 201 same shape as /v1/me.

- [ ] **Step 1: Write the failing test**

`internal/server/users_test.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func doAuthed(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func seedUserToken(t *testing.T, st *store.Store, email, role string) string {
	t.Helper()
	u, err := st.CreateUser(email, "pw123456", role)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.CreateToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMeRequiresAuth(t *testing.T) {
	srv, st := testServer(t)
	resp := doAuthed(t, "GET", srv.URL+"/v1/me", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}

	tok := seedUserToken(t, st, "me@b.co", "member")
	ok := doAuthed(t, "GET", srv.URL+"/v1/me", tok, "")
	defer ok.Body.Close()
	if ok.StatusCode != 200 {
		t.Fatalf("with token: want 200, got %d", ok.StatusCode)
	}
	var me struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(ok.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.Email != "me@b.co" || me.Role != "member" {
		t.Fatalf("bad me: %+v", me)
	}
}

func TestCreateUserAdminOnly(t *testing.T) {
	srv, st := testServer(t)
	adminTok := seedUserToken(t, st, "root@b.co", "admin")
	memberTok := seedUserToken(t, st, "pleb@b.co", "member")

	body := `{"email":"new@b.co","password":"pw123456","role":"member"}`

	forbidden := doAuthed(t, "POST", srv.URL+"/v1/users", memberTok, body)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != 403 {
		t.Fatalf("member: want 403, got %d", forbidden.StatusCode)
	}

	created := doAuthed(t, "POST", srv.URL+"/v1/users", adminTok, body)
	defer created.Body.Close()
	if created.StatusCode != 201 {
		t.Fatalf("admin: want 201, got %d", created.StatusCode)
	}
	if _, err := st.Authenticate("new@b.co", "pw123456"); err != nil {
		t.Fatalf("new user cannot authenticate: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run 'Me|CreateUser' -v`
Expected: FAIL — 404s (routes not registered)

- [ ] **Step 3: Write minimal implementation**

Append to `internal/server/auth.go`:

```go
// authed wraps a handler with bearer-token authentication.
func (s *server) authed(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		u, err := s.st.UserForToken(h[len(prefix):])
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		next(w, r, u)
	}
}

func (s *server) adminOnly(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, u store.User) {
		if u.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next(w, r, u)
	})
}
```

`internal/server/users.go`:

```go
package server

import (
	"encoding/json"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func userJSON(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "role": u.Role}
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, userJSON(u))
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request, _ store.User) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "bad_request", "password must be at least 8 characters")
		return
	}
	u, err := s.st.CreateUser(req.Email, req.Password, req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, userJSON(u))
}
```

In `internal/server/server.go`, register inside `New` after the login route:

```go
	mux.HandleFunc("GET /v1/me", s.authed(s.handleMe))
	mux.HandleFunc("POST /v1/users", s.adminOnly(s.handleCreateUser))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat: bearer auth middleware, /v1/me, admin-only user creation"
```

---

### Task 7: `luncur serve` command

**Files:**
- Create: `internal/cli/serve.go`
- Modify: `internal/cli/root.go` (register command)
- Test: `internal/cli/serve_test.go`

**Interfaces:**
- Consumes: `store.Open`, `server.New`.
- Produces: `luncur serve --db <path> --listen <addr>` (defaults `luncur.db`, `:8080`). Also `--bootstrap-admin <email>:<password>` flag: creates the admin if no users exist (used by `luncur up` in Plan D), prints nothing if users already exist.

- [ ] **Step 1: Write the failing test**

`internal/cli/serve_test.go`:

```go
package cli

import (
	"path/filepath"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func TestBootstrapAdmin(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapAdmin(st, "root@b.co:hunter2222"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := st.Authenticate("root@b.co", "hunter2222"); err != nil {
		t.Fatalf("admin cannot authenticate: %v", err)
	}
	// Second call is a no-op, not an error (idempotent restarts).
	if err := bootstrapAdmin(st, "other@b.co:whatever123"); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if _, err := st.Authenticate("other@b.co", "whatever123"); err == nil {
		t.Fatal("second admin should not have been created")
	}
	st.Close()
}

func TestBootstrapAdminRejectsBadSpec(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := bootstrapAdmin(st, "no-colon-here"); err == nil {
		t.Fatal("want error for spec without colon")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run Bootstrap -v`
Expected: FAIL — `undefined: bootstrapAdmin`

- [ ] **Step 3: Write minimal implementation**

`internal/cli/serve.go`:

```go
package cli

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

// bootstrapAdmin creates the initial admin from "email:password" iff the
// users table is empty. Idempotent so `luncur serve` restarts are safe.
func bootstrapAdmin(st *store.Store, spec string) error {
	email, password, ok := strings.Cut(spec, ":")
	if !ok || email == "" || password == "" {
		return fmt.Errorf("--bootstrap-admin must be email:password")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := st.CreateUser(email, password, "admin")
	return err
}

func serveCmd() *cobra.Command {
	var dbPath, listen, bootstrap string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the luncur API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			if bootstrap != "" {
				if err := bootstrapAdmin(st, bootstrap); err != nil {
					return err
				}
			}
			log.Printf("luncur serve listening on %s (db %s)", listen, dbPath)
			return http.ListenAndServe(listen, server.New(st))
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "luncur.db", "path to SQLite database")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "listen address")
	cmd.Flags().StringVar(&bootstrap, "bootstrap-admin", "",
		"email:password — create initial admin if no users exist")
	return cmd
}
```

In `internal/cli/root.go`, inside `newRoot()` before `return root`:

```go
	root.AddCommand(serveCmd())
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -v` → PASS
Run: `CGO_ENABLED=0 go build ./...` → exit 0

- [ ] **Step 5: Commit**

```bash
git add internal/cli
git commit -m "feat: luncur serve command with admin bootstrap"
```

---

### Task 8: CLI config + REST client

**Files:**
- Create: `internal/cli/config.go`
- Create: `internal/client/client.go`
- Test: `internal/client/client_test.go`
- Test: `internal/cli/config_test.go`

**Interfaces:**
- Consumes: API routes from Tasks 5–6.
- Produces:
  - `cli.Config struct { Server string `json:"server"`; Token string `json:"token"` }`
  - `cli.loadConfig() (Config, error)` / `cli.saveConfig(Config) error` — file at `os.UserConfigDir()/luncur/config.json`, overridable via env `LUNCUR_CONFIG` (used by tests).
  - `client.New(server, token string) *Client`
  - `(*Client) Login(email, password string) (token string, err error)`
  - `(*Client) Me() (client.UserInfo, error)` with `UserInfo struct { ID int64; Email, Role string }`
  - `(*Client) CreateUser(email, password, role string) (client.UserInfo, error)`
  - All methods decode the error envelope and return `fmt.Errorf("%s (%s)", message, code)` on non-2xx.

- [ ] **Step 1: Write the failing tests**

`internal/client/client_test.go`:

```go
package client

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func testAPI(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
}

func TestClientLoginMeCreateUser(t *testing.T) {
	srv, st := testAPI(t)
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}

	c := New(srv.URL, "")
	tok, err := c.Login("root@b.co", "pw123456")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	c = New(srv.URL, tok)
	me, err := c.Me()
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if me.Email != "root@b.co" || me.Role != "admin" {
		t.Fatalf("bad me: %+v", me)
	}

	nu, err := c.CreateUser("m@b.co", "pw123456", "member")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if nu.Email != "m@b.co" {
		t.Fatalf("bad created user: %+v", nu)
	}
}

func TestClientSurfacesAPIErrors(t *testing.T) {
	srv, _ := testAPI(t)
	c := New(srv.URL, "")
	_, err := c.Login("ghost@b.co", "nope")
	if err == nil || !strings.Contains(err.Error(), "auth_failed") {
		t.Fatalf("want auth_failed in error, got %v", err)
	}
}
```

`internal/cli/config_test.go`:

```go
package cli

import (
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	want := Config{Server: "http://x:8080", Token: "lcr_abc"}
	if err := saveConfig(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("want error for missing config")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/client/ ./internal/cli/ -v`
Expected: FAIL — `undefined: New`, `undefined: Config`

- [ ] **Step 3: Write minimal implementation**

`internal/client/client.go`:

```go
// Package client is the Go client for the luncur REST API, used by the CLI.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

type UserInfo struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

func New(server, token string) *Client {
	return &Client{
		base:  strings.TrimRight(server, "/"),
		token: token,
		http:  &http.Client{},
	}
}

// do sends a JSON request and decodes a JSON response. Non-2xx responses
// are turned into errors carrying the envelope's message and code.
func (c *Client) do(method, path string, in, out any) error {
	var body *bytes.Buffer = bytes.NewBuffer(nil)
	if in != nil {
		if err := json.NewEncoder(body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error.Code != "" {
			return fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Login(email, password string) (string, error) {
	var out struct{ Token string `json:"token"` }
	err := c.do("POST", "/v1/login",
		map[string]string{"email": email, "password": password}, &out)
	return out.Token, err
}

func (c *Client) Me() (UserInfo, error) {
	var out UserInfo
	err := c.do("GET", "/v1/me", nil, &out)
	return out, err
}

func (c *Client) CreateUser(email, password, role string) (UserInfo, error) {
	var out UserInfo
	err := c.do("POST", "/v1/users",
		map[string]string{"email": email, "password": password, "role": role}, &out)
	return out, err
}
```

`internal/cli/config.go`:

```go
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() (string, error) {
	if p := os.Getenv("LUNCUR_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "luncur", "config.json"), nil
}

func loadConfig() (Config, error) {
	var c Config
	p, err := configPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func saveConfig(c Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/client/ ./internal/cli/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/client internal/cli
git commit -m "feat: REST client and CLI config persistence"
```

---

### Task 9: `luncur login`, `luncur whoami`, `luncur user add`

**Files:**
- Create: `internal/cli/login.go`
- Create: `internal/cli/whoami.go`
- Create: `internal/cli/user.go`
- Modify: `internal/cli/root.go` (register commands)
- Test: `internal/cli/commands_test.go`

**Interfaces:**
- Consumes: `client.Client` (Task 8), `loadConfig`/`saveConfig` (Task 8).
- Produces:
  - `luncur login <server-url> --email <e> --password <p>` — flags optional; when omitted, prompt interactively (password via x/term, no echo). Saves config on success.
  - `luncur whoami` — prints `email (role)`.
  - `luncur user add <email> --role member|admin --password <p>` — admin only (server enforces).

- [ ] **Step 1: Get x/term**

```bash
go get golang.org/x/term@latest
```

- [ ] **Step 2: Write the failing test**

`internal/cli/commands_test.go`:

```go
package cli

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/server"
	"github.com/sutantodadang/luncur/internal/store"
)

func testEnv(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestLoginWhoamiUserAdd(t *testing.T) {
	srv := testEnv(t)

	out, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456")
	if err != nil {
		t.Fatalf("login: %v (%s)", err, out)
	}
	if !strings.Contains(out, "logged in") {
		t.Fatalf("want 'logged in', got %q", out)
	}

	out, err = run(t, "whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(out, "root@b.co (admin)") {
		t.Fatalf("want identity line, got %q", out)
	}

	out, err = run(t, "user", "add", "new@b.co", "--role", "member", "--password", "pw123456")
	if err != nil {
		t.Fatalf("user add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "new@b.co") {
		t.Fatalf("want created email in output, got %q", out)
	}
}

func TestWhoamiWithoutLogin(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if _, err := run(t, "whoami"); err == nil {
		t.Fatal("want error when not logged in")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/cli/ -run 'Login|Whoami' -v`
Expected: FAIL — `unknown command "login"`

- [ ] **Step 4: Write minimal implementation**

`internal/cli/login.go`:

```go
package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sutantodadang/luncur/internal/client"
)

func loginCmd() *cobra.Command {
	var email, password string
	cmd := &cobra.Command{
		Use:   "login <server-url>",
		Short: "Authenticate against a luncur server and store the token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			serverURL := args[0]
			if email == "" {
				fmt.Fprint(cmd.OutOrStdout(), "email: ")
				r := bufio.NewReader(cmd.InOrStdin())
				line, err := r.ReadString('\n')
				if err != nil {
					return err
				}
				email = strings.TrimSpace(line)
			}
			if password == "" {
				fmt.Fprint(cmd.OutOrStdout(), "password: ")
				b, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(cmd.OutOrStdout())
				if err != nil {
					return err
				}
				password = string(b)
			}
			tok, err := client.New(serverURL, "").Login(email, password)
			if err != nil {
				return err
			}
			if err := saveConfig(Config{Server: serverURL, Token: tok}); err != nil {
				return err
			}
			cmd.Printf("logged in to %s as %s\n", serverURL, email)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email (prompted if omitted)")
	cmd.Flags().StringVar(&password, "password", "", "password (prompted if omitted)")
	return cmd
}

// apiClient loads the saved config and returns a ready client.
func apiClient() (*client.Client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("not logged in — run `luncur login <server-url>` first")
	}
	return client.New(cfg.Server, cfg.Token), nil
}
```

`internal/cli/whoami.go`:

```go
package cli

import "github.com/spf13/cobra"

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the logged-in user",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			me, err := c.Me()
			if err != nil {
				return err
			}
			cmd.Printf("%s (%s)\n", me.Email, me.Role)
			return nil
		},
	}
}
```

`internal/cli/user.go`:

```go
package cli

import "github.com/spf13/cobra"

func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users (admin only)",
	}

	var role, password string
	add := &cobra.Command{
		Use:   "add <email>",
		Short: "Create a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			u, err := c.CreateUser(args[0], password, role)
			if err != nil {
				return err
			}
			cmd.Printf("created %s (%s)\n", u.Email, u.Role)
			return nil
		},
	}
	add.Flags().StringVar(&role, "role", "member", "role: admin or member")
	add.Flags().StringVar(&password, "password", "", "initial password")
	add.MarkFlagRequired("password")

	cmd.AddCommand(add)
	return cmd
}
```

In `internal/cli/root.go`, inside `newRoot()` alongside `serveCmd()`:

```go
	root.AddCommand(loginCmd())
	root.AddCommand(whoamiCmd())
	root.AddCommand(userCmd())
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./... -v` → PASS (all packages)
Run: `CGO_ENABLED=0 go build ./...` → exit 0
Run: `go vet ./...` → clean

- [ ] **Step 6: Commit**

```bash
git add internal/cli go.mod go.sum
git commit -m "feat: login, whoami, and user add CLI commands"
```

---

### Task 10: End-to-end smoke check + README stub

**Files:**
- Create: `README.md`
- Create: `.gitignore`

**Interfaces:**
- Consumes: everything above.
- Produces: documented quickstart matching real behavior.

- [ ] **Step 1: Manual end-to-end verification**

```bash
go build -o luncur.exe ./cmd/luncur
./luncur.exe serve --db ./tmp-e2e.db --listen 127.0.0.1:18080 --bootstrap-admin root@local:changeme123 &
sleep 1
./luncur.exe login http://127.0.0.1:18080 --email root@local --password changeme123
./luncur.exe whoami                     # → root@local (admin)
./luncur.exe user add dev@local --password devpass123
kill %1 && rm -f ./tmp-e2e.db* ./luncur.exe
```

Expected: each command exits 0 with the outputs shown in Task 9's tests.

- [ ] **Step 2: Write README + .gitignore**

`.gitignore`:

```
luncur
luncur.exe
*.db
*.db-wal
*.db-shm
tmp-*
```

`README.md`:

```markdown
# luncur

Tiny self-hosted PaaS on K3s. One Go binary, SQLite, deploys as simple as
Heroku — with an escape hatch to the real Kubernetes objects.

Status: Phase 1 in progress. Working today:

```sh
luncur serve --db luncur.db --bootstrap-admin you@example.com:yourpassword
luncur login http://localhost:8080
luncur whoami
luncur user add teammate@example.com --password ...
```

Design docs: `docs/superpowers/specs/`. Plans: `docs/superpowers/plans/`.
```

- [ ] **Step 3: Commit**

```bash
git add README.md .gitignore
git commit -m "docs: README quickstart and gitignore"
```
