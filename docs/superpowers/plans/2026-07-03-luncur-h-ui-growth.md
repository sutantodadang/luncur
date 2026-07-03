# luncur Plan H — invites, user management UI, YAML editor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Admins invite teammates with copyable links and manage users from the web UI; anyone can edit an app's rendered YAML in the browser and have the diff stored as a redeploy-surviving override.

**Architecture:** Invites extend the existing `invites` table (single-use, 7-day expiry, role-carrying tokens) with a registration page that consumes them. User management is a thin admin page over new `ListUsers`/`DeleteUser` store methods. The YAML editor reuses the exact machinery `luncur edit` already has — `computeOverride`/`extractDoc` move from `internal/cli` into `internal/render` (exported) so the server and CLI share one implementation; the editor POST diffs submitted YAML against a fresh base render and stores the strategic-merge patch through the same override path.

**Tech Stack:** Go stdlib, `k8s.io/apimachinery/strategicpatch` + `sigs.k8s.io/yaml` (existing), modernc.org/sqlite, cobra.

## Global Constraints

- Single Go module, one binary from `cmd/luncur`. No new dependencies.
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope via `writeError`.
- All commits conventional style; `go build ./... && go vet ./... && go test ./...` before every commit.
- Tests must not require a cluster or network. CSRF (Plan G) applies to ALL new UI forms — every form carries `_csrf` (`{{.CSRF}}` / `{{$.CSRF}}`), every new POST route goes through `uiPage` or calls `checkCSRF` explicitly (register, like login, is session-less and checks CSRF itself).
- **Approved deviations from the Phase 2 spec (record in README):**
  - No email delivery (spec already says link-copy only).
  - Registration marks the invite used AFTER creating the user (non-atomic pair); the race window is a single-instance SQLite server doing two statements — acceptable, and a burned-but-unused invite cannot happen (validation failure aborts before user creation; duplicate-email failure aborts before the invite is marked).

---

### Task 1: store — invites lifecycle + user listing/deletion

**Files:**
- Modify: `internal/store/store.go` (migrate: invites columns)
- Create: `internal/store/invites.go`
- Modify: `internal/store/users.go`
- Test: `internal/store/invites_test.go`, `internal/store/users_test.go` (append)

**Interfaces:**
- Consumes: existing `invites` table (`token TEXT PRIMARY KEY, role, expires_at`), `migrate()` column loop, `ErrNotFound`, `openTest(t)`.
- Produces:
  - migrate adds: `invites.created_by INTEGER`, `invites.used_by INTEGER`, `invites.used_at TEXT`.
  - `type Invite struct { Token, Role, ExpiresAt, UsedAt string; CreatedBy, UsedBy int64 }`
  - `Store.CreateInvite(role string, createdBy int64) (Invite, error)` — role must be admin|member; token = 32-hex `crypto/rand`; expires `datetime('now', '+7 days')`.
  - `Store.ListInvites() ([]Invite, error)` — newest first (rowid DESC).
  - `Store.GetValidInvite(token string) (Invite, error)` — `ErrNotFound` unless the invite exists, is unused (`used_by IS NULL`), and unexpired.
  - `Store.MarkInviteUsed(token string, userID int64) error` — atomic guard (`WHERE token = ? AND used_by IS NULL AND expires_at > datetime('now')`); `ErrNotFound` when the guard misses.
  - `Store.RevokeInvite(token string) error` — delete; `ErrNotFound` when absent.
  - `type UserInfo struct { ID int64; Email, Role, CreatedAt string; TokenCount int64 }`
  - `Store.ListUsers() ([]UserInfo, error)` — LEFT JOIN token counts, ordered by id.
  - `Store.DeleteUser(id int64) error` — `ErrNotFound` when absent (api_tokens/ssh_keys/memberships cascade via existing FKs).

- [ ] **Step 1: Failing tests.**

`internal/store/invites_test.go`:

```go
package store

import (
	"errors"
	"testing"
)

func TestInviteLifecycle(t *testing.T) {
	s := openTest(t)
	admin, _ := s.CreateUser("admin@example.com", "password123", "admin")

	inv, err := s.CreateInvite("member", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Token) != 32 || inv.Role != "member" || inv.ExpiresAt == "" {
		t.Fatalf("invite = %+v", inv)
	}
	if _, err := s.CreateInvite("owner", admin.ID); err == nil {
		t.Fatal("bad role accepted")
	}

	got, err := s.GetValidInvite(inv.Token)
	if err != nil || got.Role != "member" {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.GetValidInvite("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}

	list, err := s.ListInvites()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	u, _ := s.CreateUser("new@example.com", "password123", "member")
	if err := s.MarkInviteUsed(inv.Token, u.ID); err != nil {
		t.Fatal(err)
	}
	// Used invites stop validating and can't be used twice.
	if _, err := s.GetValidInvite(inv.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("used invite still valid: %v", err)
	}
	if err := s.MarkInviteUsed(inv.Token, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double-use: %v", err)
	}

	// Expired invites don't validate.
	exp, _ := s.CreateInvite("member", admin.ID)
	if _, err := s.db.Exec(
		`UPDATE invites SET expires_at = datetime('now', '-1 day') WHERE token = ?`, exp.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetValidInvite(exp.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired invite valid: %v", err)
	}

	if err := s.RevokeInvite(exp.Token); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(exp.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}
```

Append to `internal/store/users_test.go`:

```go
func TestListAndDeleteUsers(t *testing.T) {
	s := openTest(t)
	a, _ := s.CreateUser("a@example.com", "password123", "admin")
	b, _ := s.CreateUser("b@example.com", "password123", "member")
	if _, err := s.CreateToken(b.ID, "t1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUsers()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if list[0].ID != a.ID || list[1].TokenCount != 1 || list[0].TokenCount != 0 {
		t.Fatalf("rows: %+v", list)
	}
	if err := s.DeleteUser(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Cascade: b's token is gone.
	if l, _ := s.ListTokens(b.ID); len(l) != 0 {
		t.Fatalf("tokens survived user delete: %+v", l)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run 'TestInviteLifecycle|TestListAndDeleteUsers' -v` — compile failure.

- [ ] **Step 3: Implement.**

migrate loop additions:

```go
	{"invites", "created_by", `ALTER TABLE invites ADD COLUMN created_by INTEGER`},
	{"invites", "used_by", `ALTER TABLE invites ADD COLUMN used_by INTEGER`},
	{"invites", "used_at", `ALTER TABLE invites ADD COLUMN used_at TEXT`},
```

`internal/store/invites.go`:

```go
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// Invite is a single-use, role-carrying registration token.
type Invite struct {
	Token     string
	Role      string
	ExpiresAt string
	CreatedBy int64
	UsedBy    int64
	UsedAt    string
}

// CreateInvite mints a 7-day, single-use invite.
func (s *Store) CreateInvite(role string, createdBy int64) (Invite, error) {
	if role != "admin" && role != "member" {
		return Invite{}, fmt.Errorf("invalid role %q", role)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Invite{}, err
	}
	token := hex.EncodeToString(raw)
	_, err := s.db.Exec(
		`INSERT INTO invites (token, role, expires_at, created_by)
		 VALUES (?, ?, datetime('now', '+7 days'), ?)`, token, role, createdBy)
	if err != nil {
		return Invite{}, err
	}
	var inv Invite
	err = s.db.QueryRow(
		`SELECT token, role, expires_at FROM invites WHERE token = ?`, token,
	).Scan(&inv.Token, &inv.Role, &inv.ExpiresAt)
	inv.CreatedBy = createdBy
	return inv, err
}

const inviteCols = `token, role, expires_at, COALESCE(created_by, 0), COALESCE(used_by, 0), COALESCE(used_at, '')`

func (s *Store) scanInvites(query string, args ...any) ([]Invite, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.Token, &i.Role, &i.ExpiresAt, &i.CreatedBy, &i.UsedBy, &i.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListInvites returns every invite, newest first.
func (s *Store) ListInvites() ([]Invite, error) {
	return s.scanInvites(`SELECT ` + inviteCols + ` FROM invites ORDER BY rowid DESC`)
}

// GetValidInvite returns an invite iff it exists, is unused, and unexpired.
func (s *Store) GetValidInvite(token string) (Invite, error) {
	var i Invite
	err := s.db.QueryRow(
		`SELECT `+inviteCols+` FROM invites
		 WHERE token = ? AND used_by IS NULL AND expires_at > datetime('now')`, token,
	).Scan(&i.Token, &i.Role, &i.ExpiresAt, &i.CreatedBy, &i.UsedBy, &i.UsedAt)
	if err == sql.ErrNoRows {
		return Invite{}, ErrNotFound
	}
	return i, err
}

// MarkInviteUsed burns the invite; the WHERE guard makes double-use lose.
func (s *Store) MarkInviteUsed(token string, userID int64) error {
	res, err := s.db.Exec(
		`UPDATE invites SET used_by = ?, used_at = datetime('now')
		 WHERE token = ? AND used_by IS NULL AND expires_at > datetime('now')`,
		userID, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RevokeInvite(token string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE token = ?`, token)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

`internal/store/users.go` additions:

```go
// UserInfo is the admin listing view of a user.
type UserInfo struct {
	ID         int64
	Email      string
	Role       string
	CreatedAt  string
	TokenCount int64
}

// ListUsers returns every user with their live token count.
func (s *Store) ListUsers() ([]UserInfo, error) {
	rows, err := s.db.Query(
		`SELECT u.id, u.email, u.role, u.created_at, COUNT(t.id)
		 FROM users u LEFT JOIN api_tokens t ON t.user_id = u.id
		 GROUP BY u.id ORDER BY u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.TokenCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser removes a user; tokens, ssh keys, and memberships cascade.
func (s *Store) DeleteUser(id int64) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 4: Run** `go test ./internal/store/ -v` — all pass.
- [ ] **Step 5: Commit** — `feat: invites lifecycle + user listing/deletion in store`

---

### Task 2: API + CLI — invites and users

**Files:**
- Create: `internal/server/invites.go`
- Modify: `internal/server/users.go` (list/delete handlers — check the existing file name via `ls internal/server/`; user-create lives somewhere like `users.go`; put the new handlers beside it)
- Modify: `internal/server/server.go` (routes)
- Modify: `internal/client/client.go`
- Create: `internal/cli/invite.go`
- Modify: `internal/cli/root.go`
- Test: `internal/server/invites_test.go`, `internal/cli/commands_test.go` (append)

**Interfaces:**
- Consumes: Task 1 store methods, `adminOnly`, `Client.do`, cobra patterns.
- Produces:
  - `POST /v1/invites` (admin) body `{"role":"member"}` → 201 `{"token":...,"role":...,"expires_at":...,"path":"/ui/register?token=<token>"}`.
  - `GET /v1/invites` (admin) → list (token, role, expires_at, used flag via `used_by != 0`).
  - `DELETE /v1/invites/{token}` (admin) → 204 / 404.
  - `GET /v1/users` (admin) → `[{"id","email","role","created_at","token_count"}]`.
  - `DELETE /v1/users/{id}` (admin) → 204; 400 `bad_request` when deleting yourself; 404 unknown.
  - Client: `CreateInvite(role string) (InviteInfo, error)`, `ListInvites() ([]InviteInfo, error)`, `RevokeInvite(token string) error` with `type InviteInfo struct { Token string \`json:"token"\`; Role string \`json:"role"\`; ExpiresAt string \`json:"expires_at"\`; Path string \`json:"path"\`; Used bool \`json:"used"\` }`.
  - CLI: `luncur invite create [--role member]` — prints the full URL (client base URL + path); `invite list` (tabwriter TOKEN/ROLE/EXPIRES/USED); `invite revoke <token>`.

- [ ] **Step 1: Failing tests.** `internal/server/invites_test.go` (follow `users_test.go`/`settings_test.go` fixtures): admin creates invite → 201 with 32-char token + path containing the token; member POST → 403; list shows it (used=false); revoke → 204 then list empty; `GET /v1/users` as admin lists both seeded users with token counts; member → 403; `DELETE /v1/users/{other}` → 204 and the deleted user's token stops working; `DELETE /v1/users/{self}` → 400. Append a CLI test `TestInviteCommands` to `commands_test.go`: `invite create` output contains `/ui/register?token=`; `invite list` contains the token; `invite revoke <token>` then list no longer contains it. (Real code, package helpers.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement.** `internal/server/invites.go`:

```go
package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/sutantodadang/luncur/internal/store"
)

func inviteJSON(i store.Invite) map[string]any {
	return map[string]any{
		"token": i.Token, "role": i.Role, "expires_at": i.ExpiresAt,
		"path": "/ui/register?token=" + i.Token,
		"used": i.UsedBy != 0,
	}
}

func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	inv, err := s.st.CreateInvite(req.Role, u.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, inviteJSON(inv))
}

func (s *server) handleListInvites(w http.ResponseWriter, r *http.Request, _ store.User) {
	list, err := s.st.ListInvites()
	if err != nil {
		log.Printf("list invites: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, i := range list {
		out = append(out, inviteJSON(i))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleRevokeInvite(w http.ResponseWriter, r *http.Request, _ store.User) {
	if err := s.st.RevokeInvite(r.PathValue("token")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such invite")
		return
	} else if err != nil {
		log.Printf("revoke invite: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Users handlers (beside the existing create-user handler):

```go
func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request, _ store.User) {
	list, err := s.st.ListUsers()
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, u := range list {
		out = append(out, map[string]any{
			"id": u.ID, "email": u.Email, "role": u.Role,
			"created_at": u.CreatedAt, "token_count": u.TokenCount,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request, u store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid user id")
		return
	}
	if id == u.ID {
		writeError(w, http.StatusBadRequest, "bad_request", "cannot delete yourself")
		return
	}
	if err := s.st.DeleteUser(id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such user")
		return
	} else if err != nil {
		log.Printf("delete user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Routes (all `s.adminOnly`): `POST /v1/invites`, `GET /v1/invites`, `DELETE /v1/invites/{token}`, `GET /v1/users`, `DELETE /v1/users/{id}`.

Client methods per Interfaces (same `do` helper; `CreateInvite` POSTs `{"role":...}`). `internal/cli/invite.go` — `inviteCmd()` with `create` (flag `--role`, default member; print `c.base`-joined URL — check how the client exposes its base URL; if unexported, print the path plus the server URL from the CLI config, mirroring how `login` refers to it), `list`, `revoke <token>`, mirroring `token.go`'s shape. Register in `root.go`.

- [ ] **Step 4: Run** `go test ./internal/server/ ./internal/client/ ./internal/cli/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: invites + user admin API and CLI`

---

### Task 3: render — shared override-diff helpers

**Files:**
- Modify: `internal/render/render.go` (export helpers)
- Modify: `internal/cli/edit.go` (consume them, delete local copies)
- Test: `internal/render/render_test.go` (append; move/adapt any cli-side tests of these helpers — check `internal/cli/edit_test.go` existence first)

**Interfaces:**
- Consumes: existing unexported `dataStructFor`; the cli implementations of `extractDoc`/`computeOverride` (move verbatim).
- Produces:
  - `render.ExtractDoc(yamlMulti []byte, kind string) ([]byte, error)` — the cli `extractDoc`, exported.
  - `render.ComputeOverride(kind string, baseYAML, editedYAML []byte) (string, error)` — the cli `computeOverride`, exported, using render's own `dataStructFor`.
  - `internal/cli/edit.go` imports render, deletes its local `dataStructFor`/`extractDoc`/`computeOverride` (and their now-unused imports).

- [ ] **Step 1: Failing test** (append to `render_test.go`):

```go
func TestComputeOverrideAndExtractDoc(t *testing.T) {
	multi := []byte("kind: Service\nmetadata:\n  name: web\n---\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 1\n")
	doc, err := ExtractDoc(multi, "Deployment")
	if err != nil || !strings.Contains(string(doc), "replicas: 1") {
		t.Fatalf("doc=%s err=%v", doc, err)
	}
	if _, err := ExtractDoc(multi, "Ingress"); err == nil {
		t.Fatal("missing kind accepted")
	}
	edited := []byte("kind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: 3\n")
	patch, err := ComputeOverride("Deployment", doc, edited)
	if err != nil || !strings.Contains(patch, `"replicas":3`) {
		t.Fatalf("patch=%s err=%v", patch, err)
	}
	same, err := ComputeOverride("Deployment", doc, doc)
	if err != nil || same != "{}" {
		t.Fatalf("no-change patch = %q err=%v", same, err)
	}
}
```

- [ ] **Step 2: Run** — compile failure. Also check `ls internal/cli/` for an `edit_test.go`; if it tests `computeOverride`/`extractDoc` directly, port those cases into the render test and delete them from the cli test.

- [ ] **Step 3: Implement** — move the two functions from `internal/cli/edit.go` into `render.go` (exported, doc comments preserved; `ComputeOverride` calls render's existing `dataStructFor`), update `edit.go` call sites (`render.ExtractDoc`, `render.ComputeOverride`), drop cli's local `dataStructFor` + the `strategicpatch`/k8s-typed imports it no longer needs.

- [ ] **Step 4: Run** `go test ./internal/render/ ./internal/cli/ -v && go build ./...` — pass.
- [ ] **Step 5: Commit** — `refactor: shared override-diff helpers in render`

---

### Task 4: web UI — registration page + users admin page

**Files:**
- Modify: `internal/server/ui.go` (register + users handlers, routes, nav flag)
- Create: `internal/server/templates/register.html`, `internal/server/templates/users.html`
- Modify: `internal/server/templates/base.html` (admin nav link)
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: Task 1 store methods, `s.csrf`/`checkCSRF` (Plan G), `CreateSessionToken` + session cookie shape from `handleUILogin` (copy exactly), `adminOnly`-equivalent role check for UI (inline `u.Role != "admin"` → 404).
- Produces:
  - `GET /ui/register?token=T` — session-less. Valid token → registration form (email, password, hidden token, `_csrf`); invalid/missing → 200 page with "invite is invalid or expired" error and no form.
  - `POST /ui/register` — session-less, `checkCSRF` first; re-validate token (`GetValidInvite`); create user with the INVITE's role; `MarkInviteUsed`; mint session cookie exactly like `handleUILogin`; redirect `/ui/`. Duplicate email → re-render form with error, invite NOT burned.
  - `GET /ui/users` — admins only (non-admin → plain 404, mirroring `uiProject`'s leak-nothing convention): user table (email, role, created, token count, delete button — hidden for self) + invites section (unused invites with copyable link `<origin>/ui/register?token=...` rendered as a readonly `<input>` value + revoke button; used/expired shown structly) + create-invite form (role select).
  - `POST /ui/users/invite` (admin) — form `role`; create; redirect back.
  - `POST /ui/users/invite/revoke` (admin) — form `token`; redirect back.
  - `POST /ui/users/delete` (admin) — form `id`; self-delete → 400; redirect back.
  - `base.html` nav: `{{if .IsAdmin}}<a href="/ui/users">users</a>{{end}}` — every ui.go view-model gains `"IsAdmin": u.Role == "admin"` (login/register pages pass false implicitly by omission — guard the template with `{{if .IsAdmin}}`).

- [ ] **Step 1: Failing tests** (append to `ui_test.go`, reusing `uiCSRF`/`uiPost` helpers):

```go
func TestUIRegisterFlow(t *testing.T) {
	// admin session (existing fixture) creates an invite via the store
	// directly (st.CreateInvite("member", adminID)).
	// 1. GET /ui/register?token=<tok> → 200, body contains the email field.
	// 2. GET /ui/register?token=bogus → 200, body contains "invalid or expired", no <form... email.
	// 3. POST /ui/register (csrf-correct) email=new@x.com password=secret123
	//    token=<tok> → 303 to /ui/, Set-Cookie luncur_session.
	// 4. The invite is burned: GET /ui/register?token=<tok> → invalid page.
	// 5. New user exists with role member (st.GetUserByEmail).
}

func TestUIUsersPageAdminOnly(t *testing.T) {
	// member session GETs /ui/users → 404. Admin GETs → 200 with both
	// users' emails. Admin posts /ui/users/invite (role member) → 303;
	// page now shows /ui/register?token=. Admin posts /ui/users/delete
	// with the member's id → 303; page no longer lists the member.
	// Admin posting its own id → 400.
}
```

(Real code.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement** per Interfaces. Register handlers are session-less (`mux.HandleFunc` direct, not `uiPage`): GET renders `register.html` with `{"CSRF": s.csrf(w, r), "Token": token, "Valid": err == nil, "Role": inv.Role}`; POST: `checkCSRF` → `GetValidInvite(r.PostFormValue("token"))` (invalid → re-render error page) → `CreateUser(email, password, inv.Role)` (duplicate → re-render form + error, invite untouched) → `MarkInviteUsed` (failure → log + continue; user exists) → session cookie block copied from `handleUILogin` → redirect. Users page handlers are `uiPage`-wrapped with an admin guard helper:

```go
// uiAdmin 404s non-admins (leak-nothing, same policy as uiProject).
func (s *server) uiAdmin(w http.ResponseWriter, u store.User) bool {
	if u.Role != "admin" {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}
```

`users.html` (new, following the existing template idiom — head/nav/foot):

```html
{{define "users.html"}}
{{template "head" .}}{{template "nav" .}}
<h1>Users</h1>
<table><tr><th>Email</th><th>Role</th><th>Created</th><th>Tokens</th><th></th></tr>
{{range .Users}}<tr>
  <td>{{.Email}}</td><td>{{.Role}}</td><td>{{.CreatedAt}}</td><td>{{.TokenCount}}</td>
  <td>{{if ne .ID $.Self}}
    <form class="inline" method="post" action="/ui/users/delete">
      <input type="hidden" name="_csrf" value="{{$.CSRF}}">
      <input type="hidden" name="id" value="{{.ID}}">
      <button type="submit">delete</button>
    </form>
  {{end}}</td>
</tr>{{end}}
</table>

<h2>Invites</h2>
<table><tr><th>Link</th><th>Role</th><th>Expires</th><th>Status</th><th></th></tr>
{{range .Invites}}<tr>
  <td><input readonly size="48" value="/ui/register?token={{.Token}}"></td>
  <td>{{.Role}}</td><td>{{.ExpiresAt}}</td>
  <td>{{if .Used}}used{{else}}open{{end}}</td>
  <td>{{if not .Used}}
    <form class="inline" method="post" action="/ui/users/invite/revoke">
      <input type="hidden" name="_csrf" value="{{$.CSRF}}">
      <input type="hidden" name="token" value="{{.Token}}">
      <button type="submit">revoke</button>
    </form>
  {{end}}</td>
</tr>{{end}}
</table>
<form method="post" action="/ui/users/invite">
  <input type="hidden" name="_csrf" value="{{.CSRF}}">
  <label>role <select name="role"><option>member</option><option>admin</option></select></label>
  <button type="submit">create invite</button>
</form>
{{template "foot" .}}
{{end}}
```

(View-model: `"Users": []store.UserInfo`, `"Invites"` mapped to a small struct with `Used bool`, `"Self": u.ID`, plus User/CSRF/IsAdmin.) `register.html`:

```html
{{define "register.html"}}
{{template "head" .}}
<h1>Join luncur</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Valid}}
<p>You've been invited as <strong>{{.Role}}</strong>.</p>
<form method="post" action="/ui/register">
  <input type="hidden" name="_csrf" value="{{.CSRF}}">
  <input type="hidden" name="token" value="{{.Token}}">
  <p><label>email <input type="email" name="email" required autofocus></label></p>
  <p><label>password <input type="password" name="password" required minlength="8"></label></p>
  <p><button type="submit">create account</button></p>
</form>
{{else}}
<p class="err">This invite is invalid or expired. Ask your admin for a new one.</p>
{{end}}
{{template "foot" .}}
{{end}}
```

Routes: `GET /ui/register`, `POST /ui/register` (direct), `GET /ui/users`, `POST /ui/users/invite`, `POST /ui/users/invite/revoke`, `POST /ui/users/delete` (uiPage + uiAdmin). Nav link in `base.html` guarded by `{{if .IsAdmin}}`; add `"IsAdmin"` to every ui.go view-model (uiPage handlers have `u`; login/register maps just omit it).

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: web UI registration via invites + user admin page`

---

### Task 5: web UI — YAML override editor

**Files:**
- Modify: `internal/server/ui.go` (editor handlers + routes)
- Create: `internal/server/templates/edit.html`
- Modify: `internal/server/templates/app.html` (edit links)
- Modify: `internal/server/appenv.go` (extract shared override-set core — read `handleSetOverride` at `internal/server/appenv.go:151` first and mirror its validation exactly)
- Test: `internal/server/ui_test.go` (append)

**Interfaces:**
- Consumes: `render.ExtractDoc`/`render.ComputeOverride` (Task 3), `s.renderApp(p, a, image, withOverrides)` + `render.YAML`, `handleSetOverride`'s validation core, `s.st.LatestDeployment` (image for rendering — `handleRawManifest` shows how the raw endpoint picks the image; mirror it), `syncIfLive`.
- Produces:
  - Extracted `s.setOverride(ctx context.Context, p store.Project, a store.App, kind, patch string) error` — whatever `handleSetOverride` does between auth and response (store write + validation + sync), shared by API and UI.
  - `GET /ui/projects/{project}/apps/{app}/edit/{kind}` — kind ∈ Deployment|Service|Ingress (else 404). Renders `edit.html` with the CURRENT rendered doc (with overrides) in a `<textarea>`.
  - `POST /ui/projects/{project}/apps/{app}/edit/{kind}` — re-render base (withOverrides=false), `ExtractDoc`, `ComputeOverride(kind, baseDoc, submitted)`; patch `"{}"` → redirect back to app page unchanged; else `s.setOverride` → redirect to app page. Any error (bad YAML, invalid patch) → re-render `edit.html` with the user's text + error message (never lose their edit).
  - `app.html`: next to "raw YAML" link — `edit: <a .../edit/Deployment>Deployment</a> · <a .../edit/Service>Service</a> · <a .../edit/Ingress>Ingress</a>`.

- [ ] **Step 1: Failing test** (append to `ui_test.go`, fixture with a live deployment + fake kube):

```go
func TestUIYAMLEditor(t *testing.T) {
	// 1. GET /ui/projects/p/apps/web/edit/Deployment → 200, textarea
	//    contains "kind: Deployment" and "replicas: 1".
	// 2. POST same path (csrf-correct) with the yaml field = the GET's
	//    textarea content with "replicas: 1" → "replicas: 4" → 303 to the
	//    app page; store override for kind Deployment now contains
	//    `"replicas":4` (st.Overrides(appID)).
	// 3. POST with yaml "not: [valid" → 200 re-rendered editor containing
	//    the error and the submitted text.
	// 4. GET /ui/.../edit/ConfigMap → 404.
}
```

(Extract the textarea content in the test with a simple substring split on `<textarea` / `</textarea>` + html.UnescapeString.)

- [ ] **Step 2: Run** — failures.

- [ ] **Step 3: Implement** per Interfaces. Editor GET builds the doc the same way `handleRawManifest` does (latest live image or placeholder — read that handler and reuse its image-pick logic via a tiny shared helper if it's more than two lines), then `render.YAML` → `render.ExtractDoc(raw, kind)`. `edit.html`:

```html
{{define "edit.html"}}
{{template "head" .}}{{template "nav" .}}
<p><a href="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}">&larr; {{.App.Name}}</a></p>
<h1>Edit {{.Kind}} — {{.App.Name}}</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<p>Changes are stored as an override patch and survive redeploys.</p>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/edit/{{.Kind}}">
  <input type="hidden" name="_csrf" value="{{.CSRF}}">
  <p><textarea name="yaml" rows="30" style="width:100%;font-family:monospace">{{.YAML}}</textarea></p>
  <p><button type="submit">save override</button></p>
</form>
{{template "foot" .}}
{{end}}
```

Handlers `uiPage`-wrapped; `editableKinds := map[string]bool{"Deployment": true, "Service": true, "Ingress": true}`.

- [ ] **Step 4: Run** `go test ./internal/server/ -v` — pass.
- [ ] **Step 5: Commit** — `feat: web UI YAML override editor`

---

### Task 6: README + final verification

**Files:**
- Modify: `README.md`
- Test: none new.

- [ ] **Step 1: README** — Web UI section gains: invite links + registration, user management page, YAML editor (with the redeploy-surviving override note); Auth section mentions `luncur invite create/list/revoke`; deviations list gains Plan H's non-atomic invite-burn note; status line becomes "Phase 2 complete (Plans E-H)".
- [ ] **Step 2: Run** `go build ./... && go vet ./... && go test ./...` — green. `grep -rn "Plan H" README.md internal/` — only intentional mentions.
- [ ] **Step 3: Commit** — `docs: phase 2 complete — invites, user admin, YAML editor`

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test ./...` — everything green; `gofmt -l internal/ cmd/` clean (pre-existing client.go flag exempt).
- [ ] Push branch `plan-h`, open PR against `main`.
- [ ] Manual (owner's VPS, post-merge): create invite in UI → open link in second browser → register → member sees only their projects; edit Deployment YAML in UI → change survives a redeploy; delete a user.

## Spec-coverage self-check (Plan H section of 2026-07-03-luncur-phase2-design.md)

- Admin `/ui/users`: list (email, role, created, token count), delete user, create invite (role + 7-day expiry) → copyable `/ui/register?token=` link ✅ (T1/T4)
- `/ui/register`: email+password, single-use consume, creates user + logs in ✅ (T1/T4)
- CLI parity: `invite create --role`, `list`, `revoke` ✅ (T2); `invites` gains created_by/used_by/used_at ✅ (T1)
- No email sending ✅ (by design)
- YAML editor: per-kind GET textarea of final rendered YAML; POST diffs against base render → stores strategic-merge patch via the same path `luncur edit` uses; re-sync when live; invalid YAML → error re-render ✅ (T3/T5)
- Templates stay stdlib html/template, no templ swap ✅ (untouched)
