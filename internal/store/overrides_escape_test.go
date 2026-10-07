package store

import "testing"

// Overrides may not expose an app around the ingress/isolation boundary or
// borrow another identity: Service NodePort/LoadBalancer, Ingress
// defaultBackend (a cluster-wide catch-all), pod serviceAccount (alias of the
// blocked serviceAccountName).
func TestOverrideRejectsExposureEscapes(t *testing.T) {
	cases := []struct{ kind, patch string }{
		{"Service", `{"spec":{"type":"LoadBalancer","ports":[{"port":22,"targetPort":8080}]}}`},
		{"Service", `{"spec":{"type":"NodePort"}}`},
		{"Ingress", `{"spec":{"defaultBackend":{"service":{"name":"web","port":{"number":80}}}}}`},
		{"Deployment", `{"spec":{"template":{"spec":{"serviceAccount":"builder"}}}}`},
	}
	s := openTest(t)
	p, _ := s.CreateProject("p")
	a, _ := s.CreateApp(p.ID, "web", 8080, "web", "")
	for _, c := range cases {
		if err := s.SetOverride(a.ID, c.kind, c.patch); err == nil {
			t.Errorf("%s override accepted: %s", c.kind, c.patch)
		}
	}
}
