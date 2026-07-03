package server

import (
	"context"
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
	"github.com/sutantodadang/luncur/internal/store"
)

// recordedPatch captures one server-side-apply patch the cert manager sent
// through the fake dynamic client, so tests can inspect the object it
// applied (not just which verb/resource was hit).
type recordedPatch struct {
	resource  string
	namespace string
	name      string
	raw       []byte
}

// certTestServer builds a *server wired with a fake dynamic client (patches
// recorded) and a fake typed clientset (so GetSecretData/accountKey work),
// following the same fixture shape as apps_test.go's kubeServer and
// build_test.go's buildServer.
func certTestServer(t *testing.T) (*server, *store.Store, *[]recordedPatch) {
	t.Helper()
	st := newTestStore(t)
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
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{
		Store: st, Sealer: sealer, Kube: kube.NewForTest(dyn, cs), ExternalIP: "1.2.3.4",
	})
	return s, st, &patches
}

func seedDomain(t *testing.T, st *store.Store, hostname string) (store.Project, store.App, store.Domain) {
	t.Helper()
	p, err := st.CreateProject("proj")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "web", 8080)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.AddDomain(a.ID, hostname)
	if err != nil {
		t.Fatal(err)
	}
	return p, a, d
}

func hasPatch(patches []recordedPatch, resource, namespace, name string, contains ...string) bool {
	for _, p := range patches {
		if p.resource != resource || p.namespace != namespace || p.name != name {
			continue
		}
		ok := true
		for _, c := range contains {
			if !strings.Contains(string(p.raw), c) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// pollDomain polls until the app's single domain reaches one of the given
// terminal statuses, or fails the test after deadline.
func pollDomain(t *testing.T, st *store.Store, appID int64, deadline time.Duration, terminal ...string) store.Domain {
	t.Helper()
	end := time.Now().Add(deadline)
	var got store.Domain
	for {
		list, err := st.ListDomains(appID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			got = list[0]
			for _, want := range terminal {
				if got.CertStatus == want {
					return got
				}
			}
		}
		if time.Now().After(end) {
			t.Fatalf("domain did not reach a terminal status in time, stuck at %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCertManagerIssuesAndRenews(t *testing.T) {
	srv, st, patches := certTestServer(t)
	p, a, d := seedDomain(t, st, "www.example.com")

	// Mount srv's own mux (it routes the challenge path through
	// srv.certs.Challenges()) behind an httptest server, and point the fake
	// ACME directory's challenge validation at it.
	mux := httptest.NewServer(srv.handler())
	t.Cleanup(mux.Close)
	fakeDir := acmetest.New(t, strings.TrimPrefix(mux.URL, "http://"))
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go srv.certs.Run(ctx)

	srv.certs.Kick(p, a, d)

	got := pollDomain(t, st, a.ID, 15*time.Second, "issued", "failed")
	if got.CertStatus != "issued" {
		t.Fatalf("cert_status = %q (error %q), want issued", got.CertStatus, got.CertError)
	}
	if got.CertExpiresAt == "" {
		t.Fatal("cert_expires_at not set")
	}

	secretName := certSecretName(a.Name, d.Hostname)
	if !hasPatch(*patches, "secrets", p.Namespace, secretName,
		`"type":"kubernetes.io/tls"`, `"namespace":"`+p.Namespace+`"`) {
		t.Fatalf("no applied TLS secret %s/%s of type kubernetes.io/tls found in patches: %+v", p.Namespace, secretName, *patches)
	}

	if !hasPatch(*patches, "ingresses", "luncur-system", challengeIngress, `"host":"`+d.Hostname+`"`) {
		t.Fatalf("no applied challenge Ingress %s with host %s found in patches: %+v", challengeIngress, d.Hostname, *patches)
	}
}

func TestCertManagerFailureMarksDomain(t *testing.T) {
	srv, st, _ := certTestServer(t)
	p, a, d := seedDomain(t, st, "www.example.com")

	// Point the fake ACME's challenge validation at an unreachable host so
	// HTTP-01 validation never succeeds — the authorization stays pending
	// forever and issuance fails once its context deadline elapses.
	fakeDir := acmetest.New(t, "127.0.0.1:1")
	srv.certs.directoryURL = fakeDir.DirectoryURL()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go srv.certs.Run(ctx)

	srv.certs.Kick(p, a, d)

	got := pollDomain(t, st, a.ID, 15*time.Second, "issued", "failed")
	if got.CertStatus != "failed" {
		t.Fatalf("cert_status = %q, want failed", got.CertStatus)
	}
	if got.CertError == "" {
		t.Fatal("cert_error not set on failure")
	}
}
