package store

import "testing"

func TestSetOverrideRejectsDangerousDeploymentFields(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	dangerous := []string{
		`{"spec":{"template":{"spec":{"volumes":[{"name":"x","hostPath":{"path":"/"}}]}}}}`,
		`{"spec":{"template":{"spec":{"containers":[{"name":"api","securityContext":{"privileged":true}}]}}}}`,
		`{"spec":{"template":{"spec":{"serviceAccountName":"cluster-admin"}}}}`,
		`{"spec":{"template":{"spec":{"hostNetwork":true}}}}`,
		`{"spec":{"template":{"spec":{"hostPID":true}}}}`,
		`{"spec":{"template":{"spec":{"hostIPC":true}}}}`,
		`{"metadata":{"name":"other"}}`,
	}
	for _, patch := range dangerous {
		if err := s.SetOverride(a.ID, "Deployment", patch); err == nil {
			t.Errorf("patch %s: want error, got nil", patch)
		}
	}
}

func TestSetOverrideRejectsDangerousCronJobFields(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "nightly", 0, "cron", "0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}

	dangerous := []string{
		`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"volumes":[{"name":"x","hostPath":{"path":"/"}}]}}}}}}`,
		`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"app","securityContext":{"privileged":true}}]}}}}}}`,
		`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"serviceAccountName":"cluster-admin"}}}}}}`,
		`{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"hostNetwork":true}}}}}}`,
	}
	for _, patch := range dangerous {
		if err := s.SetOverride(a.ID, "CronJob", patch); err == nil {
			t.Errorf("patch %s: want error, got nil", patch)
		}
	}

	// The schedule itself stays overridable — it's the user's own app.
	if err := s.SetOverride(a.ID, "CronJob", `{"spec":{"schedule":"*/10 * * * *"}}`); err != nil {
		t.Errorf("benign schedule override: %v", err)
	}
}

func TestSetOverrideRejectsIngressHostHijack(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	patch := `{"spec":{"rules":[{"host":"evil.example.com"}]}}`
	if err := s.SetOverride(a.ID, "Ingress", patch); err == nil {
		t.Fatal("want error for spec.rules override on Ingress")
	}
}

func TestSetOverrideRejectsMetadataNameNamespaceOnAnyKind(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"Deployment", "Service", "Ingress"} {
		if err := s.SetOverride(a.ID, kind, `{"metadata":{"name":"other"}}`); err == nil {
			t.Errorf("kind %s: want error for metadata.name", kind)
		}
		if err := s.SetOverride(a.ID, kind, `{"metadata":{"namespace":"other-ns"}}`); err == nil {
			t.Errorf("kind %s: want error for metadata.namespace", kind)
		}
	}
}

func TestSetOverrideRejectsServiceExternalIPHijack(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	dangerous := []string{
		`{"spec":{"externalIPs":["1.2.3.4"]}}`,
		`{"spec":{"loadBalancerIP":"1.2.3.4"}}`,
	}
	for _, patch := range dangerous {
		if err := s.SetOverride(a.ID, "Service", patch); err == nil {
			t.Errorf("patch %s: want error, got nil", patch)
		}
	}

	benign := `{"spec":{"ports":[{"port":80}]}}`
	if err := s.SetOverride(a.ID, "Service", benign); err != nil {
		t.Errorf("benign service patch: want success, got %v", err)
	}
}

func TestSetOverrideAllowsBenignPatches(t *testing.T) {
	s := openTest(t)
	p, err := s.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApp(p.ID, "api", 3000, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	benign := []string{
		`{"spec":{"template":{"spec":{"containers":[{"name":"api","resources":{"limits":{"memory":"512Mi"}}}]}}}}`,
		`{"metadata":{"labels":{"team":"payments"}}}`,
	}
	for _, patch := range benign {
		if err := s.SetOverride(a.ID, "Deployment", patch); err != nil {
			t.Errorf("patch %s: want success, got %v", patch, err)
		}
	}
}
