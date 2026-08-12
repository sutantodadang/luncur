package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCopyEnvSetupAppsAndVars covers the app half of copyEnvSetup:
// a source-only app is created in the target with full config+vars, an
// overlapping app has its mutable config and whole var set overwritten
// (target-only var deleted), and a target-only app survives untouched.
func TestCopyEnvSetupAppsAndVars(t *testing.T) {
	s, _ := previewTestServer(t)
	p, err := s.st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	prod, err := s.st.GetEnvironment(p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	stg, err := s.st.GetEnvironment(p.ID, "staging")
	if err != nil {
		t.Fatal(err)
	}

	// Source-only app, with config + vars + git source.
	api, err := s.st.CreateAppInEnv(prod.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetReplicas(api.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetResources(api.ID, 500, 256); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetHealthPath(api.ID, "/healthz"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetAppGitSource(api.ID, "https://example.com/r.git", "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetEnv(api.ID, "API_KEY", []byte("sealed-api")); err != nil {
		t.Fatal(err)
	}

	// Overlapping app: exists in both, different config/vars in staging.
	wrk, err := s.st.CreateAppInEnv(prod.ID, "worker", 0, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetEnv(wrk.ID, "SHARED", []byte("sealed-prod")); err != nil {
		t.Fatal(err)
	}
	stgWrk, err := s.st.CreateAppInEnv(stg.ID, "worker", 0, "worker", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetReplicas(stgWrk.ID, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetEnv(stgWrk.ID, "SHARED", []byte("sealed-stale")); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetEnv(stgWrk.ID, "STAGING_ONLY", []byte("sealed-mine")); err != nil {
		t.Fatal(err)
	}

	// Target-only app must survive.
	if _, err := s.st.CreateAppInEnv(stg.ID, "legacy", 9090, "web", ""); err != nil {
		t.Fatal(err)
	}

	sum, err := s.copyEnvSetup(context.Background(), prod, stg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AppsCreated != 1 || sum.AppsUpdated != 1 {
		t.Fatalf("want 1 created + 1 updated, got %+v", sum)
	}

	// Created app carries config, git source, and vars.
	got, err := s.st.GetAppInEnv(stg.ID, "api")
	if err != nil {
		t.Fatal(err)
	}
	if got.Replicas != 3 || got.CPUMilli != 500 || got.MemoryMB != 256 || got.HealthPath != "/healthz" {
		t.Fatalf("api config not copied: %+v", got)
	}
	if got.SourceType != "git" || got.GitURL != "https://example.com/r.git" || got.GitBranch != "main" {
		t.Fatalf("api git source not copied: %+v", got)
	}
	vars, err := s.st.ListEnv(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(vars["API_KEY"]) != "sealed-api" {
		t.Fatalf("api vars not copied: %v", vars)
	}

	// Overlapping app overwritten: replicas from source, stale var replaced,
	// target-only var deleted.
	got, err = s.st.GetAppInEnv(stg.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if got.Replicas != 1 {
		t.Fatalf("worker replicas should be overwritten to source's 1, got %d", got.Replicas)
	}
	vars, err = s.st.ListEnv(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(vars["SHARED"]) != "sealed-prod" {
		t.Fatalf("SHARED not overwritten: %v", vars)
	}
	if _, ok := vars["STAGING_ONLY"]; ok {
		t.Fatal("STAGING_ONLY should have been deleted by the copy")
	}

	// Target-only app untouched.
	if _, err := s.st.GetAppInEnv(stg.ID, "legacy"); err != nil {
		t.Fatal("target-only app must survive the copy:", err)
	}
}

// TestCopyEnvSetupSkipsExistingAddonTypes covers cloneEnvAddons' new guard:
// a source addon whose type already exists in the target is skipped, a
// missing type is cloned.
func TestCopyEnvSetupSkipsExistingAddonTypes(t *testing.T) {
	s, _ := previewTestServer(t)
	p, err := s.st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	prod, err := s.st.GetEnvironment(p.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	stg, err := s.st.GetEnvironment(p.ID, "staging")
	if err != nil {
		t.Fatal(err)
	}

	// Mirror TestClonePreviewAddons' seeding (preview_test.go): seed
	// postgres + redis in production, postgres already in staging.
	seedPreviewAddon(t, s, s.st, prod, "postgres", "pg1")
	seedPreviewAddon(t, s, s.st, prod, "redis", "redis1")
	seedPreviewAddon(t, s, s.st, stg, "postgres", "pg2")

	sum, err := s.copyEnvSetup(context.Background(), prod, stg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AddonsCloned != 1 {
		t.Fatalf("want exactly the redis addon cloned, got %+v", sum)
	}
	addons, err := s.st.AddonsForEnv(stg.ID)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, a := range addons {
		types[a.Type]++
	}
	if types["postgres"] != 1 || types["redis"] != 1 {
		t.Fatalf("staging addons after copy: %v", types)
	}
}

// TestHandleCopyEnvSetupValidation covers the handler's reject paths:
// same source/target → 400, unknown env → 404.
func TestHandleCopyEnvSetupValidation(t *testing.T) {
	s, _ := previewTestServer(t)
	u, err := s.st.CreateUser("a@b.co", "pw123456", "admin")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.AddMember(p.ID, u.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}

	do := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/projects/proj/envs/copy", strings.NewReader(body))
		r.SetPathValue("project", "proj")
		w := httptest.NewRecorder()
		s.handleCopyEnvSetup(w, r, u)
		return w
	}

	if w := do(`{"source":"production","target":"production"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("same env: want 400, got %d", w.Code)
	}
	if w := do(`{"source":"production","target":"nope"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown target: want 404, got %d", w.Code)
	}
	if w := do(`{"source":"production","target":"staging"}`); w.Code != http.StatusOK {
		t.Fatalf("valid copy: want 200, got %d: %s", w.Code, w.Body)
	}
}
