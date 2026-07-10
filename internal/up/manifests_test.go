package up

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"

	"github.com/sutantodadang/luncur/internal/render"
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
	for _, want := range []string{`"ClusterRole"`, "pods/log", "helmchartconfigs", "clusterissuers", "pods/exec", "statefulsets", "metrics.k8s.io", "cronjobs", "persistentvolumeclaims", "horizontalpodautoscalers", "networkpolicies", "resourcequotas", "limitranges", "poddisruptionbudgets"} {
		if !strings.Contains(all, want) {
			t.Fatalf("manifests missing %q", want)
		}
	}
	// DeleteAppObjects removes per-run Jobs via DeleteCollection, which RBAC
	// gates behind its own verb — without it every app destroy 500s at the
	// Jobs step (Forbidden is not NotFound, so it isn't swallowed).
	if !strings.Contains(all, "deletecollection") {
		t.Fatal("manifests: batch jobs rule missing deletecollection verb")
	}
	// metrics.k8s.io must grant both pods and nodes, else node monitoring
	// (ListNodes -> NodeMetrics) is Forbidden and the nodes page never
	// leaves "collecting samples".
	if !strings.Contains(all, `"resources":["pods","nodes"]`) {
		t.Fatal(`manifests: metrics.k8s.io rule missing resources ["pods","nodes"]`)
	}
	// The self-heal escalate grant must be scoped to the "luncur" ClusterRole
	// by name, never a bare escalate on all clusterroles — that would let the
	// server's ServiceAccount grant itself arbitrary cluster-admin rules.
	if !strings.Contains(all, `"verbs":["get","update","patch","escalate"],"apiGroups":["rbac.authorization.k8s.io"],"resources":["clusterroles"],"resourceNames":["luncur"]`) {
		t.Fatal(`manifests: ClusterRole missing scoped self-heal escalate rule on resourceNames ["luncur"]`)
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

func deploymentFrom(t *testing.T, objs []render.Object) *appsv1.Deployment {
	t.Helper()
	for _, o := range objs {
		if o.Kind == "Deployment" {
			var dep appsv1.Deployment
			if err := json.Unmarshal(o.JSON, &dep); err != nil {
				t.Fatal(err)
			}
			return &dep
		}
	}
	t.Fatal("no Deployment in objs")
	return nil
}

func TestLuncurObjectsHardening(t *testing.T) {
	objs, err := LuncurObjects(Params{
		Image: "img", ExternalIP: "1.2.3.4", BuilderImage: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	dep := deploymentFrom(t, objs)
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %v, want Recreate", dep.Spec.Strategy.Type)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1 (no sidecar without ReplicaURL)", len(dep.Spec.Template.Spec.Containers))
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil ||
		c.LivenessProbe.HTTPGet.Path != "/v1/health" || c.LivenessProbe.HTTPGet.Port.IntValue() != 8080 {
		t.Fatalf("livenessProbe = %+v, want HTTP GET /v1/health:8080", c.LivenessProbe)
	}
	for _, o := range objs {
		if o.Kind == "ConfigMap" && strings.Contains(string(o.JSON), "luncur-litestream") {
			t.Fatal("unexpected luncur-litestream ConfigMap without ReplicaURL")
		}
	}
}

func TestLuncurObjectsLitestreamSidecar(t *testing.T) {
	objs, err := LuncurObjects(Params{
		Image: "img", ExternalIP: "1.2.3.4", BuilderImage: "b",
		ReplicaURL: "s3://my-bucket/luncur", ReplicaEndpoint: "https://s3.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	var cm *corev1.ConfigMap
	for _, o := range objs {
		if o.Kind == "ConfigMap" {
			var c corev1.ConfigMap
			if err := json.Unmarshal(o.JSON, &c); err != nil {
				t.Fatal(err)
			}
			if c.Name == "luncur-litestream" {
				cm = &c
			}
		}
	}
	if cm == nil {
		t.Fatal("missing luncur-litestream ConfigMap")
	}
	cfg := cm.Data["litestream.yml"]
	if !strings.Contains(cfg, "s3://my-bucket/luncur") {
		t.Fatalf("litestream.yml missing replica URL: %s", cfg)
	}
	if !strings.Contains(cfg, "endpoint: https://s3.example.com") {
		t.Fatalf("litestream.yml missing endpoint: %s", cfg)
	}

	dep := deploymentFrom(t, objs)
	if len(dep.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("containers = %d, want 2 (luncur + litestream)", len(dep.Spec.Template.Spec.Containers))
	}
	var sidecar *corev1.Container
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == "litestream" {
			sidecar = &dep.Spec.Template.Spec.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("missing litestream sidecar container")
	}
	wantEnv := map[string]string{
		"LITESTREAM_ACCESS_KEY_ID":     "access-key",
		"LITESTREAM_SECRET_ACCESS_KEY": "secret-key",
	}
	for _, e := range sidecar.Env {
		wantKey, ok := wantEnv[e.Name]
		if !ok {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != LitestreamSecretName || e.ValueFrom.SecretKeyRef.Key != wantKey {
			t.Fatalf("env %s not sourced from %s/%s: %+v", e.Name, LitestreamSecretName, wantKey, e.ValueFrom)
		}
		delete(wantEnv, e.Name)
	}
	if len(wantEnv) != 0 {
		t.Fatalf("sidecar missing env vars: %v", wantEnv)
	}

	mounts := map[string]bool{}
	for _, m := range sidecar.VolumeMounts {
		mounts[m.Name] = true
	}
	if !mounts["luncur-data"] || !mounts["litestream-config"] {
		t.Fatalf("sidecar volume mounts = %+v, want luncur-data and litestream-config", sidecar.VolumeMounts)
	}

	foundVol := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "litestream-config" {
			foundVol = true
			if v.ConfigMap == nil || v.ConfigMap.Name != "luncur-litestream" {
				t.Fatalf("litestream-config volume = %+v, want ConfigMap luncur-litestream", v)
			}
		}
	}
	if !foundVol {
		t.Fatal("missing litestream-config pod volume")
	}
}

func TestForwardIngress(t *testing.T) {
	obj, err := ForwardIngress("waku-simpaniz--waku.1.2.3.4.sslip.io", "waku-simpaniz", "luncur-waku")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Kind != "Ingress" {
		t.Fatalf("kind %s", obj.Kind)
	}
	var ing netv1.Ingress
	if err := json.Unmarshal(obj.JSON, &ing); err != nil {
		t.Fatal(err)
	}
	if ing.Name != "fwd-waku-simpaniz-luncur-waku" {
		t.Fatalf("name %s", ing.Name)
	}
	if ing.Labels["luncur.dev/forward"] != "true" {
		t.Fatalf("labels %v", ing.Labels)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != "waku-simpaniz--waku.1.2.3.4.sslip.io" {
		t.Fatalf("rules %+v", ing.Spec.Rules)
	}
	b := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if b.Name != "luncur" || b.Port.Number != 80 {
		t.Fatalf("backend %+v", b)
	}
	if len(ing.Spec.TLS) != 0 {
		t.Fatalf("unexpected TLS %+v", ing.Spec.TLS)
	}
}
