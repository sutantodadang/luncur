package server

import (
	"context"
	"sort"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// Turning network_isolation on applies the policy to every environment
// namespace of every project, not only the production one.
func TestNetworkIsolationCoversEveryEnvNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var patched []string
	dyn.PrependReactor("patch", "networkpolicies", func(a ktesting.Action) (bool, runtime.Object, error) {
		patched = append(patched, a.GetNamespace())
		return true, nil, nil
	})
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "shop") // production + develop + staging
	_ = p
	if err := s.st.SetSetting("network_isolation", "on"); err != nil {
		t.Fatal(err)
	}
	if err := s.networkIsolationChanged(context.Background()); err != nil {
		t.Fatal(err)
	}
	sort.Strings(patched)
	want := []string{"luncur-shop", "luncur-shop-develop", "luncur-shop-staging"}
	if len(patched) != len(want) {
		t.Fatalf("isolation applied to %v, want all env namespaces %v", patched, want)
	}
}

// The project's CPU/memory budget is enforced in every environment
// namespace, not only the production one.
func TestProjectQuotaCoversEveryEnvNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	quotaNS := map[string]bool{}
	dyn.PrependReactor("*", "*", func(a ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	dyn.PrependReactor("patch", "resourcequotas", func(a ktesting.Action) (bool, runtime.Object, error) {
		quotaNS[a.GetNamespace()] = true
		return true, nil, nil
	})
	s := pipelineTestServer(t, dyn, nil)
	p := pipelineSeedProject(t, s.st, "shop")
	if err := s.setProjectQuota(context.Background(), p, 2000, 4096); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"luncur-shop", "luncur-shop-develop", "luncur-shop-staging"} {
		if !quotaNS[ns] {
			t.Errorf("no ResourceQuota applied in %s (applied in %v)", ns, quotaNS)
		}
	}
}
