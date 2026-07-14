package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// TestSanitizeBranch covers the common shapes: lowercasing, "/" and other
// punctuation both collapsing to "-", repeated dashes collapsing to one,
// leading/trailing dashes trimmed, and an empty result falling back to a
// non-empty placeholder.
func TestSanitizeBranch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"main", "main"},
		{"develop", "develop"},
		{"feature/Fix_Login", "feature-fix-login"},
		{"UPPER/CASE", "upper-case"},
		{"a//b--c", "a-b-c"},
		{"-leading-and-trailing-", "leading-and-trailing"},
		{"", "branch"},
	}
	for _, c := range cases {
		if got := sanitizeBranch(c.in); got != c.want {
			t.Errorf("sanitizeBranch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeBranchTruncatesLongNames checks a long branch name is
// truncated to maxSanitizedBranch, still DNS-1123 shaped (no leading/
// trailing dash), and actually accepted by store.CreateEnvironment — the
// real acceptance test for "leaves room for the <app>-<env> host label".
func TestSanitizeBranchTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("feature-branch-", 5) // 75 chars
	got := sanitizeBranch(long)
	if len(got) > maxSanitizedBranch {
		t.Fatalf("sanitizeBranch(%q) = %q, len %d > %d", long, got, len(got), maxSanitizedBranch)
	}
	if got == "" || got[0] == '-' || got[len(got)-1] == '-' {
		t.Fatalf("sanitizeBranch(%q) = %q, not DNS-1123 shaped", long, got)
	}

	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateEnvironment(p.ID, got, "preview", ""); err != nil {
		t.Fatalf("sanitized name rejected by CreateEnvironment: %v", err)
	}
}

// previewTestServer builds a *server (no HTTP wrapper — routeBranch/
// ensurePreview are unexported, so tests call them directly, mirroring
// TestRequireEnv's own style) with a fake kube layer that never reports a
// build job as terminal, so a deploy routeBranch triggers deterministically
// stays "building" — same convention as webhook_test.go's
// webhookTestServer.
func previewTestServer(t *testing.T) (*server, *[]string) {
	t.Helper()
	st := newTestStore(t)
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var actions []string
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		actions = append(actions, a.GetVerb()+" "+a.GetResource().Resource)
		if a.GetVerb() == "get" || a.GetVerb() == "list" {
			// Let the default tracker answer reads (e.g. minio addon
			// creation's ensureMinioBucket polls StatefulSetReady in the
			// background — a swallowed nil object there is a nil-pointer
			// panic, not a clean "not found") — same convention as
			// addonTestServer's reactor (addons_test.go).
			return false, nil, nil
		}
		return true, nil, nil
	})
	dyn.PrependReactor("get", "jobs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": a.(ktesting.GetAction).GetName(), "namespace": "luncur-system"},
			"status":   map[string]any{},
		}}, nil
	})
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{
		Store: st, Kube: kube.NewFromDynamic(dyn), Sealer: sealer,
		ExternalIP: "1.2.3.4", DataDir: t.TempDir(),
	})
	return srv, &actions
}

// TestRouteBranch covers routeBranch's three-way dispatch: a standing
// environment's base_branch match deploys that environment's git apps and
// bumps LastActiveAt; an unmapped branch routes through ensurePreview and
// deploys the freshly cloned app; a delete/PR-close event tears down (via
// the stubbed teardownPreview) a matching preview and no-ops when there is
// none.
func TestRouteBranch(t *testing.T) {
	srv, _ := previewTestServer(t)
	st := srv.st

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := st.GetEnvironment(p.ID, "production") // base_branch "main"
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}

	prodApp, err := st.CreateGitApp(p.ID, "api", 8080, "https://x/y.git", "main", "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(prodApp.ID, prod.ID); err != nil {
		t.Fatal(err)
	}

	// Standing branch match: deploys the production app and touches the
	// environment.
	if err := srv.routeBranch(context.Background(), p, "main", false); err != nil {
		t.Fatalf("routeBranch(main): %v", err)
	}
	if n, err := st.CountDeployments(prodApp.ID); err != nil || n != 1 {
		t.Fatalf("want 1 deployment after standing-branch route, got %d (%v)", n, err)
	}
	touched, err := st.GetEnvironmentByID(prod.ID)
	if err != nil || touched.LastActiveAt == "" {
		t.Fatalf("expected production LastActiveAt set: %+v %v", touched, err)
	}

	// Seed an app in the preview base env (develop) so a fresh preview has
	// something to clone.
	devApp, err := st.CreateGitApp(p.ID, "worker", 9090, "https://x/y.git", "develop", "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(devApp.ID, dev.ID); err != nil {
		t.Fatal(err)
	}

	// Unmapped branch: routes through ensurePreview, then deploys the
	// cloned app inside the new preview environment.
	if err := srv.routeBranch(context.Background(), p, "feature/x", false); err != nil {
		t.Fatalf("routeBranch(feature/x): %v", err)
	}
	previewEnv, err := st.GetEnvironment(p.ID, sanitizeBranch("feature/x"))
	if err != nil {
		t.Fatalf("preview environment not created: %v", err)
	}
	if previewEnv.Kind != "preview" || previewEnv.SourceBranch != "feature/x" {
		t.Fatalf("preview env = %+v", previewEnv)
	}
	clonedApps, err := st.ListAppsInEnv(previewEnv.ID)
	if err != nil || len(clonedApps) != 1 || clonedApps[0].Name != "worker" {
		t.Fatalf("cloned apps = %+v, err %v", clonedApps, err)
	}
	if n, err := st.CountDeployments(clonedApps[0].ID); err != nil || n != 1 {
		t.Fatalf("want 1 deployment for cloned preview app, got %d (%v)", n, err)
	}

	// Delete/PR-close routing to a branch with no matching preview: no-op,
	// no error.
	if err := srv.routeBranch(context.Background(), p, "no-such-branch", true); err != nil {
		t.Fatalf("routeBranch(delete, missing preview): %v", err)
	}

	// Delete/PR-close routing to an existing preview calls the (stubbed)
	// teardownPreview and returns no error.
	if err := srv.routeBranch(context.Background(), p, "feature/x", true); err != nil {
		t.Fatalf("routeBranch(delete, existing preview): %v", err)
	}
}

// TestEnsurePreview covers Task 13's create-and-clone core directly (no
// kube needed: ensurePreview skips the namespace-ensure step when s.kube is
// nil): a fresh preview clones the base env's app (same port/kind, replicas
// capped to 1, health path copied, git_branch overridden to the pushed
// branch, env vars copied) into luncur-<p>-<sanitized>, and a second call
// for the same branch is idempotent — same environment, no duplicate app.
func TestEnsurePreview(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}

	base, err := st.CreateGitApp(p.ID, "api", 8080, "https://x/y.git", "develop", "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(base.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReplicas(base.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := st.SetHealthPath(base.ID, "/healthz"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnv(base.ID, "FOO", []byte("sealed-bytes")); err != nil {
		t.Fatal(err)
	}

	env, err := srv.ensurePreview(context.Background(), p, "feature/big-thing")
	if err != nil {
		t.Fatalf("ensurePreview: %v", err)
	}

	wantName := sanitizeBranch("feature/big-thing")
	if env.Name != wantName {
		t.Fatalf("env.Name = %q, want %q", env.Name, wantName)
	}
	if env.Kind != "preview" {
		t.Fatalf("env.Kind = %q, want preview", env.Kind)
	}
	if env.SourceBranch != "feature/big-thing" {
		t.Fatalf("env.SourceBranch = %q, want feature/big-thing", env.SourceBranch)
	}
	wantNS := "luncur-proj-" + wantName
	if env.Namespace != wantNS {
		t.Fatalf("env.Namespace = %q, want %q", env.Namespace, wantNS)
	}

	apps, err := st.ListAppsInEnv(env.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps = %+v, err %v", apps, err)
	}
	cloned := apps[0]
	if cloned.Name != "api" || cloned.Port != 8080 || cloned.Kind != "web" {
		t.Fatalf("cloned app = %+v", cloned)
	}
	if cloned.Replicas != 1 {
		t.Fatalf("cloned replicas = %d, want capped to 1", cloned.Replicas)
	}
	if cloned.HealthPath != "/healthz" {
		t.Fatalf("cloned health path = %q, want /healthz", cloned.HealthPath)
	}
	if cloned.SourceType != "git" || cloned.GitURL != "https://x/y.git" || cloned.GitBranch != "feature/big-thing" {
		t.Fatalf("cloned git source = %+v", cloned)
	}
	vars, err := st.ListEnv(cloned.ID)
	if err != nil || string(vars["FOO"]) != "sealed-bytes" {
		t.Fatalf("cloned env vars = %+v, err %v", vars, err)
	}

	// Idempotent: a second call for the same branch returns the same
	// environment and does not duplicate the cloned app.
	env2, err := srv.ensurePreview(context.Background(), p, "feature/big-thing")
	if err != nil {
		t.Fatalf("ensurePreview (2nd call): %v", err)
	}
	if env2.ID != env.ID {
		t.Fatalf("2nd call: env.ID = %d, want %d", env2.ID, env.ID)
	}
	apps2, err := st.ListAppsInEnv(env.ID)
	if err != nil || len(apps2) != 1 {
		t.Fatalf("apps after 2nd call = %+v, err %v", apps2, err)
	}
}

// seedPreviewAddon creates an addon of typ directly (bypassing createAddon's
// kube/exec machinery, like backup_test.go's seedBackupAddon) and attributes
// it to env, mirroring what createAddon now does via SetAddonEnvironmentID.
func seedPreviewAddon(t *testing.T, srv *server, st *store.Store, env store.Environment, typ, name string) store.Addon {
	t.Helper()
	creds, err := newAddonCreds(typ)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := srv.sealCreds(creds)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := st.CreateAddon(env.ProjectID, typ, name, "", 1, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAddonEnvironmentID(ad.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	ad.EnvironmentID = env.ID
	return ad
}

// TestClonePreviewAddons covers Task 14's core: a base postgres addon's dump
// is piped straight into the freshly created preview addon's restore (the
// same fakeExecer capture backup_test.go uses — the LAST ExecPod call's
// cmd/stdin, i.e. the restore, is what's asserted), a minio addon has no
// logical dump so it's created empty with a warning, and an app attached to
// the base addon gets its preview counterpart attached to the new addon too.
func TestClonePreviewAddons(t *testing.T) {
	srv, _ := previewTestServer(t)
	exec := &fakeExecer{out: "PGDUMPDATA"}
	srv.execer = exec
	st := srv.st

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.CreateEnvironment(p.ID, "feature-clone", "preview", "")
	if err != nil {
		t.Fatal(err)
	}

	pgAddon := seedPreviewAddon(t, srv, st, dev, "postgres", "db1")
	seedPreviewAddon(t, srv, st, dev, "minio", "store1")

	// An app in dev attached to the postgres addon, plus its already-cloned
	// preview counterpart (clonePreviewApp normally does this cloning; here
	// it's done directly since this test targets clonePreviewAddons alone).
	devApp, err := st.CreateAppInEnv(dev.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachAddon(pgAddon.ID, devApp.ID); err != nil {
		t.Fatal(err)
	}
	previewApp, err := st.CreateAppInEnv(preview.ID, "api", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	warnings := srv.clonePreviewAddons(context.Background(), dev, preview)

	previewAddons, err := st.AddonsForEnv(preview.ID)
	if err != nil || len(previewAddons) != 2 {
		t.Fatalf("preview addons = %+v, err %v, want 2", previewAddons, err)
	}
	var previewPG, previewMinio store.Addon
	for _, a := range previewAddons {
		switch a.Type {
		case "postgres":
			previewPG = a
		case "minio":
			previewMinio = a
		}
	}
	if previewPG.ID == 0 {
		t.Fatalf("no postgres addon cloned into preview: %+v", previewAddons)
	}
	if previewPG.Name == pgAddon.Name {
		t.Fatalf("preview addon reused base's name %q, want a distinct auto-minted name", previewPG.Name)
	}
	if previewMinio.ID == 0 {
		t.Fatalf("no minio addon cloned into preview: %+v", previewAddons)
	}

	// The dump->restore round trip: the fake execer's last call is the
	// restore, with the dump's bytes piped in as stdin.
	if string(exec.stdin) != "PGDUMPDATA" {
		t.Fatalf("restore stdin = %q, want PGDUMPDATA", exec.stdin)
	}
	if !strings.Contains(strings.Join(exec.cmd, " "), "pg_restore") {
		t.Fatalf("last exec cmd = %v, want pg_restore", exec.cmd)
	}

	foundMinioWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "store1") {
			foundMinioWarning = true
		}
	}
	if !foundMinioWarning {
		t.Fatalf("warnings = %v, want a minio warning mentioning store1", warnings)
	}

	// Attachment re-pointing: the preview app is now attached to the
	// preview's own postgres addon (not the base's).
	attached, err := st.AddonsForApp(previewApp.ID)
	if err != nil || len(attached) != 1 || attached[0].ID != previewPG.ID {
		t.Fatalf("preview app addon attachments = %+v, err %v, want [%d]", attached, err, previewPG.ID)
	}
}

// TestClonePreviewAddonsPerAddonFailureWarns covers per-addon degrade: with
// every ExecPod call failing, both a postgres and a redis base addon still
// get created (empty) in the preview — the exec failure only turns into a
// warning for that one addon, it never aborts the rest of the loop.
func TestClonePreviewAddonsPerAddonFailureWarns(t *testing.T) {
	srv, _ := previewTestServer(t)
	srv.execer = &fakeExecer{err: fmt.Errorf("pod gone")}
	st := srv.st

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.CreateEnvironment(p.ID, "feature-fail", "preview", "")
	if err != nil {
		t.Fatal(err)
	}

	seedPreviewAddon(t, srv, st, dev, "postgres", "db1")
	seedPreviewAddon(t, srv, st, dev, "redis", "cache1")

	warnings := srv.clonePreviewAddons(context.Background(), dev, preview)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2 (one dump failure per addon)", warnings)
	}

	previewAddons, err := st.AddonsForEnv(preview.ID)
	if err != nil || len(previewAddons) != 2 {
		t.Fatalf("preview addons = %+v, err %v, want 2 (created empty despite dump failure)", previewAddons, err)
	}
}

// TestClonePreviewAddonsNoExecerWarns covers the s.execer == nil guard: a
// postgres addon is still created (empty) in the preview, with a warning
// explaining exec was unavailable rather than a panic or hard failure.
func TestClonePreviewAddonsNoExecerWarns(t *testing.T) {
	srv, _ := previewTestServer(t) // execer left nil
	st := srv.st

	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SeedProjectEnvironments(p.ID); err != nil {
		t.Fatal(err)
	}
	p, err = st.GetProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.GetEnvironment(p.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.CreateEnvironment(p.ID, "feature-noexec", "preview", "")
	if err != nil {
		t.Fatal(err)
	}

	seedPreviewAddon(t, srv, st, dev, "postgres", "db1")

	warnings := srv.clonePreviewAddons(context.Background(), dev, preview)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "exec unavailable") {
		t.Fatalf("warnings = %v, want one exec-unavailable warning", warnings)
	}
	previewAddons, err := st.AddonsForEnv(preview.ID)
	if err != nil || len(previewAddons) != 1 {
		t.Fatalf("preview addons = %+v, err %v, want 1 (created empty)", previewAddons, err)
	}
}
