# luncur Plan P — Addon Upgrade + App Adopt Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upgrade an addon in place to a new version (`addon upgrade`), and reverse eject (`app adopt`) so luncur reclaims management of an ejected app.

**Architecture:** Two small verticals over existing machinery. Upgrade: new store setter `SetAddonVersion`, a `POST .../addons/{name}/upgrade` handler that re-renders the addon's manifests (existing `addon.Render` with the new version → new image tag) and SSA-applies them (rolling restart; PVC untouched), plus a fixed migration warning in every response. Adopt: new store setter `SetAppAdopted` (ejected=0), a `POST .../apps/{app}/adopt` handler that 409s `not_ejected` on non-ejected apps, clears the flag, and re-applies the app's rendered state via the existing `syncApp` (reclaiming `fieldManager=luncur`). CLI, client, and UI mirror both.

**Tech Stack:** Go stdlib + existing deps only (cobra, k8s client-go fakes in tests). No new Go module dependencies.

**Branch:** `plan-p` off `main`.

## Global Constraints (from Phase 4 spec)

- Single Go module, one binary from `cmd/luncur`. **No new Go module dependencies.**
- Server-side apply everywhere, `fieldManager=luncur`. API error envelope unchanged.
- Addon upgrade warning copy, verbatim in API response and CLI: `major version DB upgrades may require manual migration — take a backup first.`
- Adopt on a non-ejected app → `409` code `not_ejected`. Addon upgrade on a missing addon → `404`.
- Data (the addon PVC) is untouched by upgrade. Adopt re-applies luncur's rendered state, winning any drift (spec: out of scope to preserve diverged cluster state).
- Tests must not require a cluster: fake dynamic kube client fixtures (`addonTestServer`, `ejectTestServer`) already exist.
- Conventional commits. Before **every** commit: `go build ./... && go vet ./... && go test ./...` — all green.

---

### Task 1: store setters — `SetAddonVersion`, `SetAppAdopted`

**Files:**
- Modify: `internal/store/addons.go`
- Modify: `internal/store/apps.go` (also fix the now-stale "There is no un-eject setter" comment on `SetAppEjected`)
- Test: `internal/store/addons_test.go`, `internal/store/apps_test.go`

**Interfaces:**
- Consumes: existing `Store.CreateAddon(projectID, typ, name, version string, sizeGB int, credsEnc []byte)`, `Store.GetAddon(projectID, name)`, `Store.CreateApp(projectID, name string, port int)`, `Store.SetAppEjected(id)`, `Store.GetApp(projectID, name)`, `ErrNotFound`, test helper `openTest(t)` (store_test.go).
- Produces: `func (s *Store) SetAddonVersion(id int64, version string) error` and `func (s *Store) SetAppAdopted(id int64) error` — both return `ErrNotFound` when no row matches. Tasks 2 and 3 call these.

- [ ] **Step 1: Create branch**

```bash
git checkout -b plan-p
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/store/addons_test.go`:

```go
func TestSetAddonVersion(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAddon(p.ID, "postgres", "pg1", "16", 1, []byte("creds"))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetAddonVersion(a.ID, "17"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAddon(p.ID, "pg1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "17" {
		t.Fatalf("version = %q, want 17", got.Version)
	}

	if err := s.SetAddonVersion(99999, "17"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}
```

(Add `"errors"` to the imports if missing.)

Append to `internal/store/apps_test.go`:

```go
func TestSetAppAdopted(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SetAppEjected(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ejected {
		t.Fatal("want ejected after SetAppEjected")
	}

	if err := s.SetAppAdopted(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ejected {
		t.Fatal("want not ejected after SetAppAdopted")
	}

	if err := s.SetAppAdopted(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
}
```

(Check each test file's existing imports; both need `"errors"` and `"testing"`. `CreateProject`/`CreateApp` signatures: verify against the file's existing tests and adjust the seeding lines to match if they differ.)

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestSetAddonVersion|TestSetAppAdopted' -v`
Expected: FAIL — `s.SetAddonVersion undefined`, `s.SetAppAdopted undefined`.

- [ ] **Step 4: Implement**

Append to `internal/store/addons.go`:

```go
// SetAddonVersion updates an addon's recorded version. The caller is
// responsible for re-rendering and applying the addon's manifests.
func (s *Store) SetAddonVersion(id int64, version string) error {
	res, err := s.db.Exec(`UPDATE addons SET version = ? WHERE id = ?`, version, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
```

In `internal/store/apps.go`, update `SetAppEjected`'s comment (eject is no longer one-way) and add the adopt setter below it:

```go
// SetAppEjected marks an app as ejected from luncur's management.
// SetAppAdopted reverses it.
func (s *Store) SetAppEjected(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppAdopted clears the ejected flag: luncur manages the app again.
func (s *Store) SetAppAdopted(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestSetAddonVersion|TestSetAppAdopted' -v`
Expected: PASS.

- [ ] **Step 6: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/store/
git commit -m "feat: store setters for addon version and app adopt"
```

---

### Task 2: `POST /v1/projects/{p}/addons/{name}/upgrade`

**Files:**
- Modify: `internal/server/addons.go` (new handler + warning const)
- Modify: `internal/server/server.go` (route)
- Test: `internal/server/addons_test.go`

**Interfaces:**
- Consumes: `s.st.SetAddonVersion` (Task 1); existing `s.requireProject`, `s.requireKube`, `s.requireAddon`, `s.unsealCreds(a store.Addon) (addon.Creds, error)`, `addon.Render(addon.Params{Namespace, Type, Name, Version string; SizeGB int; Creds addon.Creds})`, `s.kube.EnsureNamespace`, `s.kube.Apply`; test fixture `addonTestServer(t) (*server, *httptest.Server, *store.Store, *[]string)`.
- Produces: route `POST /v1/projects/{project}/addons/{name}/upgrade` accepting `{"version":"V"}`, responding `200 {"name","type","version","warning"}`; `const addonUpgradeWarning`. Task 4's client calls this route.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/addons_test.go`:

```go
func TestAddonUpgrade(t *testing.T) {
	_, srv, st, actions := addonTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin,
		`{"type":"postgres","name":"pg1"}`).Body.Close()

	*actions = nil
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/pg1/upgrade", admin,
		`{"version":"17"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("upgrade: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Warning string `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Version != "17" {
		t.Fatalf("version = %q, want 17", out.Version)
	}
	if !strings.Contains(out.Warning, "take a backup") {
		t.Fatalf("warning = %q, want migration warning", out.Warning)
	}

	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAddon(p.ID, "pg1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != "17" {
		t.Fatalf("stored version = %q, want 17", a.Version)
	}

	applied := strings.Join(*actions, ",")
	if !strings.Contains(applied, "statefulsets") {
		t.Fatalf("no StatefulSet apply recorded, actions: %s", applied)
	}

	// Missing addon -> 404.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/nope/upgrade", admin,
		`{"version":"17"}`)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("missing addon: want 404, got %d", resp.StatusCode)
	}

	// Empty version -> 400.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons/pg1/upgrade", admin, `{}`)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("empty version: want 400, got %d", resp.StatusCode)
	}
}
```

(`json` and `strings` are already imported by addons_test.go; verify.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestAddonUpgrade -v`
Expected: FAIL — upgrade route missing, 404 body decode mismatch (`want 200, got 404`).

- [ ] **Step 3: Implement**

Append to `internal/server/addons.go`:

```go
// addonUpgradeWarning rides every upgrade response: luncur only swaps the
// image tag; it cannot run engine-level data migrations.
const addonUpgradeWarning = "major version DB upgrades may require manual migration — take a backup first."

// handleUpgradeAddon re-renders an addon's manifests at a new version and
// SSA-applies them (rolling restart). The PVC and credentials are untouched.
func (s *server) handleUpgradeAddon(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	a, ok := s.requireAddon(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Version == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "version is required")
		return
	}

	if err := s.st.SetAddonVersion(a.ID, req.Version); err != nil {
		log.Printf("upgrade addon %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.Version = req.Version

	creds, err := s.unsealCreds(a)
	if err != nil {
		if errors.Is(err, errSealerUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "sealer_unavailable", "sealer is not configured")
			return
		}
		log.Printf("upgrade addon %s: creds: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	objs, err := addon.Render(addon.Params{
		Namespace: p.Namespace, Type: a.Type, Name: a.Name, Version: a.Version,
		SizeGB: a.SizeGB, Creds: creds,
	})
	if err != nil {
		log.Printf("upgrade addon %s: render: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.kube.EnsureNamespace(r.Context(), p.Namespace); err != nil {
		log.Printf("upgrade addon %s: namespace: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if err := s.kube.Apply(r.Context(), p.Namespace, objs); err != nil {
		log.Printf("upgrade addon %s: apply: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name": a.Name, "type": a.Type, "version": a.Version,
		"warning": addonUpgradeWarning,
	})
}
```

In `internal/server/server.go` `handler()`, after the `DELETE /v1/projects/{project}/addons/{name}` line, add:

```go
	mux.HandleFunc("POST /v1/projects/{project}/addons/{name}/upgrade", s.authed(s.handleUpgradeAddon))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/server/ -run 'TestAddon' -v`
Expected: PASS — new test plus existing addon tests.

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/addons.go internal/server/server.go internal/server/addons_test.go
git commit -m "feat: addon upgrade endpoint — re-render at new version, SSA apply"
```

---

### Task 3: `POST /v1/projects/{p}/apps/{app}/adopt`

**Files:**
- Modify: `internal/server/eject.go` (new handler)
- Modify: `internal/server/server.go` (route)
- Test: `internal/server/eject_test.go`

**Interfaces:**
- Consumes: `s.st.SetAppAdopted` (Task 1); existing `s.requireProject`, `s.requireApp`, `s.syncApp(ctx, p, a)` (no-ops without a live deployment; re-renders + `kube.Apply` otherwise), fixtures `ejectTestServer(t) (*httptestServer, *store.Store, *[]string, string)` and `appID(t, st, project, app)` (apps_test.go).
- Produces: route `POST /v1/projects/{project}/apps/{app}/adopt` → `200 {"adopted":true}` (+ `"warning"` if the re-apply failed), `409` code `not_ejected` on a non-ejected app. Task 4's client and Task 5's UI handler call the same store+sync sequence.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/eject_test.go`:

```go
func TestAdoptFlow(t *testing.T) {
	srv, st, actions, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()

	id := appID(t, st, "proj", "web")
	if _, err := st.CreateDeployment(id, "live", "nginx:1", 0); err != nil {
		t.Fatal(err)
	}

	// Adopt before eject -> 409 not_ejected.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/adopt", admin, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("adopt non-ejected: want 409, got %d", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if env.Error.Code != "not_ejected" {
		t.Fatalf("code = %q, want not_ejected", env.Error.Code)
	}

	// Eject, then adopt.
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "").Body.Close()
	*actions = nil
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/adopt", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("adopt: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Adopted bool   `json:"adopted"`
		Warning string `json:"warning"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !out.Adopted || out.Warning != "" {
		t.Fatalf("adopt response = %+v", out)
	}

	// Flag cleared in the store.
	p, err := st.GetProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(p.ID, "web")
	if err != nil {
		t.Fatal(err)
	}
	if a.Ejected {
		t.Fatal("app still ejected after adopt")
	}

	// The live state was re-applied (fieldManager reclaim).
	applied := strings.Join(*actions, ",")
	if !strings.Contains(applied, "deployments") {
		t.Fatalf("no Deployment re-apply recorded, actions: %s", applied)
	}

	// Mutations work again: scale no longer 409s.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/scale", admin, `{"replicas":2}`)
	resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		t.Fatal("scale still 409s after adopt")
	}
}
```

(eject_test.go already imports `encoding/json`, `net/http`, `strings`; verify.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestAdoptFlow -v`
Expected: FAIL — adopt route missing (404 where 409 expected).

- [ ] **Step 3: Implement**

Append to `internal/server/eject.go`:

```go
// handleAdoptApp reverses eject: clears the flag and re-applies luncur's
// rendered state onto the still-running objects, reclaiming
// fieldManager=luncur (and overwriting any drift — documented behavior).
func (s *server) handleAdoptApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if !a.Ejected {
		writeError(w, http.StatusConflict, "not_ejected", "app is not ejected")
		return
	}

	if err := s.st.SetAppAdopted(a.ID); err != nil {
		log.Printf("adopt %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.Ejected = false

	out := map[string]any{"adopted": true}
	if s.kube != nil {
		if err := s.syncApp(r.Context(), p, a); err != nil {
			log.Printf("adopt %s: sync: %v", a.Name, err)
			out["warning"] = "adopted, but re-apply failed: " + err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}
```

In `internal/server/server.go` `handler()`, after the eject route, add:

```go
	mux.HandleFunc("POST /v1/projects/{project}/apps/{app}/adopt", s.authed(s.handleAdoptApp))
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestAdoptFlow|TestEject' -v`
Expected: PASS — adopt flow plus existing eject tests.

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/eject.go internal/server/server.go internal/server/eject_test.go
git commit -m "feat: app adopt endpoint — reverse eject, re-apply rendered state"
```

---

### Task 4: client + CLI (`addon upgrade`, `app adopt`)

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/cli/addon.go` (upgrade subcommand)
- Modify: `internal/cli/eject.go` (adoptCmd lives beside ejectCmd)
- Modify: `internal/cli/app.go:113` (register adoptCmd)
- Test: `internal/cli/commands_test.go`

**Interfaces:**
- Consumes: Task 2/3 routes; existing `AddonInfo{Name, Type, Version, Warning ...}`, `c.do`, `url.PathEscape`, CLI test helpers `testEnv(t)` (no kube: upgrade → 503 kubernetes_unavailable) and `run(t, args...)`.
- Produces: `func (c *Client) UpgradeAddon(project, name, version string) (AddonInfo, error)`; `func (c *Client) AdoptApp(project, app string) (warning string, err error)`; `luncur addon upgrade <name> --project P --version V`; `luncur app adopt <name> --project P`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/commands_test.go`:

```go
// TestAddonUpgradeCommand: testEnv has no kube, so the server answers 503
// kubernetes_unavailable — proves the CLI wiring reaches the right route.
func TestAddonUpgradeCommand(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}

	_, err := run(t, "addon", "upgrade", "pg1", "--project", "p", "--version", "17")
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes error, got %v", err)
	}
}

// TestAppAdoptCommand: eject then adopt round-trips through the CLI; no
// kube in testEnv means adopt just clears the flag (sync is skipped).
func TestAppAdoptCommand(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "web", "--project", "p", "--port", "8080"); err != nil {
		t.Fatal(err)
	}

	// Adopt before eject -> not_ejected error.
	_, err := run(t, "app", "adopt", "web", "--project", "p")
	if err == nil || !strings.Contains(err.Error(), "not ejected") {
		t.Fatalf("adopt non-ejected: want not-ejected error, got %v", err)
	}

	if out, err := run(t, "app", "eject", "web", "--project", "p", "--yes"); err != nil {
		t.Fatalf("eject: %v (%s)", err, out)
	}
	out, err := run(t, "app", "adopt", "web", "--project", "p")
	if err != nil {
		t.Fatalf("adopt: %v (%s)", err, out)
	}
	if !strings.Contains(out, "adopted web") {
		t.Fatalf("adopt output: %s", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestAddonUpgradeCommand|TestAppAdoptCommand' -v`
Expected: FAIL — `unknown command "upgrade"`, `unknown command "adopt"`.

- [ ] **Step 3: Implement**

1. `internal/client/client.go` — after `RemoveAddon`, add:

```go
// UpgradeAddon re-renders an addon at a new version and applies it (a
// rolling restart). The response carries the manual-migration warning.
func (c *Client) UpgradeAddon(project, name, version string) (AddonInfo, error) {
	var out AddonInfo
	err := c.do("POST",
		"/v1/projects/"+url.PathEscape(project)+"/addons/"+url.PathEscape(name)+"/upgrade",
		map[string]string{"version": version}, &out)
	return out, err
}
```

After `EjectApp`, add:

```go
// AdoptApp reverses eject: luncur reclaims management of project/app and
// re-applies its rendered state. warning is non-empty when the flag was
// cleared but the re-apply failed.
func (c *Client) AdoptApp(project, app string) (string, error) {
	var out struct {
		Warning string `json:"warning"`
	}
	err := c.do("POST",
		"/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/adopt", nil, &out)
	return out.Warning, err
}
```

2. `internal/cli/addon.go` — before the `cmd.AddCommand(...)` line, add the upgrade subcommand; then register it:

```go
	var upgradeProject, upgradeVersion string
	upgrade := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "Upgrade an addon in place to a new version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			a, err := c.UpgradeAddon(upgradeProject, args[0], upgradeVersion)
			if err != nil {
				return err
			}
			cmd.Printf("upgraded %s to %s\n", a.Name, a.Version)
			if a.Warning != "" {
				cmd.Printf("warning: %s\n", a.Warning)
			}
			return nil
		},
	}
	upgrade.Flags().StringVar(&upgradeProject, "project", "", "project name")
	upgrade.MarkFlagRequired("project")
	upgrade.Flags().StringVar(&upgradeVersion, "version", "", "target version (image tag)")
	upgrade.MarkFlagRequired("version")

	cmd.AddCommand(create, add, attach, detach, list, remove, upgrade)
```

3. `internal/cli/eject.go` — append below `ejectCmd`:

```go
// adoptCmd is `app adopt`: reverses eject — luncur reclaims management of
// the app and re-applies its rendered state (winning any manual drift).
func adoptCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Re-adopt an ejected app under luncur's management",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			warning, err := c.AdoptApp(project, args[0])
			if err != nil {
				return err
			}
			cmd.Printf("adopted %s — luncur manages it again\n", args[0])
			if warning != "" {
				cmd.Printf("warning: %s\n", warning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name")
	cmd.MarkFlagRequired("project")
	return cmd
}
```

4. `internal/cli/app.go:113` — register it:

```go
	cmd.AddCommand(create, list, info, raw, ejectCmd(), adoptCmd())
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestAddonUpgradeCommand|TestAppAdoptCommand' -v`
Expected: PASS. (If the adopt-before-eject assertion fails on message wording, match the server's `not_ejected` message: the client surfaces `app is not ejected`.)

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/client/client.go internal/cli/addon.go internal/cli/eject.go internal/cli/app.go internal/cli/commands_test.go
git commit -m "feat: addon upgrade + app adopt CLI"
```

---

### Task 5: UI adopt button

**Files:**
- Modify: `internal/server/ui.go` (route in `uiRoutes` + handler)
- Modify: `internal/server/templates/app.html` (adopt form in the ejected note)
- Test: `internal/server/ui_test.go`

**Interfaces:**
- Consumes: `s.st.SetAppAdopted`, `s.syncIfLive(ctx, p, a)` (kube-nil-safe, log-only), existing UI helpers `s.uiProject`, `s.uiApp`, `uiRedirect(w, r, p, a)`; test helpers `ejectTestServer`, `uiSessionCookie`, `uiCSRF`, `uiPost`, `noRedirectClient`.
- Produces: `POST /ui/projects/{project}/apps/{app}/adopt` → 303 back to the app page; adopt button rendered inside the ejected note.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/ui_test.go`:

```go
// TestUIAdoptButton: an ejected app's page shows the adopt form; posting it
// clears the flag and the page returns to normal management UI.
func TestUIAdoptButton(t *testing.T) {
	srv, st, _, _ := ejectTestServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps", admin, `{"name":"web","port":8080}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/apps/web/eject", admin, "").Body.Close()

	u, err := st.GetUserByEmail("root@b.co")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	ck := uiSessionCookie(t, st, u.ID)

	appPage := func(t *testing.T) string {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+"/ui/projects/proj/apps/web", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(ck)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("app page: want 200, got %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	body := appPage(t)
	if !strings.Contains(body, `action="/ui/projects/proj/apps/web/adopt"`) {
		t.Fatalf("ejected page missing adopt form:\n%s", body)
	}

	csrfCk := uiCSRF(t, client, srv.URL)
	resp := uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/adopt", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("adopt post: want 303, got %d", resp.StatusCode)
	}

	body = appPage(t)
	if strings.Contains(body, "This app is ejected") {
		t.Fatalf("page still shows ejected note after adopt:\n%s", body)
	}
	if !strings.Contains(body, `action="/ui/projects/proj/apps/web/scale"`) {
		t.Fatalf("management UI (scale form) not back after adopt:\n%s", body)
	}

	// Adopt on a non-ejected app -> 409.
	resp = uiPost(t, client, srv.URL+"/ui/projects/proj/apps/web/adopt", csrfCk, ck, url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("adopt non-ejected: want 409, got %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestUIAdoptButton -v`
Expected: FAIL — adopt form absent from the ejected page.

- [ ] **Step 3: Implement**

1. `internal/server/ui.go`, in `uiRoutes` after the rollback route, add:

```go
	mux.HandleFunc("POST /ui/projects/{project}/apps/{app}/adopt", s.uiPage(s.handleUIAdopt))
```

2. Add the handler (near the other app-page POST handlers):

```go
// handleUIAdopt is handleAdoptApp's UI twin: clear the ejected flag,
// best-effort re-sync, redirect back to the app page.
func (s *server) handleUIAdopt(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if !a.Ejected {
		http.Error(w, "app is not ejected", http.StatusConflict)
		return
	}
	if err := s.st.SetAppAdopted(a.ID); err != nil {
		log.Printf("ui adopt: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.Ejected = false
	s.syncIfLive(r.Context(), p, a)
	uiRedirect(w, r, p, a)
}
```

3. `internal/server/templates/app.html` — extend the ejected note:

```html
{{if .App.Ejected}}
<p>This app is ejected — luncur no longer manages it.</p>
<form method="post" action="/ui/projects/{{.Project.Name}}/apps/{{.App.Name}}/adopt">
  <input type="hidden" name="_csrf" value="{{.CSRF}}">
  <button type="submit">adopt — luncur manages it again</button>
</form>
{{else}}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestUIAdoptButton|TestUIApp' -v`
Expected: PASS — new test plus existing app-page tests (the ejected-badge test must still pass: the badge and note remain, only augmented by the form).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/ui.go internal/server/templates/app.html internal/server/ui_test.go
git commit -m "feat: adopt button on the ejected app page"
```

---

### Task 6: docs

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README**

1. Addon section (near the other `luncur addon ...` command lines): add

```sh
luncur addon upgrade pg1 --project myproj --version 17   # in-place, rolling restart; PVC untouched
```

with the note: every upgrade response carries the warning *"major version DB upgrades may require manual migration — take a backup first."* — luncur swaps the image tag; it does not run `pg_upgrade`.

2. "Ejecting an app" section (~line 356): eject is no longer one-way. Reword the "one-way — there is no un-eject" sentence to point at adopt, and append:

```sh
luncur app adopt myapp --project myproj
```

Adopt clears the ejected flag and re-applies luncur's rendered state onto the running objects (reclaiming `fieldManager=luncur`); any manual drift made while ejected is overwritten. The web UI's ejected note gains an adopt button that does the same.

3. Status line (~line 6) still says "eject + GC" for Phase 3 — leave it; no change needed.

- [ ] **Step 2: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add README.md
git commit -m "docs: addon upgrade and app adopt"
```

---

## Manual verification (owner's VPS, after merge)

Per the Phase 4 test strategy: upgrade a postgres addon (`addon upgrade pg1 --version 17`) and watch the StatefulSet roll; eject an app, tweak nothing, adopt it back and confirm scale/deploy work again.

## Self-review notes

- Spec coverage: upgrade endpoint + version row update + re-render/SSA + warning (Task 2), CLI upgrade (Task 4), adopt endpoint + 409 `not_ejected` + `SetAppAdopted` + re-apply (Task 3), CLI adopt (Task 4), UI adopt button restoring management UI (Task 5), store setters (Task 1), docs (Task 6). Error table: missing addon 404 and non-ejected 409 both tested.
- Type consistency: `SetAddonVersion(id int64, version string) error` / `SetAppAdopted(id int64) error` used verbatim in Tasks 2/3/5; `UpgradeAddon(project, name, version) (AddonInfo, error)` / `AdoptApp(project, app) (string, error)` match Task 4 CLI call sites; warning const string matches the spec copy exactly.
- Store seeding lines in Task 1 tests flag signature verification (`CreateProject`/`CreateApp`) — executor confirms against neighboring tests in the same files.
