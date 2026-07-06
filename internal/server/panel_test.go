package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/acme/acmetest"
	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
)

func TestValidPanelDomain(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", true},
		{"panel.example.com", true},
		{"*.x.com", false},
		{"UPPER.example.com", false},
		{"not a host!", false},
	}
	for _, c := range cases {
		if got := validPanelDomain(c.v); got != c.want {
			t.Errorf("validPanelDomain(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// TestPanelDomainSetting exercises the same rule through setSetting (the
// path handleSetSetting and the UI form both use), not just the validator
// function directly.
func TestPanelDomainSetting(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})

	if err := srv.setSetting("panel_domain", "panel.example.com"); err != nil {
		t.Fatalf("valid hostname rejected: %v", err)
	}
	if err := srv.setSetting("panel_domain", ""); err != nil {
		t.Fatalf("clearing rejected: %v", err)
	}
	for _, bad := range []string{"*.x.com", "UPPER.example.com", "not a host!"} {
		if err := srv.setSetting("panel_domain", bad); !errors.Is(err, errInvalidSettingValue) {
			t.Errorf("setSetting(%q) = %v, want errInvalidSettingValue", bad, err)
		}
	}
}

// recordingKube builds a fake dynamic client (patches recorded into the
// returned slice pointer) plus a fake typed clientset, following the same
// shape as certs_test.go's certTestServer fixture.
func recordingKube(t *testing.T) (*kube.Client, *[]recordedPatch) {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	cs := k8sfake.NewSimpleClientset()
	var patches []recordedPatch
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if pa, ok := a.(ktesting.PatchAction); ok {
			patches = append(patches, recordedPatch{
				resource:  a.GetResource().Resource,
				namespace: a.GetNamespace(),
				name:      pa.GetName(),
				raw:       pa.GetPatch(),
			})
		}
		return true, nil, nil
	})
	return kube.NewForTest(dyn, cs), &patches
}

// TestSetPanelDomainAppliesIngress covers panelDomainChanged's write path
// end to end through the JSON settings API: setting panel_domain adds the
// custom host to luncur's own Ingress; clearing it removes it again.
func TestSetPanelDomainAppliesIngress(t *testing.T) {
	st := newTestStore(t)
	kubeClient, patches := recordingKube(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer, Kube: kubeClient, ExternalIP: "1.2.3.4"})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/panel_domain", admin, `{"value":"panel.example.com"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set panel_domain: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if !hasPatch(*patches, "ingresses", "luncur-system", "luncur",
		`"host":"panel.1.2.3.4.sslip.io"`, `"host":"panel.example.com"`) {
		t.Fatalf("ingress missing both hosts: %+v", *patches)
	}

	*patches = nil
	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/panel_domain", admin, `{"value":""}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear panel_domain: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	found := false
	for _, p := range *patches {
		if p.resource == "ingresses" && p.name == "luncur" {
			found = true
			if strings.Contains(string(p.raw), "panel.example.com") {
				t.Fatalf("ingress still has custom host after clearing: %s", p.raw)
			}
		}
	}
	if !found {
		t.Fatal("no ingress patch applied after clearing panel_domain")
	}
}

// TestPanelCertIssue mirrors certs_test.go's offline-ACME fixture
// (TestCertIssueNotifiesOnSuccess) for one panel issuance: issuePanel must
// flip panel_cert_status to issued, write the panel TLS secret, and
// re-apply the Ingress with a tls block over the custom host.
func TestPanelCertIssue(t *testing.T) {
	st := newTestStore(t)
	kubeClient, patches := recordingKube(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{Store: st, Sealer: sealer, Kube: kubeClient, ExternalIP: "1.2.3.4"})
	if err := st.SetSetting("panel_domain", "panel.example.com"); err != nil {
		t.Fatal(err)
	}

	mux := httptest.NewServer(srv.handler())
	t.Cleanup(mux.Close)
	fakeDir := acmetest.New(t, strings.TrimPrefix(mux.URL, "http://"))
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.certs.issuePanel(ctx, "panel.example.com")

	status, err := st.GetSetting("panel_cert_status")
	if err != nil {
		t.Fatal(err)
	}
	if status != "issued" {
		certErr, _ := st.GetSetting("panel_cert_error")
		t.Fatalf("panel_cert_status = %q, want issued (error %q)", status, certErr)
	}
	exp, err := st.GetSetting("panel_cert_expires_at")
	if err != nil || exp == "" {
		t.Fatalf("panel_cert_expires_at not set: err=%v", err)
	}

	if !hasPatch(*patches, "secrets", "luncur-system", panelTLSSecret, `"type":"kubernetes.io/tls"`) {
		t.Fatalf("no applied panel TLS secret found: %+v", *patches)
	}
	if !hasPatch(*patches, "ingresses", "luncur-system", "luncur", `"secretName":"`+panelTLSSecret+`"`) {
		t.Fatalf("ingress missing tls block after issuance: %+v", *patches)
	}
}

// TestPanelSweepRenews: a panel_domain with an expired issued cert must be
// re-enqueued by sweep().
func TestPanelSweepRenews(t *testing.T) {
	st := newTestStore(t)
	kubeClient, _ := recordingKube(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(Deps{Store: st, Sealer: sealer, Kube: kubeClient, ExternalIP: "1.2.3.4"})

	if err := st.SetSetting("panel_domain", "panel.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("panel_cert_status", "issued"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("panel_cert_expires_at", time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	srv.certs.sweep(context.Background())

	select {
	case j := <-srv.certs.jobs:
		if j.panelHost != "panel.example.com" {
			t.Fatalf("job = %+v, want panelHost panel.example.com", j)
		}
	default:
		t.Fatal("sweep did not enqueue panel renewal job")
	}
}
