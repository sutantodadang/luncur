package up

import (
	"strings"
	"testing"
)

func TestTraefikACMEConfig(t *testing.T) {
	o, err := TraefikACMEConfig("ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "HelmChartConfig" {
		t.Fatalf("kind = %s", o.Kind)
	}
	s := string(o.JSON)
	for _, want := range []string{
		"kube-system", "certificatesresolvers.le.acme", "ops@example.com", "acme.json",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}

func TestClusterIssuer(t *testing.T) {
	o, err := ClusterIssuer("ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "ClusterIssuer" {
		t.Fatalf("kind = %s", o.Kind)
	}
	s := string(o.JSON)
	for _, want := range []string{
		"luncur-le", "http01", "traefik", "ops@example.com", "letsencrypt",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}
