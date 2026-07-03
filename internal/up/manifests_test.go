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
	for _, want := range []string{`"ClusterRole"`, "pods/log", "helmchartconfigs", "clusterissuers"} {
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
