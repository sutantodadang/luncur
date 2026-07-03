package render

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
)

func testInput() Input {
	return Input{
		AppName:   "api",
		Namespace: "luncur-proj",
		Image:     "registry.luncur-system:5000/api:42",
		Host:      "api.203-0-113-7.sslip.io",
		Port:      3000,
		Replicas:  2,
	}
}

func mustRender(t *testing.T, in Input, env map[string]string) Rendered {
	t.Helper()
	r, err := Render(in, env)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func objByKind(t *testing.T, r Rendered, kind string) []byte {
	t.Helper()
	for _, o := range r.Objects {
		if o.Kind == kind {
			return o.JSON
		}
	}
	t.Fatalf("no %s in rendered objects", kind)
	return nil
}

func TestRenderDeployment(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"K": "v"})
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	if d.APIVersion != "apps/v1" || d.Kind != "Deployment" {
		t.Fatalf("TypeMeta missing: %s/%s", d.APIVersion, d.Kind)
	}
	if d.Name != "api" || d.Namespace != "luncur-proj" {
		t.Fatalf("meta: %s/%s", d.Namespace, d.Name)
	}
	if *d.Spec.Replicas != 2 {
		t.Fatalf("replicas: %d", *d.Spec.Replicas)
	}
	if d.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "api" {
		t.Fatalf("selector: %v", d.Spec.Selector.MatchLabels)
	}
	if d.Labels["app.kubernetes.io/managed-by"] != "luncur" {
		t.Fatalf("labels: %v", d.Labels)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Name != "app" || c.Image != "registry.luncur-system:5000/api:42" || c.Ports[0].ContainerPort != 3000 {
		t.Fatalf("container: %+v", c)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "api-env" {
		t.Fatalf("envFrom: %+v", c.EnvFrom)
	}
}

func TestRenderNoEnvMeansNoSecret(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	if len(r.Objects) != 3 {
		t.Fatalf("want 3 objects without env, got %d", len(r.Objects))
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if len(d.Spec.Template.Spec.Containers[0].EnvFrom) != 0 {
		t.Fatal("envFrom should be absent without env vars")
	}
}

func TestRenderServiceAndIngress(t *testing.T) {
	r := mustRender(t, testInput(), nil)

	var svc corev1.Service
	json.Unmarshal(objByKind(t, r, "Service"), &svc)
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 3000 {
		t.Fatalf("service ports: %+v", svc.Spec.Ports)
	}
	if svc.Spec.Selector["app.kubernetes.io/name"] != "api" {
		t.Fatalf("service selector: %v", svc.Spec.Selector)
	}

	var ing netv1.Ingress
	json.Unmarshal(objByKind(t, r, "Ingress"), &ing)
	rule := ing.Spec.Rules[0]
	if rule.Host != "api.203-0-113-7.sslip.io" {
		t.Fatalf("host: %s", rule.Host)
	}
	path := rule.HTTP.Paths[0]
	if path.Backend.Service.Name != "api" || path.Backend.Service.Port.Number != 80 {
		t.Fatalf("backend: %+v", path.Backend)
	}
}

func TestRenderSecret(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"A": "1", "B": "2"})
	if len(r.Objects) != 4 || r.Objects[0].Kind != "Secret" {
		t.Fatalf("want Secret first of 4, got %+v", r.Objects)
	}
	var sec corev1.Secret
	json.Unmarshal(r.Objects[0].JSON, &sec)
	if sec.Name != "api-env" || sec.StringData["A"] != "1" || sec.StringData["B"] != "2" {
		t.Fatalf("secret: %+v", sec)
	}
}

func TestYAMLMultiDoc(t *testing.T) {
	r := mustRender(t, testInput(), map[string]string{"A": "1"})
	y, err := YAML(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if strings.Count(s, "\n---\n") != 3 {
		t.Fatalf("want 3 separators for 4 docs, got:\n%s", s)
	}
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: Ingress", "kind: Secret"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in YAML", want)
		}
	}
}

func TestRenderValidatesInput(t *testing.T) {
	in := testInput()
	in.Image = ""
	if _, err := Render(in, nil); err == nil {
		t.Fatal("want error for empty image")
	}
}

func TestRenderCustomDomains(t *testing.T) {
	in := Input{
		AppName: "web", Namespace: "proj", Image: "img:1",
		Host: "web.1-2-3-4.sslip.io", Port: 8080, Replicas: 1,
		ExtraHosts:         []string{"www.example.com"},
		IngressAnnotations: map[string]string{"cert-manager.io/cluster-issuer": "luncur-le"},
		TLS: []netv1.IngressTLS{{
			Hosts: []string{"www.example.com"}, SecretName: "tls-web-abc12345",
		}},
	}
	r, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ing string
	for _, o := range r.Objects {
		if o.Kind == "Ingress" {
			ing = string(o.JSON)
		}
	}
	for _, want := range []string{
		`"www.example.com"`,
		`"web.1-2-3-4.sslip.io"`,
		`"cert-manager.io/cluster-issuer":"luncur-le"`,
		`"secretName":"tls-web-abc12345"`,
	} {
		if !strings.Contains(ing, want) {
			t.Fatalf("ingress missing %s:\n%s", want, ing)
		}
	}
}
