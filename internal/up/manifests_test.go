package up

import (
	"strings"
	"testing"
)

func TestLuncurObjects(t *testing.T) {
	objs, err := LuncurObjects(Params{
		Image: "ghcr.io/sutantodadang/luncur:v1", ExternalIP: "1.2.3.4",
		BuilderImage: "luncur/builder:v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o.Kind] = true
	}
	for _, k := range []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Deployment", "Service", "Ingress"} {
		if !kinds[k] {
			t.Fatalf("missing kind %s", k)
		}
	}
	all := ""
	for _, o := range objs {
		all += string(o.JSON)
	}
	for _, want := range []string{
		"panel.1.2.3.4.sslip.io",
		"ghcr.io/sutantodadang/luncur:v1",
		"$(BOOTSTRAP_ADMIN)",
		"luncur-bootstrap",
		"/v1/health",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
	if strings.Contains(all, "cluster-admin") {
		t.Fatal("cluster-admin binding must be gone")
	}
	for _, want := range []string{`"ClusterRole"`, "pods/log", "helmchartconfigs", "clusterissuers", "pods/exec", "statefulsets", "metrics.k8s.io", "cronjobs", "persistentvolumeclaims"} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
	for _, want := range []string{
		`"--ssh-listen"`,
		`"nodePort":30022`,
		`"luncur-ssh"`,
		`"containerPort":2222`,
		`"--cert-provider"`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
	if PanelHost("1.2.3.4") != "panel.1.2.3.4.sslip.io" {
		t.Fatal("PanelHost")
	}
}

func TestPanelIngress(t *testing.T) {
	// No custom host: single sslip.io rule, no tls block — must match what
	// LuncurObjects itself renders.
	base, err := PanelIngress("1.2.3.4", "", "")
	if err != nil {
		t.Fatal(err)
	}
	baseJSON := string(base.JSON)
	if !strings.Contains(baseJSON, "panel.1.2.3.4.sslip.io") {
		t.Fatalf("missing sslip host: %s", baseJSON)
	}
	if strings.Contains(baseJSON, `"tls"`) {
		t.Fatalf("unexpected tls block with no custom host: %s", baseJSON)
	}

	objs, err := LuncurObjects(Params{Image: "img", ExternalIP: "1.2.3.4", BuilderImage: "b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.Kind == "Ingress" && string(o.JSON) != baseJSON {
			t.Fatalf("LuncurObjects Ingress diverged from PanelIngress base case:\n%s\nvs\n%s", o.JSON, baseJSON)
		}
	}

	// Custom host + secret: both hosts present, tls block covers only the
	// custom host.
	custom, err := PanelIngress("1.2.3.4", "panel.example.com", "luncur-panel-tls")
	if err != nil {
		t.Fatal(err)
	}
	customJSON := string(custom.JSON)
	for _, want := range []string{
		"panel.1.2.3.4.sslip.io",
		"panel.example.com",
		`"secretName":"luncur-panel-tls"`,
	} {
		if !strings.Contains(customJSON, want) {
			t.Fatalf("missing %q in: %s", want, customJSON)
		}
	}
}
