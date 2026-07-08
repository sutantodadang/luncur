package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/sutantodadang/luncur/internal/client"
	"github.com/sutantodadang/luncur/internal/secret"
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
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{Store: st, Sealer: sealer, DataDir: t.TempDir()}))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

// testEnvStore is testEnv's twin for tests that need direct store access to
// verify state the CLI/API don't surface in responses (e.g. build_path,
// which mirrors git_url in not being exposed via GET/app info).
func testEnvStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{Store: st, Sealer: sealer, DataDir: t.TempDir()}))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
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

func TestLoginPromptsForEmail(t *testing.T) {
	srv := testEnv(t)

	root := newRoot()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader("root@b.co\n"))
	root.SetArgs([]string{"login", srv.URL, "--password", "pw123456"})
	if err := root.Execute(); err != nil {
		t.Fatalf("login with prompted email: %v (%s)", err, out.String())
	}
	if !strings.Contains(out.String(), "email: ") {
		t.Fatalf("want email prompt, got %q", out.String())
	}
	if !strings.Contains(out.String(), "logged in") {
		t.Fatalf("want 'logged in', got %q", out.String())
	}

	got, err := run(t, "whoami")
	if err != nil {
		t.Fatalf("whoami after prompted login: %v", err)
	}
	if !strings.Contains(got, "root@b.co (admin)") {
		t.Fatalf("want identity line, got %q", got)
	}
}

func TestWhoamiWithoutLogin(t *testing.T) {
	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if _, err := run(t, "whoami"); err == nil {
		t.Fatal("want error when not logged in")
	}
}

func TestStatusAppAndList(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "api", "--project", "web", "--port", "3000"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "status", "api", "--project", "web")
	if err != nil {
		t.Fatalf("status app: %v (%s)", err, out)
	}
	// testEnv has no kube, so metrics are unavailable but deploys is still shown.
	for _, want := range []string{"app:      api", "status:   never_deployed", "replicas: 1", "metrics:  unavailable", "deploys:  0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in output, got %q", want, out)
		}
	}

	out, err = run(t, "status", "--project", "web")
	if err != nil {
		t.Fatalf("status list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "api") {
		t.Fatalf("want app list, got %q", out)
	}
}

// testSSHPubKey generates a fresh ed25519 key in authorized_keys format.
func testSSHPubKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
}

func TestDomainAndConfigCommands(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "web", "--project", "proj", "--port", "8080"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "domain", "add", "web", "www.example.com", "--project", "proj")
	if err != nil {
		t.Fatalf("domain add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "www.example.com") {
		t.Fatalf("want hostname in output, got %q", out)
	}

	out, err = run(t, "domain", "list", "web", "--project", "proj")
	if err != nil {
		t.Fatalf("domain list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "www.example.com") || !strings.Contains(out, "none") {
		t.Fatalf("want domain + status in list, got %q", out)
	}

	if _, err := run(t, "domain", "remove", "web", "www.example.com", "--project", "proj"); err != nil {
		t.Fatalf("domain remove: %v", err)
	}
	out, err = run(t, "domain", "list", "web", "--project", "proj")
	if err != nil {
		t.Fatalf("domain list after remove: %v (%s)", err, out)
	}
	if strings.Contains(out, "www.example.com") {
		t.Fatalf("domain not removed: %s", out)
	}

	if _, err := run(t, "config", "set", "cert_provider", "traefik"); err != nil {
		t.Fatalf("config set: %v", err)
	}
	out, err = run(t, "config", "get", "cert_provider")
	if err != nil {
		t.Fatalf("config get: %v (%s)", err, out)
	}
	if !strings.Contains(out, "traefik") {
		t.Fatalf("want traefik in output, got %q", out)
	}

	if _, err := run(t, "config", "set", "cert_provider", "bogus"); err == nil {
		t.Fatal("want error for invalid cert_provider value")
	}
}

// TestScaleCommand exercises scaleCmd's flag matrix: cpu-only, all three
// flags together, and no flags at all (rejected before hitting the API).
func TestScaleCommand(t *testing.T) {
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

	out, err := run(t, "scale", "web", "--project", "p", "--cpu", "250m")
	if err != nil {
		t.Fatalf("cpu-only scale: %v (%s)", err, out)
	}
	if !strings.Contains(out, "cpu=250m") || strings.Contains(out, "replicas=") {
		t.Fatalf("cpu-only output: %q", out)
	}

	out, err = run(t, "scale", "web", "--project", "p", "--replicas", "3", "--cpu", "500m", "--memory", "512Mi")
	if err != nil {
		t.Fatalf("all-three scale: %v (%s)", err, out)
	}
	for _, want := range []string{"replicas=3", "cpu=500m", "memory=512Mi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("all-three output missing %q: %q", want, out)
		}
	}

	if _, err := run(t, "scale", "web", "--project", "p"); err == nil {
		t.Fatal("want error when no scale flags are given")
	}
}

// TestAutoscaleCommand exercises autoscaleCmd's flag matrix: set, show, and
// --off.
func TestAutoscaleCommand(t *testing.T) {
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
	if _, err := run(t, "scale", "web", "--project", "p", "--cpu", "250m"); err != nil {
		t.Fatal(err)
	}

	// Show, off by default.
	out, err := run(t, "autoscale", "web", "--project", "p")
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	if !strings.Contains(out, "autoscale: off") {
		t.Fatalf("want 'autoscale: off', got %q", out)
	}

	// Set.
	out, err = run(t, "autoscale", "web", "--project", "p", "--min", "1", "--max", "5", "--cpu", "70")
	if err != nil {
		t.Fatalf("set: %v (%s)", err, out)
	}
	if !strings.Contains(out, "1-5") || !strings.Contains(out, "70%") {
		t.Fatalf("set output: %q", out)
	}

	// Show after set.
	out, err = run(t, "autoscale", "web", "--project", "p")
	if err != nil {
		t.Fatalf("show after set: %v (%s)", err, out)
	}
	if !strings.Contains(out, "1-5") || !strings.Contains(out, "70%") {
		t.Fatalf("show after set output: %q", out)
	}

	// Partial flags (missing --cpu) rejected.
	if _, err := run(t, "autoscale", "web", "--project", "p", "--min", "1", "--max", "5"); err == nil {
		t.Fatal("want error when min/max/cpu aren't all set together")
	}

	// Off.
	out, err = run(t, "autoscale", "web", "--project", "p", "--off")
	if err != nil {
		t.Fatalf("off: %v (%s)", err, out)
	}
	if !strings.Contains(out, "autoscale off") {
		t.Fatalf("off output: %q", out)
	}
	out, err = run(t, "autoscale", "web", "--project", "p")
	if err != nil {
		t.Fatalf("show after off: %v (%s)", err, out)
	}
	if !strings.Contains(out, "autoscale: off") {
		t.Fatalf("want 'autoscale: off' after --off, got %q", out)
	}
}

// TestHealthCommand exercises healthCmd's flag matrix: --path sets, --off
// clears, and neither/both are rejected before hitting the API.
func TestHealthCommand(t *testing.T) {
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

	out, err := run(t, "health", "web", "--project", "p", "--path", "/healthz")
	if err != nil {
		t.Fatalf("health --path: %v (%s)", err, out)
	}
	if !strings.Contains(out, "health check: /healthz") {
		t.Fatalf("--path output: %q", out)
	}

	out, err = run(t, "health", "web", "--project", "p", "--off")
	if err != nil {
		t.Fatalf("health --off: %v (%s)", err, out)
	}
	if !strings.Contains(out, "health check: off") {
		t.Fatalf("--off output: %q", out)
	}

	if _, err := run(t, "health", "web", "--project", "p"); err == nil {
		t.Fatal("want error when neither --path nor --off is given")
	}
	if _, err := run(t, "health", "web", "--project", "p", "--path", "/healthz", "--off"); err == nil {
		t.Fatal("want error when both --path and --off are given")
	}
}

func TestSSHKeyCommands(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}

	pubPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(pubPath, []byte(testSSHPubKey(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "ssh-key", "add", pubPath, "--name", "laptop")
	if err != nil {
		t.Fatalf("ssh-key add: %v (%s)", err, out)
	}
	if !strings.Contains(out, "SHA256:") {
		t.Fatalf("add output missing fingerprint: %s", out)
	}

	out, err = run(t, "ssh-key", "list")
	if err != nil {
		t.Fatalf("ssh-key list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "laptop") {
		t.Fatalf("list missing key: %s", out)
	}

	if _, err := run(t, "ssh-key", "remove", "1"); err != nil {
		t.Fatalf("ssh-key remove: %v", err)
	}

	out, err = run(t, "ssh-key", "list")
	if err != nil {
		t.Fatalf("ssh-key list after remove: %v (%s)", err, out)
	}
	if strings.Contains(out, "laptop") {
		t.Fatalf("key not removed: %s", out)
	}
}

// tokenIDs extracts the ID column from `token list`'s tabwriter output.
func tokenIDs(out string) map[string]bool {
	ids := map[string]bool{}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) > 0 {
			ids[fields[0]] = true
		}
	}
	return ids
}

func TestTokenAndRollbackCommands(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "token", "list")
	if err != nil {
		t.Fatalf("token list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "login") {
		t.Fatalf("want login-created token name, got %q", out)
	}
	before := tokenIDs(out)

	// Create a second token directly via the API (not through the CLI's
	// saved config), so the CLI's active session survives untouched.
	if _, err := client.New(srv.URL, "").Login("root@b.co", "pw123456"); err != nil {
		t.Fatalf("second login: %v", err)
	}

	out, err = run(t, "token", "list")
	if err != nil {
		t.Fatalf("token list after second login: %v (%s)", err, out)
	}
	after := tokenIDs(out)
	var newID string
	for id := range after {
		if !before[id] {
			newID = id
			break
		}
	}
	if newID == "" {
		t.Fatalf("did not find new token id; before=%v after=%v", before, after)
	}

	if _, err := run(t, "token", "revoke", newID); err != nil {
		t.Fatalf("token revoke: %v", err)
	}

	out, err = run(t, "token", "list")
	if err != nil {
		t.Fatalf("token list after revoke: %v (%s)", err, out)
	}
	if tokenIDs(out)[newID] {
		t.Fatalf("revoked token still listed: %s", out)
	}
	if !strings.Contains(out, "login") {
		t.Fatalf("session token should survive revoke of the other token: %s", out)
	}

	// rollback requires kube; testEnv has none, so it should surface the
	// server's kubernetes_unavailable error.
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "web", "--project", "p", "--port", "8080"); err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "rollback", "web", "--project", "p")
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes error, got %v", err)
	}
}

func TestInviteCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "invite", "create", "--role", "member")
	if err != nil {
		t.Fatalf("invite create: %v (%s)", err, out)
	}
	if !strings.Contains(out, "/ui/register?token=") {
		t.Fatalf("create output missing link: %s", out)
	}

	out, err = run(t, "invite", "list")
	if err != nil {
		t.Fatal(err)
	}
	tok := ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && len(fields[0]) == 32 {
			tok = fields[0]
		}
	}
	if tok == "" {
		t.Fatalf("no token in list output:\n%s", out)
	}

	if out, err = run(t, "invite", "revoke", tok); err != nil {
		t.Fatalf("revoke: %v (%s)", err, out)
	}
	out, err = run(t, "invite", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, tok) {
		t.Fatalf("revoked invite still listed:\n%s", out)
	}
}

// TestInviteCreateEmailWarning: --email against a server without SMTP
// configured still creates the invite and prints the warning.
func TestInviteCreateEmailWarning(t *testing.T) {
	srv := testEnv(t)
	if out, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatalf("login: %v (%s)", err, out)
	}

	out, err := run(t, "invite", "create", "--role", "member", "--email", "new@b.co")
	if err != nil {
		t.Fatalf("invite create --email: %v (%s)", err, out)
	}
	if !strings.Contains(out, "/ui/register?token=") {
		t.Fatalf("missing invite link:\n%s", out)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "smtp is not configured") {
		t.Fatalf("want unconfigured-SMTP warning, got:\n%s", out)
	}
}

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

// TestAddonCommands exercises the CLI wiring for addon commands. testEnv has
// no kube, so provisioning surfaces the server's kubernetes_unavailable
// error — the same honest, no-cluster-needed check other kube-dependent
// commands rely on. Wire-level behavior (create/attach/env injection) is
// covered by internal/server/addons_test.go.
func TestAddonCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "addon", "list", "--project", "p")
	if err != nil {
		t.Fatalf("addon list: %v (%s)", err, out)
	}
	if !strings.Contains(out, "NAME") {
		t.Fatalf("want header, got %q", out)
	}

	_, err = run(t, "addon", "create", "postgres", "--project", "p")
	if err == nil || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("want kubernetes error, got %v", err)
	}
}

// registryTestEnv is testEnv plus a reachable (empty-catalog) fake registry
// HTTP server wired in as RegistryHost, so `registry gc` can complete
// against the default unreachable registry.luncur-system:5000 host that a
// plain testEnv leaves in place.
func registryTestEnv(t *testing.T) *httptest.Server {
	t.Helper()
	regMux := http.NewServeMux()
	regMux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"repositories":[]}`)
	})
	reg := httptest.NewServer(regMux)
	t.Cleanup(reg.Close)

	t.Setenv("LUNCUR_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("root@b.co", "pw123456", "admin"); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{
		Store: st, Sealer: sealer, DataDir: t.TempDir(),
		RegistryHost: strings.TrimPrefix(reg.URL, "http://"),
	}))
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv
}

// TestEjectAndRegistryCommands exercises `app eject` and `registry gc`'s
// CLI wiring end to end against a real (in-process) server. Kube-dependent
// behavior (guard matrix, blob reclamation) is covered by
// internal/server/eject_test.go and registrygc_test.go.
func TestEjectAndRegistryCommands(t *testing.T) {
	srv := registryTestEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "web", "--project", "p", "--port", "8080"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "app", "eject", "web", "--project", "p", "--yes")
	if err != nil {
		t.Fatalf("eject: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind:") {
		t.Fatalf("want rendered YAML in output, got %q", out)
	}

	_, err = run(t, "app", "eject", "web", "--project", "p", "--yes")
	if err == nil || !strings.Contains(err.Error(), "ejected") {
		t.Fatalf("want ejected error on second eject, got %v", err)
	}

	out, err = run(t, "registry", "gc")
	if err != nil {
		t.Fatalf("registry gc: %v (%s)", err, out)
	}
	if !strings.Contains(out, "deleted 0") {
		t.Fatalf("want deleted count in output, got %q", out)
	}
}

func TestBackupCommands(t *testing.T) {
	srv := testEnv(t)
	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "backup", "create", "--no-upload")
	if err != nil {
		t.Fatalf("backup create: %v (%s)", err, out)
	}
	if !strings.Contains(out, ".tar.gz") {
		t.Fatalf("create output missing archive path: %s", out)
	}

	out, err = run(t, "backup", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "luncur-") {
		t.Fatalf("list missing backup: %s", out)
	}

	out, err = run(t, "backup", "prune")
	if err != nil {
		t.Fatalf("prune: %v (%s)", err, out)
	}
	if !strings.Contains(out, "removed") {
		t.Fatalf("prune output: %s", out)
	}
}
