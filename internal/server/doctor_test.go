package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
)

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	}
}

func readyTraefikPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kube-system",
			Labels: map[string]string{"app.kubernetes.io/name": "traefik"},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
}

type doctorResponse struct {
	ServerVersion string        `json:"server_version"`
	Checks        []doctorCheck `json:"checks"`
}

func doctorChecks(t *testing.T, resp *http.Response) doctorResponse {
	t.Helper()
	defer resp.Body.Close()
	var out doctorResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkByName(t *testing.T, checks []doctorCheck, name string) doctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return doctorCheck{}
}

// TestDoctorAllHealthy seeds every dependency doctor probes as healthy: a
// ready node, a ready traefik pod, a reachable registry, no stuck deploys,
// no domains, and every setting configured. It asserts all 9 checks come
// back "ok" in the fixed order the plan specifies, and server_version is set.
func TestDoctorAllHealthy(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{"proj/web": {"1"}})
	st := newTestStore(t)
	if err := st.SetSetting("smtp_host", "smtp.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_url", "https://example.com/hook"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("backup_schedule", "daily"); err != nil {
		t.Fatal(err)
	}
	cs := k8sfake.NewSimpleClientset(readyNode("n1"), readyTraefikPod("traefik-1"))
	srv := newHTTPTest(t, Deps{
		Store: st, RegistryHost: registryHost, Kube: kube.NewForTest(nil, cs), Version: "v1.2.3",
	})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	out := doctorChecks(t, resp)
	if out.ServerVersion != "v1.2.3" {
		t.Fatalf("server_version = %q, want v1.2.3", out.ServerVersion)
	}
	wantOrder := []string{"database", "kubernetes", "registry", "builds", "ingress",
		"certificates", "smtp", "notifications", "backups"}
	if len(out.Checks) != len(wantOrder) {
		t.Fatalf("checks = %+v, want %d entries", out.Checks, len(wantOrder))
	}
	for i, name := range wantOrder {
		if out.Checks[i].Name != name {
			t.Fatalf("checks[%d].Name = %q, want %q (order must be fixed)", i, out.Checks[i].Name, name)
		}
		if out.Checks[i].Status != "ok" {
			t.Errorf("check %s: status = %q, want ok (detail %q)", name, out.Checks[i].Status, out.Checks[i].Detail)
		}
	}
}

// TestDoctorNoKube asserts kube==nil fails both the kubernetes and ingress
// checks (each with the shared "not configured" phrasing) while every other
// check still runs and reports.
func TestDoctorNoKube(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	out := doctorChecks(t, resp)

	kubeCheck := checkByName(t, out.Checks, "kubernetes")
	if kubeCheck.Status != "fail" || kubeCheck.Detail != "kubernetes is not configured" {
		t.Fatalf("kubernetes check = %+v", kubeCheck)
	}
	ingressCheck := checkByName(t, out.Checks, "ingress")
	if ingressCheck.Status != "fail" || ingressCheck.Detail != "kubernetes is not configured" {
		t.Fatalf("ingress check = %+v", ingressCheck)
	}
	if len(out.Checks) != 9 {
		t.Fatalf("checks = %+v, want 9 entries even with kube nil", out.Checks)
	}
}

// TestDoctorRegistryUnreachable points registryHost at a closed port so the
// registry check fails while every other check is unaffected.
func TestDoctorRegistryUnreachable(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: "127.0.0.1:1"})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	out := doctorChecks(t, resp)

	regCheck := checkByName(t, out.Checks, "registry")
	if regCheck.Status != "fail" {
		t.Fatalf("registry check = %+v, want fail", regCheck)
	}
	dbCheck := checkByName(t, out.Checks, "database")
	if dbCheck.Status != "ok" {
		t.Fatalf("database check = %+v, want ok despite registry failure", dbCheck)
	}
}

// TestDoctorStuckBuild seeds a deployment stuck in 'building' for over 30
// minutes and asserts the builds check warns and names it.
func TestDoctorStuckBuild(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "building", "img:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`UPDATE deployments SET created_at = datetime('now', '-45 minutes') WHERE id = ?`, d.ID,
	); err != nil {
		t.Fatal(err)
	}

	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	out := doctorChecks(t, resp)

	buildsCheck := checkByName(t, out.Checks, "builds")
	if buildsCheck.Status != "warn" || !strings.Contains(buildsCheck.Detail, d.ID) {
		t.Fatalf("builds check = %+v, want warn mentioning deploy id %s", buildsCheck, d.ID)
	}
}

// TestDoctorFailingCert seeds a domain with cert_status='failed' and a
// cert_error message, and asserts the certificates check warns, names the
// hostname, but never leaks the cert_error text (which may contain internal
// ACME/detail noise not meant for a one-shot health summary).
func TestDoctorFailingCert(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	dom, err := st.AddDomain(a.ID, "broken.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDomainCert(dom.ID, "failed", "very secret internal acme detail", ""); err != nil {
		t.Fatal(err)
	}

	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	out := doctorChecks(t, resp)

	certCheck := checkByName(t, out.Checks, "certificates")
	if certCheck.Status != "warn" || !strings.Contains(certCheck.Detail, "broken.example.com") {
		t.Fatalf("certificates check = %+v, want warn naming the hostname", certCheck)
	}
	if strings.Contains(certCheck.Detail, "acme detail") {
		t.Fatalf("certificates check leaked cert_error text: %+v", certCheck)
	}
}

// TestDoctorUnsetSettings asserts smtp/notifications/backups all warn when
// their settings are unset.
func TestDoctorUnsetSettings(t *testing.T) {
	registryHost, _ := newFakeRegistryServer(t, map[string][]string{})
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st, RegistryHost: registryHost})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", admin, "")
	out := doctorChecks(t, resp)

	for _, name := range []string{"smtp", "notifications", "backups"} {
		c := checkByName(t, out.Checks, name)
		if c.Status != "warn" {
			t.Errorf("%s check = %+v, want warn", name, c)
		}
	}
}

// TestDoctorForbidsMember asserts a non-admin token gets 403.
func TestDoctorForbidsMember(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	member := seedUserToken(t, st, "pleb@b.co", "member")

	resp := doAuthed(t, "GET", srv.URL+"/v1/doctor", member, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member: want 403, got %d", resp.StatusCode)
	}
}
