package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
)

// recordedAction captures verb/resource/namespace/name for every dynamic
// client action, patch AND delete — recordingKube (panel_test.go/
// certs_test.go) only records patches, but network_isolation=off tears the
// isolation NetworkPolicy down via delete, so this file needs both. Mirrors
// kube_test.go's own `recorded` fixture in the kube package.
type recordedAction struct {
	verb      string
	resource  string
	namespace string
	name      string
}

func recordingActionsKube(t *testing.T) (*kube.Client, *[]recordedAction) {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)
	var log []recordedAction
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		rec := recordedAction{verb: a.GetVerb(), resource: a.GetResource().Resource, namespace: a.GetNamespace()}
		switch act := a.(type) {
		case ktesting.PatchAction:
			rec.name = act.GetName()
		case ktesting.DeleteAction:
			rec.name = act.GetName()
		}
		log = append(log, rec)
		return true, nil, nil // short-circuit: assert on actions, not state
	})
	return kube.NewFromDynamic(dyn), &log
}

func hasAction(log []recordedAction, verb, resource, namespace, name string) bool {
	for _, a := range log {
		if a.verb == verb && a.resource == resource && a.namespace == namespace && a.name == name {
			return true
		}
	}
	return false
}

// TestNetworkIsolationValidation exercises settableKeys' network_isolation
// rule through setSetting, mirroring TestPanelDomainSetting's shape.
func TestNetworkIsolationValidation(t *testing.T) {
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})

	if err := srv.setSetting("network_isolation", "maybe"); !errors.Is(err, errInvalidSettingValue) {
		t.Fatalf("setSetting(maybe) = %v, want errInvalidSettingValue", err)
	}
	if err := srv.setSetting("network_isolation", "on"); err != nil {
		t.Fatalf("setSetting(on): %v", err)
	}
	if err := srv.setSetting("network_isolation", "off"); err != nil {
		t.Fatalf("setSetting(off): %v", err)
	}
}

// TestEnsureProjectNamespaceAppliesIsolationWhenOn covers the choke-point
// every lazily-created project namespace goes through: with the setting on,
// ensureProjectNamespace must apply the luncur-isolation NetworkPolicy right
// alongside the namespace itself.
func TestEnsureProjectNamespaceAppliesIsolationWhenOn(t *testing.T) {
	st := newTestStore(t)
	kubeClient, log := recordingActionsKube(t)
	srv := newServer(Deps{Store: st, Kube: kubeClient})

	if err := st.SetSetting("network_isolation", "on"); err != nil {
		t.Fatal(err)
	}
	if err := srv.ensureProjectNamespace(context.Background(), "luncur-demo"); err != nil {
		t.Fatal(err)
	}
	if !hasAction(*log, "patch", "networkpolicies", "luncur-demo", "luncur-isolation") {
		t.Fatalf("no isolation policy applied: %+v", *log)
	}
}

// TestEnsureProjectNamespaceSkipsIsolationWhenOff is the same path with the
// setting off: the namespace is still stamped, but no NetworkPolicy touches
// the cluster.
func TestEnsureProjectNamespaceSkipsIsolationWhenOff(t *testing.T) {
	st := newTestStore(t)
	kubeClient, log := recordingActionsKube(t)
	srv := newServer(Deps{Store: st, Kube: kubeClient})

	if err := st.SetSetting("network_isolation", "off"); err != nil {
		t.Fatal(err)
	}
	if err := srv.ensureProjectNamespace(context.Background(), "luncur-demo"); err != nil {
		t.Fatal(err)
	}
	if hasAction(*log, "patch", "networkpolicies", "luncur-demo", "luncur-isolation") {
		t.Fatalf("isolation policy applied despite setting off: %+v", *log)
	}
}

// TestNetworkIsolationChangedFansOutToProjects covers networkIsolationChanged
// end to end through the JSON settings API (the panel_domain hook's twin):
// toggling network_isolation off removes the policy from every existing
// project's namespace, and toggling it back on re-applies it.
func TestNetworkIsolationChangedFansOutToProjects(t *testing.T) {
	st := newTestStore(t)
	kubeClient, log := recordingActionsKube(t)
	srv := newHTTPTest(t, Deps{Store: st, Kube: kubeClient})
	admin := seedUserToken(t, st, "root@b.co", "admin")

	p, err := st.CreateProject("demo")
	if err != nil {
		t.Fatal(err)
	}

	// Fresh store seeds network_isolation=on (no projects existed at Open
	// time) — flipping it off here must remove the policy from the project
	// created above.
	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/network_isolation", admin, `{"value":"off"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set off: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !hasAction(*log, "delete", "networkpolicies", p.Namespace, "luncur-isolation") {
		t.Fatalf("no isolation policy removal recorded: %+v", *log)
	}

	*log = nil
	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/network_isolation", admin, `{"value":"on"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set on: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if !hasAction(*log, "patch", "networkpolicies", p.Namespace, "luncur-isolation") {
		t.Fatalf("no isolation policy apply recorded: %+v", *log)
	}
}
