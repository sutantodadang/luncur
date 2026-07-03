# luncur Plan B1 — App Model, Sealer, Manifest Renderer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Everything Plan B2's K8s applier and API need, with zero cluster dependency: project/app/env/override/deployment store queries, AES-GCM secret sealing, and the manifest renderer that turns an app model + user overrides into final Deployment/Service/Ingress/Secret JSON.

**Architecture:** `internal/store` grows typed queries per entity (one file each). New `internal/secret` seals env values at rest (AES-GCM, key file). New `internal/render` builds typed k8s objects (k8s.io/api structs), marshals to JSON, applies stored strategic-merge-patch overrides, and emits multi-doc YAML for `--raw`. Renderer is pure — no client-go until Plan B2.

**Tech Stack:** existing stack + `k8s.io/api`, `k8s.io/apimachinery` (structs + strategicpatch), `sigs.k8s.io/yaml`.

**Plan sequence (Phase 1):** A: core (merged) · **B1: model + renderer (this plan)** · B2: K8s apply + API + CLI · C: build pipeline + registry · D: web UI + `luncur up`.

## Global Constraints

- Everything from Plan A still binds (no CGO, single binary, error envelope, `t.TempDir()` tests).
- New dependencies allowed by this plan, and only these: `k8s.io/api`, `k8s.io/apimachinery`, `sigs.k8s.io/yaml`. NOT client-go (that is B2).
- All k8s objects carry labels `app.kubernetes.io/name: <app>` and `app.kubernetes.io/managed-by: luncur`. Deployment selector uses ONLY `app.kubernetes.io/name`.
- Object names: Deployment/Service/Ingress = app name; Secret = `<app>-env`.
- TypeMeta (apiVersion/kind) must be set explicitly on every rendered object.
- Project name = DNS-1123 label, 1–40 chars: regex `^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`. Namespace = `luncur-<name>`. App name: same regex.
- Overrides are strategic-merge-patch JSON, stored per (app, Kind), applied at render time.
- `store.ErrNotFound` is the shared sentinel for all missing-row lookups in this plan.

## File Structure

```
internal/secret/secret.go        AES-GCM Sealer: New, LoadOrCreate, Seal, Open
internal/store/projects.go       Project CRUD
internal/store/apps.go           App CRUD + SetReplicas
internal/store/deployments.go    deployment rows: create, set status, latest
internal/store/env.go            sealed env upsert/unset/list
internal/store/overrides.go      override upsert/list/delete
internal/render/render.go        Input, Render, YAML — typed objects + override merge
```

---

### Task 1: Secret sealer (AES-GCM)

**Files:**
- Create: `internal/secret/secret.go`
- Test: `internal/secret/secret_test.go`

**Interfaces:**
- Produces:
  - `secret.New(key []byte) (*Sealer, error)` — key must be 32 bytes.
  - `secret.LoadOrCreate(path string) (*Sealer, error)` — reads 64-hex-char key file; if missing, generates one, writes it `0600`, and uses it.
  - `(*Sealer) Seal(plain []byte) ([]byte, error)` — returns `nonce||ciphertext`.
  - `(*Sealer) Open(box []byte) ([]byte, error)` — inverse; error on tamper/short input.

- [ ] **Step 1: Write the failing test**

`internal/secret/secret_test.go`:

```go
package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	s, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.Seal([]byte("DATABASE_URL=postgres://x"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(box, []byte("postgres")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := s.Open(box)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DATABASE_URL=postgres://x" {
		t.Fatalf("round trip: %q", got)
	}
	// Tamper detection.
	box[len(box)-1] ^= 0xff
	if _, err := s.Open(box); err == nil {
		t.Fatal("want error on tampered box")
	}
	// Short input.
	if _, err := s.Open([]byte{1, 2}); err == nil {
		t.Fatal("want error on short box")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("want error for non-32-byte key")
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	s1, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	box, err := s1.Seal([]byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	// Second load reads the same key and can open the first sealer's box.
	s2, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Open(box); err != nil || string(got) != "v" {
		t.Fatalf("reload open: %q %v", got, err)
	}
	if b, _ := os.ReadFile(path); len(b) != 64 {
		t.Fatalf("key file should be 64 hex chars, got %d", len(b))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/secret/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write minimal implementation**

`internal/secret/secret.go`:

```go
// Package secret seals small values (env vars, git tokens) at rest with
// AES-256-GCM. The key lives in a file created at first boot.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

type Sealer struct {
	aead cipher.AEAD
}

func New(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// LoadOrCreate reads a 64-hex-char key file, generating it (0600) if absent.
func LoadOrCreate(path string) (*Sealer, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)), 0o600); err != nil {
			return nil, err
		}
		return New(key)
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(string(b))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return New(key)
}

func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, nil), nil
}

func (s *Sealer) Open(box []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(box) < ns {
		return nil, errors.New("sealed value too short")
	}
	return s.aead.Open(nil, box[:ns], box[ns:], nil)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/secret/ -v` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/secret
git commit -m "feat: AES-GCM sealer for secrets at rest"
```

---

### Task 2: Store — projects

**Files:**
- Create: `internal/store/projects.go`
- Test: `internal/store/projects_test.go`

**Interfaces:**
- Produces:
  - `var ErrNotFound = errors.New("not found")` (package store — shared by later tasks)
  - `type Project struct { ID int64; Name, Namespace string }`
  - `(*Store) CreateProject(name string) (Project, error)` — validates DNS-1123 regex from Global Constraints; namespace `luncur-<name>`.
  - `(*Store) GetProject(name string) (Project, error)` — ErrNotFound when missing.
  - `(*Store) ListProjects() ([]Project, error)`
  - `validName(s string) bool` (package-private, reused by apps task)

- [ ] **Step 1: Write the failing test**

`internal/store/projects_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestProjectCRUD(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "luncur-web" || p.ID == 0 {
		t.Fatalf("bad project: %+v", p)
	}

	got, err := s.GetProject("web")
	if err != nil || got.ID != p.ID {
		t.Fatalf("get: %+v %v", got, err)
	}

	if _, err := s.GetProject("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	if _, err := s.CreateProject("web"); err == nil {
		t.Fatal("want duplicate name error")
	}

	list, err := s.ListProjects()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
}

func TestCreateProjectValidatesName(t *testing.T) {
	s := openTest(t)
	for _, bad := range []string{"", "-x", "x-", "UPPER", "has_underscore", "waaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaytoolong"} {
		if _, err := s.CreateProject(bad); err == nil {
			t.Errorf("name %q: want error", bad)
		}
	}
	for _, good := range []string{"a", "web-1", "my-app"} {
		if _, err := s.CreateProject(good); err != nil {
			t.Errorf("name %q: %v", good, err)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run Project -v`
Expected: FAIL — `undefined: ErrNotFound` / `s.CreateProject undefined`

- [ ] **Step 3: Write minimal implementation**

`internal/store/projects.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

var ErrNotFound = errors.New("not found")

// validName enforces a DNS-1123 label (1-40 chars) so names can become
// Kubernetes namespaces, object names, and hostnames unmodified.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

func validName(s string) bool { return nameRe.MatchString(s) }

type Project struct {
	ID        int64
	Name      string
	Namespace string
}

func (s *Store) CreateProject(name string) (Project, error) {
	if !validName(name) {
		return Project{}, fmt.Errorf("invalid project name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	ns := "luncur-" + name
	res, err := s.db.Exec(`INSERT INTO projects (name, k8s_namespace) VALUES (?, ?)`, name, ns)
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Project{}, err
	}
	return Project{ID: id, Name: name, Namespace: ns}, nil
}

func (s *Store) GetProject(name string) (Project, error) {
	var p Project
	err := s.db.QueryRow(
		`SELECT id, name, k8s_namespace FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &p.Namespace)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id, name, k8s_namespace FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Namespace); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: project store with DNS-1123 name validation"
```

---

### Task 3: Store — apps + deployments

**Files:**
- Create: `internal/store/apps.go`
- Create: `internal/store/deployments.go`
- Test: `internal/store/apps_test.go`

**Interfaces:**
- Consumes: `validName`, `ErrNotFound` (Task 2).
- Produces:
  - `type App struct { ID, ProjectID int64; Name string; Port, Replicas int }`
  - `(*Store) CreateApp(projectID int64, name string, port int) (App, error)` — validates name (validName) and port (1–65535); source_type `'tarball'`, replicas default 1 (DB defaults).
  - `(*Store) GetApp(projectID int64, name string) (App, error)`
  - `(*Store) ListApps(projectID int64) ([]App, error)`
  - `(*Store) DeleteApp(id int64) error` — ErrNotFound if no row deleted.
  - `(*Store) SetReplicas(id int64, n int) error` — validates 0–20.
  - `type Deployment struct { ID, AppID int64; Status, ImageRef, CreatedAt string }`
  - `(*Store) CreateDeployment(appID int64, status, imageRef string) (Deployment, error)`
  - `(*Store) SetDeploymentStatus(id int64, status string) error`
  - `(*Store) LatestDeployment(appID int64) (Deployment, error)` — ErrNotFound when none.

- [ ] **Step 1: Write the failing test**

`internal/store/apps_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func seedProject(t *testing.T, s *Store) Project {
	t.Helper()
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAppCRUD(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)

	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if a.Port != 3000 || a.Replicas != 1 {
		t.Fatalf("bad app defaults: %+v", a)
	}

	if _, err := s.CreateApp(p.ID, "api", 3000); err == nil {
		t.Fatal("want duplicate app name error")
	}
	if _, err := s.CreateApp(p.ID, "Bad_Name", 3000); err == nil {
		t.Fatal("want invalid name error")
	}
	if _, err := s.CreateApp(p.ID, "ok", 0); err == nil {
		t.Fatal("want invalid port error")
	}

	if err := s.SetReplicas(a.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "api")
	if err != nil || got.Replicas != 3 {
		t.Fatalf("get after scale: %+v %v", got, err)
	}

	list, err := s.ListApps(p.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	if err := s.DeleteApp(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetApp(p.ID, "api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteApp(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestDeployments(t *testing.T) {
	s := openTest(t)
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.LatestDeployment(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound with no deployments, got %v", err)
	}

	d1, err := s.CreateDeployment(a.ID, "deploying", "registry/x:1")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDeployment(a.ID, "deploying", "registry/x:2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeploymentStatus(d2.ID, "live"); err != nil {
		t.Fatal(err)
	}

	latest, err := s.LatestDeployment(a.ID)
	if err != nil || latest.ID != d2.ID || latest.Status != "live" {
		t.Fatalf("latest: %+v %v (d1=%d d2=%d)", latest, err, d1.ID, d2.ID)
	}

	if _, err := s.CreateDeployment(a.ID, "bogus", "x"); err == nil {
		t.Fatal("want CHECK violation for bogus status")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'App|Deployments' -v`
Expected: FAIL — `s.CreateApp undefined`

- [ ] **Step 3: Write minimal implementation**

`internal/store/apps.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type App struct {
	ID        int64
	ProjectID int64
	Name      string
	Port      int
	Replicas  int
}

func (s *Store) CreateApp(projectID int64, name string, port int) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if port < 1 || port > 65535 {
		return App{}, fmt.Errorf("invalid port %d", port)
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, port) VALUES (?, ?, 'tarball', ?)`,
		projectID, name, port,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1}, nil
}

func (s *Store) GetApp(projectID int64, name string) (App, error) {
	var a App
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas FROM apps WHERE project_id = ? AND name = ?`,
		projectID, name,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	return a, err
}

func (s *Store) ListApps(projectID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, port, replicas FROM apps WHERE project_id = ? ORDER BY name`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteApp(id int64) error {
	res, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetReplicas(id int64, n int) error {
	if n < 0 || n > 20 {
		return fmt.Errorf("replicas must be 0-20, got %d", n)
	}
	res, err := s.db.Exec(`UPDATE apps SET replicas = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
```

`internal/store/deployments.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type Deployment struct {
	ID        int64
	AppID     int64
	Status    string
	ImageRef  string
	CreatedAt string
}

func (s *Store) CreateDeployment(appID int64, status, imageRef string) (Deployment, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, status, image_ref) VALUES (?, ?, ?)`,
		appID, status, imageRef,
	)
	if err != nil {
		return Deployment{}, fmt.Errorf("insert deployment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, AppID: appID, Status: status, ImageRef: imageRef}, nil
}

func (s *Store) SetDeploymentStatus(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE deployments SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) LatestDeployment(appID int64) (Deployment, error) {
	var d Deployment
	err := s.db.QueryRow(
		`SELECT id, app_id, status, image_ref, created_at FROM deployments
		 WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID,
	).Scan(&d.ID, &d.AppID, &d.Status, &d.ImageRef, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	return d, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: app and deployment store queries"
```

---

### Task 4: Store — env vars + overrides

**Files:**
- Create: `internal/store/env.go`
- Create: `internal/store/overrides.go`
- Test: `internal/store/env_test.go`

**Interfaces:**
- Consumes: `ErrNotFound`.
- Produces (store stays crypto-agnostic — values are opaque bytes, the server layer seals/unseals):
  - `(*Store) SetEnv(appID int64, key string, sealed []byte) error` — upsert; key must match `^[A-Z_][A-Z0-9_]*$`.
  - `(*Store) UnsetEnv(appID int64, key string) error` — ErrNotFound if absent.
  - `(*Store) ListEnv(appID int64) (map[string][]byte, error)`
  - `(*Store) SetOverride(appID int64, kind, patchJSON string) error` — upsert; kind one of Deployment|Service|Ingress; patchJSON must be valid JSON object.
  - `(*Store) Overrides(appID int64) (map[string]string, error)` — kind → patch.
  - `(*Store) DeleteOverride(appID int64, kind string) error` — ErrNotFound if absent.

- [ ] **Step 1: Write the failing test**

`internal/store/env_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func seedApp(t *testing.T, s *Store) App {
	t.Helper()
	p := seedProject(t, s)
	a, err := s.CreateApp(p.ID, "api", 3000)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestEnvVars(t *testing.T) {
	s := openTest(t)
	a := seedApp(t, s)

	if err := s.SetEnv(a.ID, "DB_URL", []byte("sealed-1")); err != nil {
		t.Fatal(err)
	}
	// Upsert overwrites.
	if err := s.SetEnv(a.ID, "DB_URL", []byte("sealed-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnv(a.ID, "lowercase", []byte("x")); err == nil {
		t.Fatal("want error for invalid key")
	}

	env, err := s.ListEnv(a.ID)
	if err != nil || len(env) != 1 || string(env["DB_URL"]) != "sealed-2" {
		t.Fatalf("list: %v %v", env, err)
	}

	if err := s.UnsetEnv(a.ID, "DB_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.UnsetEnv(a.ID, "DB_URL"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestOverrides(t *testing.T) {
	s := openTest(t)
	a := seedApp(t, s)

	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"256Mi"}}}]}}}}`
	if err := s.SetOverride(a.ID, "Deployment", patch); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces.
	if err := s.SetOverride(a.ID, "Deployment", `{"metadata":{"labels":{"x":"y"}}}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOverride(a.ID, "Pod", `{}`); err == nil {
		t.Fatal("want error for unsupported kind")
	}
	if err := s.SetOverride(a.ID, "Service", `not json`); err == nil {
		t.Fatal("want error for invalid JSON")
	}

	m, err := s.Overrides(a.ID)
	if err != nil || len(m) != 1 || m["Deployment"] != `{"metadata":{"labels":{"x":"y"}}}` {
		t.Fatalf("overrides: %v %v", m, err)
	}

	if err := s.DeleteOverride(a.ID, "Deployment"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOverride(a.ID, "Deployment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'EnvVars|Overrides' -v`
Expected: FAIL — `s.SetEnv undefined`

- [ ] **Step 3: Write minimal implementation**

`internal/store/env.go`:

```go
package store

import (
	"fmt"
	"regexp"
)

var envKeyRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// SetEnv upserts one env var. Values arrive already sealed — the store
// never sees plaintext.
func (s *Store) SetEnv(appID int64, key string, sealed []byte) error {
	if !envKeyRe.MatchString(key) {
		return fmt.Errorf("invalid env key %q (must match [A-Z_][A-Z0-9_]*)", key)
	}
	_, err := s.db.Exec(
		`INSERT INTO env_vars (app_id, key, value_enc) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, key) DO UPDATE SET value_enc = excluded.value_enc`,
		appID, key, sealed,
	)
	return err
}

func (s *Store) UnsetEnv(appID int64, key string) error {
	res, err := s.db.Exec(`DELETE FROM env_vars WHERE app_id = ? AND key = ?`, appID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListEnv(appID int64) (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT key, value_enc FROM env_vars WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
```

`internal/store/overrides.go`:

```go
package store

import (
	"encoding/json"
	"fmt"
)

var overridableKinds = map[string]bool{"Deployment": true, "Service": true, "Ingress": true}

func (s *Store) SetOverride(appID int64, kind, patchJSON string) error {
	if !overridableKinds[kind] {
		return fmt.Errorf("unsupported kind %q (Deployment, Service, or Ingress)", kind)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(patchJSON), &obj); err != nil {
		return fmt.Errorf("override patch must be a JSON object: %w", err)
	}
	_, err := s.db.Exec(
		`INSERT INTO overrides (app_id, kind, patch_json) VALUES (?, ?, ?)
		 ON CONFLICT (app_id, kind) DO UPDATE
		 SET patch_json = excluded.patch_json, updated_at = datetime('now')`,
		appID, kind, patchJSON,
	)
	return err
}

func (s *Store) Overrides(appID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT kind, patch_json FROM overrides WHERE app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, p string
		if err := rows.Scan(&k, &p); err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, rows.Err()
}

func (s *Store) DeleteOverride(appID int64, kind string) error {
	res, err := s.db.Exec(`DELETE FROM overrides WHERE app_id = ? AND kind = ?`, appID, kind)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v` → PASS (all)

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat: env var and override store queries"
```

---

### Task 5: Renderer — typed objects

**Files:**
- Create: `internal/render/render.go`
- Test: `internal/render/render_test.go`

**Interfaces:**
- Produces:
  - `type Input struct { AppName, Namespace, Image, Host string; Port, Replicas int32; Overrides map[string]string }`
  - `render.Render(in Input, env map[string]string) (Rendered, error)` — env is PLAINTEXT (server unseals before calling). Empty env → no Secret object and no envFrom.
  - `type Rendered struct { Objects []Object }` with `type Object struct { Kind string; JSON []byte }` — order: Secret (if any), Deployment, Service, Ingress.
  - `render.YAML(r Rendered) ([]byte, error)` — `---`-separated multi-doc YAML (for `--raw`).
  - `render.SecretName(app string) string` → `<app>-env`.
- Object shape requirements (Global Constraints plus):
  - Deployment: container named `app`, image `in.Image`, containerPort `in.Port`, replicas `in.Replicas`, `envFrom` secretRef `<app>-env` only when env non-empty.
  - Service: port 80 → targetPort `in.Port`, selector `app.kubernetes.io/name: <app>`.
  - Ingress: single rule for `in.Host`, path `/` (PathType Prefix), backend service `<app>` port 80. No ingressClassName (K3s Traefik is the default class).
  - Secret: `stringData` = env map, type Opaque.
  - This task ignores `in.Overrides` (Task 6 wires them).

- [ ] **Step 1: Get deps**

```bash
go get k8s.io/api@latest k8s.io/apimachinery@latest sigs.k8s.io/yaml@latest
```

- [ ] **Step 2: Write the failing test**

`internal/render/render_test.go`:

```go
package render

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
)

func testInput() Input {
	return Input{
		AppName:   "api",
		Namespace: "luncur-proj",
		Image:     "registry.luncur-system:5000/api:42",
		Host:      "api.203-0-113-7.sslip.io",
		Port:      3000,
		Replicas:  2,
	}
}

func mustRender(t *testing.T, in Input, env map[string]string) Rendered {
	t.Helper()
	r, err := Render(in, env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func objByKind(t *testing.T, r Rendered, kind string) []byte {
	t.Helper()
	for _, o := range r.Objects {
		if o.Kind == kind {
			return o.JSON
		}
	}
	t.Fatalf("no %s in rendered objects", kind)
	return nil
}

func TestRenderDeployment(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"K": "v"})
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	if d.APIVersion != "apps/v1" || d.Kind != "Deployment" {
		t.Fatalf("TypeMeta missing: %s/%s", d.APIVersion, d.Kind)
	}
	if d.Name != "api" || d.Namespace != "luncur-proj" {
		t.Fatalf("meta: %s/%s", d.Namespace, d.Name)
	}
	if *d.Spec.Replicas != 2 {
		t.Fatalf("replicas: %d", *d.Spec.Replicas)
	}
	if d.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "api" {
		t.Fatalf("selector: %v", d.Spec.Selector.MatchLabels)
	}
	if d.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("labels: %v", d.Labels)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Name != "app" || c.Image != "registry.luncur-system:5000/api:42" || c.Ports[0].ContainerPort != 3000 {
		t.Fatalf("container: %+v", c)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "api-env" {
		t.Fatalf("envFrom: %+v", c.EnvFrom)
	}
}

func TestRenderNoEnvMeansNoSecret(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	if len(r.Objects) != 3 {
		t.Fatalf("want 3 objects without env, got %d", len(r.Objects))
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if len(d.Spec.Template.Spec.Containers[0].EnvFrom) != 0 {
		t.Fatal("envFrom should be absent without env vars")
	}
}

func TestRenderServiceAndIngress(t *testing.T) {
	r := mustRender(t, testInput(), nil)

	var svc corev1.Service
	json.Unmarshal(objByKind(t, r, "Service"), &svc)
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 3000 {
		t.Fatalf("service ports: %+v", svc.Spec.Ports)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != "api" {
		t.Fatalf("service selector: %v", svc.Spec.Selector)
	}

	var ing netv1.Ingress
	json.Unmarshal(objByKind(t, r, "Ingress"), &ing)
	rule := ing.Spec.Rules[0]
	if rule.Host != "api.203-0-113-7.sslip.io" {
		t.Fatalf("host: %s", rule.Host)
	}
	path := rule.HTTP.Paths[0]
	if path.Backend.Service.Name != "api" || path.Backend.Service.Port.Number != 80 {
		t.Fatalf("backend: %+v", path.Backend)
	}
}

func TestRenderSecret(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"A": "1", "B": "2"})
	if len(r.Objects) != 4 || r.Objects[0].Kind != "Secret" {
		t.Fatalf("want Secret first of 4, got %+v", r.Objects)
	}
	var sec corev1.Secret
	json.Unmarshal(r.Objects[0].JSON, &sec)
	if sec.Name != "api-env" || sec.StringData["A"] != "1" || sec.StringData["B"] != "2" {
		t.Fatalf("secret: %+v", sec)
	}
}

func TestYAMLMultiDoc(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"A": "1"})
	y, err := YAML(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if strings.Count(s, "\n---\n") != 3 {
		t.Fatalf("want 3 separators for 4 docs, got:\n%s", s)
	}
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: Ingress", "kind: Secret"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in YAML", want)
		}
	}
}

func TestRenderValidatesInput(t *testing.T) {
	in := testInput()
	in.Image = ""
	if _, err := Render(in, nil); err == nil {
		t.Fatal("want error for empty image")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/render/ -v`
Expected: FAIL — `undefined: Input`

- [ ] **Step 4: Write minimal implementation**

`internal/render/render.go`:

```go
// Package render turns luncur's app model into Kubernetes manifests.
// Objects are rendered from the model, then per-kind user overrides
// (strategic merge patches) are applied — so user customizations survive
// every redeploy. Pure package: no cluster access.
package render

import (
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

type Input struct {
	AppName   string
	Namespace string
	Image     string
	Host      string
	Port      int32
	Replicas  int32
	// Overrides maps Kind -> strategic-merge-patch JSON. Applied by Task 6.
	Overrides map[string]string
}

type Object struct {
	Kind string
	JSON []byte
}

type Rendered struct {
	Objects []Object
}

func SecretName(app string) string { return app + "-env" }

func labels(app string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       app,
		"app.kubernetes.io/managed-by": "luncur",
	}
}

func selector(app string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": app}
}

func meta(in Input, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: in.Namespace, Labels: labels(in.AppName)}
}

// Render builds the manifest set for one app. env is plaintext (the caller
// unseals); empty env omits the Secret entirely.
func Render(in Input, env map[string]string) (Rendered, error) {
	if in.AppName == "" || in.Namespace == "" || in.Image == "" || in.Host == "" || in.Port < 1 {
		return Rendered{}, fmt.Errorf("render: AppName, Namespace, Image, Host, and Port are required")
	}

	var objs []Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, Object{Kind: kind, JSON: b})
		return nil
	}

	if len(env) > 0 {
		sec := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: meta(in, SecretName(in.AppName)),
			Type:       corev1.SecretTypeOpaque,
			StringData: env,
		}
		if err := add("Secret", sec); err != nil {
			return Rendered{}, err
		}
	}

	container := corev1.Container{
		Name:  "app",
		Image: in.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: in.Port}},
	}
	if len(env) > 0 {
		container.EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName(in.AppName)},
			},
		}}
	}
	replicas := in.Replicas
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: meta(in, in.AppName),
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector(in.AppName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(in.AppName)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}
	if err := add("Deployment", dep); err != nil {
		return Rendered{}, err
	}

	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta(in, in.AppName),
		Spec: corev1.ServiceSpec{
			Selector: selector(in.AppName),
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(in.Port),
			}},
		},
	}
	if err := add("Service", svc); err != nil {
		return Rendered{}, err
	}

	pathType := netv1.PathTypePrefix
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: meta(in, in.AppName),
		Spec: netv1.IngressSpec{
			Rules: []netv1.IngressRule{{
				Host: in.Host,
				IngressRuleValue: netv1.IngressRuleValue{
					HTTP: &netv1.HTTPIngressRuleValue{
						Paths: []netv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: netv1.IngressBackend{
								Service: &netv1.IngressServiceBackend{
									Name: in.AppName,
									Port: netv1.ServiceBackendPort{Number: 80},
								},
							},
						}},
					},
				},
			}},
		},
	}
	if err := add("Ingress", ing); err != nil {
		return Rendered{}, err
	}

	return Rendered{Objects: objs}, nil
}

// YAML renders the object set as ----separated multi-doc YAML (for --raw).
func YAML(r Rendered) ([]byte, error) {
	var out []byte
	for i, o := range r.Objects {
		y, err := yaml.JSONToYAML(o.JSON)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, []byte("---\n")...)
		}
		out = append(out, y...)
	}
	return out, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/render/ -v` → PASS
Run: `go mod tidy && CGO_ENABLED=0 go build ./...` → exit 0

- [ ] **Step 6: Commit**

```bash
git add internal/render go.mod go.sum
git commit -m "feat: manifest renderer for Deployment/Service/Ingress/Secret"
```

---

### Task 6: Renderer — override merge

**Files:**
- Modify: `internal/render/render.go` (wire `in.Overrides` into `Render`)
- Test: `internal/render/override_test.go`

**Interfaces:**
- Consumes: Task 5's `Render`.
- Produces: `Render` now applies `in.Overrides[kind]` as a strategic merge patch to the matching rendered object, using `k8s.io/apimachinery/pkg/util/strategicpatch.StrategicMergePatch(original, patch, dataStruct)` with the typed zero value (`appsv1.Deployment{}`, `corev1.Service{}`, `netv1.Ingress{}`) as dataStruct. Secret is never overridable. Unknown kinds in the map → error.

- [ ] **Step 1: Write the failing test**

`internal/render/override_test.go`:

```go
package render

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

func TestOverrideMergesIntoDeployment(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{
		"Deployment": `{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":{"limits":{"memory":"256Mi"}}}]}}}}`,
	}
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	// Strategic merge: patch by container name merges INTO the container,
	// preserving image/ports while adding resources.
	if c.Image != in.Image {
		t.Fatalf("image lost in merge: %q", c.Image)
	}
	if got := c.Resources.Limits.Memory().String(); got != "256Mi" {
		t.Fatalf("memory limit: %s", got)
	}
}

func TestOverrideBaseRenderStillWinsElsewhere(t *testing.T) {
	// An override set when replicas was 2 must not pin replicas after the
	// model changes — only fields the patch names are overridden.
	in := testInput()
	in.Overrides = map[string]string{"Deployment": `{"metadata":{"labels":{"team":"x"}}}`}
	in.Replicas = 5
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if *d.Spec.Replicas != 5 {
		t.Fatalf("replicas: %d", *d.Spec.Replicas)
	}
	if d.Labels["team"] != "x" || d.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("labels: %v", d.Labels)
	}
}

func TestOverrideUnknownKindErrors(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{"Secret": `{}`}
	if _, err := Render(in, map[string]string{"A": "1"}); err == nil {
		t.Fatal("want error for Secret override")
	}
}

func TestOverrideInvalidPatchErrors(t *testing.T) {
	in := testInput()
	in.Overrides = map[string]string{"Deployment": `{"spec":{"replicas":"not-a-number"}}`}
	if _, err := Render(in, nil); err == nil {
		t.Fatal("want error for type-mismatched patch")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/render/ -run Override -v`
Expected: FAIL — overrides ignored (memory limit missing) and unknown kind accepted.

- [ ] **Step 3: Implement**

In `internal/render/render.go`:

Add import `"k8s.io/apimachinery/pkg/util/strategicpatch"`.

Add after the type declarations:

```go
// dataStructFor returns the typed zero value strategicpatch needs to
// understand list-merge keys (e.g. containers merged by name).
func dataStructFor(kind string) (any, error) {
	switch kind {
	case "Deployment":
		return appsv1.Deployment{}, nil
	case "Service":
		return corev1.Service{}, nil
	case "Ingress":
		return netv1.Ingress{}, nil
	default:
		return nil, fmt.Errorf("kind %q cannot be overridden", kind)
	}
}

func applyOverride(kind string, base []byte, patch string) ([]byte, error) {
	ds, err := dataStructFor(kind)
	if err != nil {
		return nil, err
	}
	merged, err := strategicpatch.StrategicMergePatch(base, []byte(patch), ds)
	if err != nil {
		return nil, fmt.Errorf("apply %s override: %w", kind, err)
	}
	// Round-trip through the typed struct so type mismatches fail loudly
	// at render time instead of at cluster apply time.
	typed, err := roundTrip(kind, merged)
	if err != nil {
		return nil, fmt.Errorf("%s override produces invalid object: %w", kind, err)
	}
	return typed, nil
}

func roundTrip(kind string, raw []byte) ([]byte, error) {
	var v any
	switch kind {
	case "Deployment":
		v = &appsv1.Deployment{}
	case "Service":
		v = &corev1.Service{}
	case "Ingress":
		v = &netv1.Ingress{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
```

(add `"bytes"` import). Then at the end of `Render`, before `return`:

```go
	for kind := range in.Overrides {
		if _, err := dataStructFor(kind); err != nil {
			return Rendered{}, err
		}
	}
	for i, o := range objs {
		patch, ok := in.Overrides[o.Kind]
		if !ok {
			continue
		}
		merged, err := applyOverride(o.Kind, o.JSON, patch)
		if err != nil {
			return Rendered{}, err
		}
		objs[i].JSON = merged
	}
```

Note: `TestOverrideInvalidPatchErrors` relies on the round-trip decode rejecting `"replicas":"not-a-number"`. If `StrategicMergePatch` itself already errors on it, that also passes the test — either failure point is acceptable.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/render/ -v` → PASS (all, including Task 5's)
Run: `go test ./... ` → PASS; `go vet ./...` clean

- [ ] **Step 5: Commit**

```bash
git add internal/render
git commit -m "feat: strategic-merge override application at render time"
```
