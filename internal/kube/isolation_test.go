package kube

import (
	"encoding/json"
	"testing"

	netv1 "k8s.io/api/networking/v1"
)

// S13: the luncur-system peer admits only the luncur server pod, not every
// pod there (user BuildKit builds run in luncur-system too).
func TestIsolationPolicyNarrowsLuncurSystemPeer(t *testing.T) {
	var np netv1.NetworkPolicy
	if err := json.Unmarshal([]byte(luncurIsolationPolicy), &np); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, peer := range np.Spec.Ingress[0].From {
		if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "luncur-system" {
			continue
		}
		found = true
		if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "luncur" {
			t.Fatalf("luncur-system peer admits every pod: %+v", peer)
		}
	}
	if !found {
		t.Fatal("no luncur-system peer (the panel's proxies need it)")
	}
}
