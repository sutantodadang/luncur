package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
)

// TestUINodesPage checks the admin sees the seeded node's name/role/status,
// a member gets 404, and a nil kube client renders the "not configured"
// message instead of erroring.
func TestUINodesPage(t *testing.T) {
	st := newTestStore(t)
	cp := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cp1",
			Labels: map[string]string{"node-role.kubernetes.io/control-plane": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "1.2.3.4"}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.32.5+k3s1"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	cs := k8sfake.NewSimpleClientset(cp)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kube.NewForTest(nil, cs)})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.CreateUser("m@b.co", "password123", "member")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes: want 200, got %d", status)
	}
	if !strings.Contains(body, "cp1") {
		t.Fatalf("nodes page missing node name, got: %s", body)
	}
	if !strings.Contains(body, "control-plane") {
		t.Fatalf("nodes page missing role, got: %s", body)
	}
	if !strings.Contains(body, "ready") {
		t.Fatalf("nodes page missing status, got: %s", body)
	}
	if !strings.Contains(body, "4000m") {
		t.Fatalf("nodes page missing cpu capacity, got: %s", body)
	}

	status, _ = getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, member.ID))
	if status != http.StatusNotFound {
		t.Fatalf("member GET /ui/nodes: want 404, got %d", status)
	}
}

// TestUINodesPageGPUProviderSurface checks the nodes page renders both the
// vast.ai key form and the Nebius creds form (provider select + all five
// fields) when neither provider is configured yet, that the rent form
// carries the provider toggle and both CLI-echo lines, and — the security
// property that matters most here — that submitting the Nebius creds form
// never renders the raw PEM bytes back into the page.
func TestUINodesPageGPUProviderSurface(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	sessionCk := uiSessionCookie(t, st, admin.ID)

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes", sessionCk)
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes: want 200, got %d", status)
	}

	// vast.ai key form (unchanged, back-compat).
	if !strings.Contains(body, `name="api_key"`) {
		t.Fatalf("nodes page missing vast.ai key field, got: %s", body)
	}
	if !strings.Contains(body, `action="/ui/gpu/key"`) {
		t.Fatalf("nodes page missing vast.ai key form action, got: %s", body)
	}

	// Nebius creds form: all five fields + its own action + CLI-echo. The
	// rent controls (provider select, offer table, Nebius rent form) only
	// appear once at least one provider is configured, so they aren't on
	// this pre-configuration render.
	if !strings.Contains(body, `action="/ui/gpu/key/nebius"`) {
		t.Fatalf("nodes page missing nebius key form action, got: %s", body)
	}
	for _, field := range []string{`name="sa_id"`, `name="pubkey_id"`, `name="private_key"`, `name="parent_id"`, `name="subnet_id"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("nodes page missing nebius field %q, got: %s", field, body)
		}
	}
	if !strings.Contains(body, "luncur gpu key --provider nebius") {
		t.Fatalf("nodes page missing nebius key CLI-echo, got: %s", body)
	}
	if strings.Contains(body, `id="gpu-provider-select"`) {
		t.Fatalf("rent controls should be hidden before any provider is configured, got: %s", body)
	}

	// Submit the Nebius creds form with a distinctive PEM marker, then
	// re-fetch the page: the marker must never appear back in the HTML.
	const pemMarker = "SUPERSECRETPEMMARKERXYZ"
	csrfCk := uiCSRF(t, client, srv.URL)
	form := url.Values{
		"sa_id": {"sa-1"}, "pubkey_id": {"kid-1"},
		"private_key": {"-----BEGIN PRIVATE KEY-----\n" + pemMarker + "\n-----END PRIVATE KEY-----"},
		"parent_id":   {"proj-1"}, "subnet_id": {"subnet-1"},
	}
	resp := uiPost(t, client, srv.URL+"/ui/gpu/key/nebius", csrfCk, sessionCk, form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST nebius key: want 303, got %d", resp.StatusCode)
	}

	status, body = getUIPage(t, client, srv.URL, "/ui/nodes", sessionCk)
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes after nebius key: want 200, got %d", status)
	}
	if strings.Contains(body, pemMarker) {
		t.Fatalf("nodes page must never echo the raw PEM contents, got: %s", body)
	}
	// Once configured, the creds form (and its plaintext inputs) is hidden,
	// and the rent controls (provider select + Nebius rent form/CLI-echo)
	// now appear.
	if strings.Contains(body, `action="/ui/gpu/key/nebius"`) {
		t.Fatalf("nodes page should hide the nebius creds form once configured, got: %s", body)
	}
	if !strings.Contains(body, `id="gpu-provider-select"`) {
		t.Fatalf("nodes page missing provider select toggle once configured, got: %s", body)
	}
	if !strings.Contains(body, "luncur gpu rent --provider nebius") {
		t.Fatalf("nodes page missing nebius rent CLI-echo once configured, got: %s", body)
	}
}

// TestUINodesPageNoKube asserts a nil kube client renders the "not
// configured" message instead of erroring.
func TestUINodesPageNoKube(t *testing.T) {
	st := newTestStore(t)
	srv := newHTTPTest(t, Deps{Store: st})
	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()

	status, body := getUIPage(t, client, srv.URL, "/ui/nodes", uiSessionCookie(t, st, admin.ID))
	if status != http.StatusOK {
		t.Fatalf("admin GET /ui/nodes: want 200, got %d", status)
	}
	if !strings.Contains(body, "kubernetes is not configured") {
		t.Fatalf("nodes page missing not-configured message, got: %s", body)
	}
}
