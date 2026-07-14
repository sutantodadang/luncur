package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestRenderDeployStamp(t *testing.T) {
	// Absent by default: deterministic render, no annotation.
	r := mustRender(t, testInput(), map[string]string{"K": "v"})
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Spec.Template.Annotations["luncur.dev/deploy"]; ok {
		t.Fatalf("deploy annotation present without DeployStamp: %v", d.Spec.Template.Annotations)
	}

	// Set: stamped on the pod template so a new deployment rolls the pods.
	in := testInput()
	in.DeployStamp = "dep123"
	r = mustRender(t, in, map[string]string{"K": "v"})
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	if got := d.Spec.Template.Annotations["luncur.dev/deploy"]; got != "dep123" {
		t.Fatalf("deploy annotation = %q, want dep123", got)
	}
}

func TestRenderNoEnvMeansNoSecret(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	// D4: testInput's Replicas:2 now also renders an automatic PDB, so
	// Deployment+Service+Ingress+PodDisruptionBudget = 4.
	if len(r.Objects) != 4 {
		t.Fatalf("want 4 objects without env, got %d", len(r.Objects))
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
	// D4: +1 for the automatic PDB (testInput's Replicas:2).
	if len(r.Objects) != 5 || r.Objects[0].Kind != "Secret" {
		t.Fatalf("want Secret first of 5, got %+v", r.Objects)
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
	// D4: +1 doc for the automatic PDB (testInput's Replicas:2) -> 5 docs, 4
	// separators.
	if strings.Count(s, "\n---\n") != 4 {
		t.Fatalf("want 4 separators for 5 docs, got:\n%s", s)
	}
	for _, want := range []string{"kind: Deployment", "kind: Service", "kind: Ingress", "kind: Secret", "kind: PodDisruptionBudget"} {
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

func TestRenderResourcesCPUAndMemory(t *testing.T) {
	in := testInput()
	in.CPUMilli = 250
	in.MemoryMB = 256
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	wantCPU := *resource.NewMilliQuantity(250, resource.DecimalSI)
	wantMem := *resource.NewQuantity(256*1024*1024, resource.BinarySI)
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
		t.Fatalf("cpu request: got %s want %s", got.String(), wantCPU.String())
	}
	if got := c.Resources.Limits[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
		t.Fatalf("cpu limit: got %s want %s", got.String(), wantCPU.String())
	}
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.Cmp(wantMem) != 0 {
		t.Fatalf("memory request: got %s want %s", got.String(), wantMem.String())
	}
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(wantMem) != 0 {
		t.Fatalf("memory limit: got %s want %s", got.String(), wantMem.String())
	}
}

func TestRenderResourcesCPUOnly(t *testing.T) {
	in := testInput()
	in.CPUMilli = 500
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	c := d.Spec.Template.Spec.Containers[0]
	if _, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok {
		t.Fatal("want cpu request present")
	}
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		t.Fatal("want memory request absent")
	}
}

func TestRenderResourcesMemoryOnly(t *testing.T) {
	in := testInput()
	in.MemoryMB = 512
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	c := d.Spec.Template.Spec.Containers[0]
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok {
		t.Fatal("want memory request present")
	}
	if _, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		t.Fatal("want cpu request absent")
	}
}

func TestRenderResourcesNeitherMeansNoResourcesKey(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	c := d.Spec.Template.Spec.Containers[0]
	if len(c.Resources.Requests) != 0 || len(c.Resources.Limits) != 0 {
		t.Fatalf("want empty resources when cpu/memory unset: %+v", c.Resources)
	}
}

func TestRenderHealthPath(t *testing.T) {
	in := testInput()
	in.HealthPath = "/healthz"
	r := mustRender(t, in, nil)
	var d appsv1.Deployment
	if err := json.Unmarshal(objByKind(t, r, "Deployment"), &d); err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe == nil || c.LivenessProbe == nil {
		t.Fatalf("want both probes set: %+v", c)
	}
	rp := c.ReadinessProbe
	if rp.HTTPGet == nil || rp.HTTPGet.Path != "/healthz" || rp.HTTPGet.Port.IntValue() != 3000 {
		t.Fatalf("readiness httpGet: %+v", rp.HTTPGet)
	}
	if rp.PeriodSeconds != 5 || rp.FailureThreshold != 3 {
		t.Fatalf("readiness timing: period=%d failureThreshold=%d", rp.PeriodSeconds, rp.FailureThreshold)
	}
	lp := c.LivenessProbe
	if lp.HTTPGet == nil || lp.HTTPGet.Path != "/healthz" || lp.HTTPGet.Port.IntValue() != 3000 {
		t.Fatalf("liveness httpGet: %+v", lp.HTTPGet)
	}
	if lp.InitialDelaySeconds != 15 || lp.PeriodSeconds != 20 || lp.FailureThreshold != 3 {
		t.Fatalf("liveness timing: initialDelay=%d period=%d failureThreshold=%d", lp.InitialDelaySeconds, lp.PeriodSeconds, lp.FailureThreshold)
	}
}

func TestRenderHealthPathUnsetMeansNoProbes(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	c := d.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Fatalf("want no probes when health path unset: %+v", c)
	}
	raw := objByKind(t, r, "Deployment")
	if strings.Contains(string(raw), "readinessProbe") || strings.Contains(string(raw), "livenessProbe") {
		t.Fatalf("raw JSON should not contain probe keys:\n%s", raw)
	}
}

func workerInput() Input {
	return Input{
		AppName:   "worker",
		Namespace: "luncur-proj",
		Image:     "registry.luncur-system:5000/worker:1",
		Kind:      "worker",
		Replicas:  2,
	}
}

func cronInput() Input {
	return Input{
		AppName:   "nightly",
		Namespace: "luncur-proj",
		Image:     "registry.luncur-system:5000/nightly:1",
		Kind:      "cron",
		Schedule:  "0 3 * * *",
	}
}

func TestRenderWorkerHasOnlyDeploymentNoPorts(t *testing.T) {
	r := mustRender(t, workerInput(), nil)
	// D4: workerInput's Replicas:2 now also renders an automatic PDB.
	if len(r.Objects) != 2 || r.Objects[0].Kind != "Deployment" {
		t.Fatalf("want Deployment+PodDisruptionBudget for worker, got %+v", r.Objects)
	}
	var d appsv1.Deployment
	if err := json.Unmarshal(r.Objects[0].JSON, &d); err != nil {
		t.Fatal(err)
	}
	c := d.Spec.Template.Spec.Containers[0]
	if len(c.Ports) != 0 {
		t.Fatalf("want no container ports for worker, got %+v", c.Ports)
	}
	if *d.Spec.Replicas != 2 {
		t.Fatalf("replicas: %d", *d.Spec.Replicas)
	}
}

func TestRenderWorkerWithEnvHasSecretAndEnvFrom(t *testing.T) {
	r := mustRender(t, workerInput(), map[string]string{"K": "v"})
	// D4: +1 for the automatic PDB (workerInput's Replicas:2).
	if len(r.Objects) != 3 {
		t.Fatalf("want Secret+Deployment+PodDisruptionBudget for worker with env, got %+v", r.Objects)
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	c := d.Spec.Template.Spec.Containers[0]
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "worker-env" {
		t.Fatalf("envFrom: %+v", c.EnvFrom)
	}
}

func TestRenderCronJob(t *testing.T) {
	r := mustRender(t, cronInput(), nil)
	if len(r.Objects) != 1 || r.Objects[0].Kind != "CronJob" {
		t.Fatalf("want exactly one CronJob, got %+v", r.Objects)
	}
	var cj batchv1.CronJob
	if err := json.Unmarshal(r.Objects[0].JSON, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.APIVersion != "batch/v1" || cj.Kind != "CronJob" {
		t.Fatalf("TypeMeta: %s/%s", cj.APIVersion, cj.Kind)
	}
	if cj.Spec.Schedule != "0 3 * * *" {
		t.Fatalf("schedule: %q", cj.Spec.Schedule)
	}
	if cj.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Fatalf("concurrencyPolicy: %q", cj.Spec.ConcurrencyPolicy)
	}
	if cj.Spec.SuccessfulJobsHistoryLimit == nil || *cj.Spec.SuccessfulJobsHistoryLimit != 3 {
		t.Fatalf("successfulJobsHistoryLimit: %v", cj.Spec.SuccessfulJobsHistoryLimit)
	}
	if cj.Spec.FailedJobsHistoryLimit == nil || *cj.Spec.FailedJobsHistoryLimit != 3 {
		t.Fatalf("failedJobsHistoryLimit: %v", cj.Spec.FailedJobsHistoryLimit)
	}
	jobSpec := cj.Spec.JobTemplate.Spec
	if jobSpec.BackoffLimit == nil || *jobSpec.BackoffLimit != 2 {
		t.Fatalf("backoffLimit: %v", jobSpec.BackoffLimit)
	}
	pod := jobSpec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("restartPolicy: %q", pod.RestartPolicy)
	}
	c := pod.Containers[0]
	if len(c.Ports) != 0 {
		t.Fatalf("want no container ports for cron, got %+v", c.Ports)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend != false {
		t.Fatalf("suspend: want false (default), got %v", cj.Spec.Suspend)
	}
	if got := cj.Spec.JobTemplate.Labels["app.kubernetes.io/name"]; got != "nightly" {
		t.Fatalf("JobTemplate labels missing app label: %+v", cj.Spec.JobTemplate.Labels)
	}
}

// TestRenderCronJobSuspended covers the pause/resume feature: Suspended maps
// straight to CronJob.Spec.Suspend so a paused cron's schedule stops firing
// without losing its stored schedule/history.
func TestRenderCronJobSuspended(t *testing.T) {
	in := cronInput()
	in.Suspended = true
	r := mustRender(t, in, nil)
	var cj batchv1.CronJob
	if err := json.Unmarshal(r.Objects[0].JSON, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend != true {
		t.Fatalf("suspend: want true, got %v", cj.Spec.Suspend)
	}
}

func TestRenderCronJobWithEnvAndResources(t *testing.T) {
	in := cronInput()
	in.CPUMilli, in.MemoryMB = 250, 256
	r, err := Render(in, map[string]string{"K": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Objects) != 2 || r.Objects[0].Kind != "Secret" || r.Objects[1].Kind != "CronJob" {
		t.Fatalf("want Secret+CronJob, got %+v", r.Objects)
	}
	var cj batchv1.CronJob
	json.Unmarshal(r.Objects[1].JSON, &cj)
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef.Name != "nightly-env" {
		t.Fatalf("envFrom: %+v", c.EnvFrom)
	}
	if _, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok {
		t.Fatal("want cpu request present in cron pod template")
	}
	if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok {
		t.Fatal("want memory request present in cron pod template")
	}
}

func TestRenderHealthPathIgnoredForNonWebKinds(t *testing.T) {
	w := workerInput()
	w.HealthPath = "/healthz"
	r := mustRender(t, w, nil)
	var d appsv1.Deployment
	json.Unmarshal(r.Objects[0].JSON, &d)
	c := d.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Fatalf("want no probes for worker even with HealthPath set: %+v", c)
	}

	cr := cronInput()
	cr.HealthPath = "/healthz"
	r2 := mustRender(t, cr, nil)
	var cj batchv1.CronJob
	json.Unmarshal(r2.Objects[0].JSON, &cj)
	cc := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if cc.ReadinessProbe != nil || cc.LivenessProbe != nil {
		t.Fatalf("want no probes for cron even with HealthPath set: %+v", cc)
	}
}

func TestRenderWorkerAndCronRequireOnlyCoreFields(t *testing.T) {
	w := workerInput()
	w.AppName = ""
	if _, err := Render(w, nil); err == nil {
		t.Fatal("want error for empty AppName")
	}

	// Host/Port are NOT required for worker/cron.
	c := cronInput()
	if c.Host != "" || c.Port != 0 {
		t.Fatalf("test input should have no host/port set: %+v", c)
	}
	if _, err := Render(c, nil); err != nil {
		t.Fatalf("cron render should succeed without Host/Port: %v", err)
	}
}

func TestOverrideCronJobRoundTrip(t *testing.T) {
	base := mustRender(t, cronInput(), nil)
	baseYAML, err := YAML(base)
	if err != nil {
		t.Fatal(err)
	}

	edited := cronInput()
	edited.Schedule = "0 4 * * *"
	editedRendered := mustRender(t, edited, nil)
	editedYAML, err := YAML(editedRendered)
	if err != nil {
		t.Fatal(err)
	}

	patch, err := ComputeOverride("CronJob", baseYAML, editedYAML)
	if err != nil {
		t.Fatal(err)
	}

	in := cronInput()
	in.Overrides = map[string]string{"CronJob": patch}
	out, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cj batchv1.CronJob
	json.Unmarshal(objByKind(t, out, "CronJob"), &cj)
	if cj.Spec.Schedule != "0 4 * * *" {
		t.Fatalf("want overridden schedule, got %q", cj.Spec.Schedule)
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

func TestRenderVolumes(t *testing.T) {
	in := testInput()
	in.Volumes = []Volume{
		{Name: "data", Path: "/data", SizeGB: 10},
		{Name: "cache", Path: "/var/cache", SizeGB: 1},
	}
	r := mustRender(t, in, nil)

	// PVCs come before the Deployment.
	kinds := make([]string, 0, len(r.Objects))
	for _, o := range r.Objects {
		kinds = append(kinds, o.Kind)
	}
	pvcCount, depIdx, lastPVCIdx := 0, -1, -1
	for i, k := range kinds {
		if k == "PersistentVolumeClaim" {
			pvcCount++
			lastPVCIdx = i
		}
		if k == "Deployment" {
			depIdx = i
		}
	}
	if pvcCount != 2 {
		t.Fatalf("want 2 PVCs, got %d (%v)", pvcCount, kinds)
	}
	if depIdx == -1 || lastPVCIdx > depIdx {
		t.Fatalf("PVCs must precede Deployment: %v", kinds)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := json.Unmarshal(r.Objects[0].JSON, &pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Name != "api-data" {
		t.Fatalf("pvc name = %q, want api-data", pvc.Name)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Fatalf("access modes: %v", pvc.Spec.AccessModes)
	}
	want := resource.MustParse("10Gi")
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(want) != 0 {
		t.Fatalf("storage request = %s, want 10Gi", got.String())
	}

	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want Recreate", d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	if len(pod.Volumes) != 2 {
		t.Fatalf("pod volumes: %+v", pod.Volumes)
	}
	if pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "api-data" {
		t.Fatalf("pod volume claim: %+v", pod.Volumes[0])
	}
	mounts := pod.Containers[0].VolumeMounts
	if len(mounts) != 2 || mounts[0].Name != "data" || mounts[0].MountPath != "/data" ||
		mounts[1].Name != "cache" || mounts[1].MountPath != "/var/cache" {
		t.Fatalf("mounts: %+v", mounts)
	}
}

func TestRenderNoVolumesMeansRollingStrategyNoPVC(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	for _, o := range r.Objects {
		if o.Kind == "PersistentVolumeClaim" {
			t.Fatal("PVC rendered without volumes")
		}
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("strategy should be RollingUpdate without volumes, got %q", d.Spec.Strategy.Type)
	}
	ru := d.Spec.Strategy.RollingUpdate
	if ru == nil || ru.MaxUnavailable.IntValue() != 0 || ru.MaxSurge.IntValue() != 1 {
		t.Fatalf("rollingUpdate: %+v", ru)
	}
}

func TestRenderWorkerWithVolume(t *testing.T) {
	in := workerInput()
	in.Volumes = []Volume{{Name: "state", Path: "/state", SizeGB: 2}}
	r := mustRender(t, in, nil)
	// D4: +1 for the automatic PDB (workerInput's Replicas:2).
	if len(r.Objects) != 3 || r.Objects[0].Kind != "PersistentVolumeClaim" {
		t.Fatalf("want PVC+Deployment+PodDisruptionBudget for worker with volume, got %+v", r.Objects)
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want Recreate", d.Spec.Strategy.Type)
	}
	if len(d.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("mounts: %+v", d.Spec.Template.Spec.Containers[0].VolumeMounts)
	}
}

func TestRenderCronIgnoresVolumes(t *testing.T) {
	in := cronInput()
	in.Volumes = []Volume{{Name: "data", Path: "/data", SizeGB: 1}}
	r := mustRender(t, in, nil)
	if len(r.Objects) != 1 || r.Objects[0].Kind != "CronJob" {
		t.Fatalf("cron must ignore volumes, got %+v", r.Objects)
	}
	var cj batchv1.CronJob
	json.Unmarshal(r.Objects[0].JSON, &cj)
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.Volumes) != 0 || len(pod.Containers[0].VolumeMounts) != 0 {
		t.Fatalf("cron pod picked up volumes: %+v %+v", pod.Volumes, pod.Containers[0].VolumeMounts)
	}
}

// TestRenderInternalWebHasServiceNoIngress covers an internal web app: it
// still gets a ClusterIP Service (so other in-cluster apps can reach it) but
// no Ingress at all — no public URL, no domains possible.
func TestRenderInternalWebHasServiceNoIngress(t *testing.T) {
	in := testInput()
	in.Internal = true
	r := mustRender(t, in, nil)
	// D4: +1 for the automatic PDB (testInput's Replicas:2) — Internal only
	// suppresses the Ingress, not the PDB.
	if len(r.Objects) != 3 {
		t.Fatalf("want Deployment+Service+PodDisruptionBudget for internal web, got %+v", r.Objects)
	}
	if _, err := findKind(r, "Ingress"); err == nil {
		t.Fatalf("internal web app must not render an Ingress, got %+v", r.Objects)
	}
	var svc corev1.Service
	json.Unmarshal(objByKind(t, r, "Service"), &svc)
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntValue() != 3000 {
		t.Fatalf("service ports: %+v", svc.Spec.Ports)
	}
}

// findKind reports whether kind is present in r.Objects, without failing the
// test (unlike objByKind) — used to assert absence.
func findKind(r Rendered, kind string) (int, error) {
	for i, o := range r.Objects {
		if o.Kind == kind {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no %s in rendered objects", kind)
}

// TestRenderNonInternalWebUnchanged pins the default (Internal: false, the
// zero value) web object set: Deployment+Service+Ingress, exactly as before
// this field existed — byte-identical behavior for internal=0.
func TestRenderNonInternalWebUnchanged(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	// D4: +1 for the automatic PDB (testInput's Replicas:2).
	if len(r.Objects) != 4 {
		t.Fatalf("want Deployment+Service+Ingress+PodDisruptionBudget for non-internal web, got %+v", r.Objects)
	}
	if _, err := findKind(r, "Ingress"); err != nil {
		t.Fatalf("non-internal web app must render an Ingress: %v", err)
	}
}

// TestRenderInternalIgnoredForWorkerAndCron checks the Internal flag has no
// effect outside the web case: worker/cron already render no Service, so
// setting Internal must not change their object set.
func TestRenderInternalIgnoredForWorkerAndCron(t *testing.T) {
	w := workerInput()
	w.Internal = true
	rw := mustRender(t, w, nil)
	// D4: +1 for the automatic PDB (workerInput's Replicas:2); Internal has
	// no bearing on it either way.
	if len(rw.Objects) != 2 || rw.Objects[0].Kind != "Deployment" {
		t.Fatalf("internal=true must not change worker's object set, got %+v", rw.Objects)
	}

	c := cronInput()
	c.Internal = true
	rc := mustRender(t, c, nil)
	if len(rc.Objects) != 1 || rc.Objects[0].Kind != "CronJob" {
		t.Fatalf("internal=true must not change cron's object set, got %+v", rc.Objects)
	}
}

// TestZeroDowntimeDeploy covers D1: rolling-update strategy (0 unavailable, 1
// surge) on Deployments that don't require Recreate, a startupProbe when a
// health path is set, and an explicit 30s termination grace period.
func TestZeroDowntimeDeploy(t *testing.T) {
	t.Run("web with health path", func(t *testing.T) {
		in := testInput()
		in.HealthPath = "/healthz"
		r := mustRender(t, in, nil)
		var d appsv1.Deployment
		json.Unmarshal(objByKind(t, r, "Deployment"), &d)

		if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
			t.Fatalf("strategy = %q, want RollingUpdate", d.Spec.Strategy.Type)
		}
		ru := d.Spec.Strategy.RollingUpdate
		if ru == nil || ru.MaxUnavailable.IntValue() != 0 || ru.MaxSurge.IntValue() != 1 {
			t.Fatalf("rollingUpdate: %+v", ru)
		}

		c := d.Spec.Template.Spec.Containers[0]
		sp := c.StartupProbe
		if sp == nil || sp.HTTPGet == nil || sp.HTTPGet.Path != "/healthz" {
			t.Fatalf("startupProbe: %+v", sp)
		}
		if sp.PeriodSeconds != 2 || sp.FailureThreshold != 30 {
			t.Fatalf("startupProbe timing: period=%d failureThreshold=%d", sp.PeriodSeconds, sp.FailureThreshold)
		}

		pod := d.Spec.Template.Spec
		if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != 30 {
			t.Fatalf("terminationGracePeriodSeconds: %v", pod.TerminationGracePeriodSeconds)
		}
	})

	t.Run("web with volume forces Recreate", func(t *testing.T) {
		in := testInput()
		in.Volumes = []Volume{{Name: "data", Path: "/data", SizeGB: 10}}
		r := mustRender(t, in, nil)
		var d appsv1.Deployment
		json.Unmarshal(objByKind(t, r, "Deployment"), &d)

		if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Fatalf("strategy = %q, want Recreate", d.Spec.Strategy.Type)
		}
		if d.Spec.Strategy.RollingUpdate != nil {
			t.Fatalf("rollingUpdate should be nil for Recreate, got %+v", d.Spec.Strategy.RollingUpdate)
		}
	})

	t.Run("worker without health path", func(t *testing.T) {
		r := mustRender(t, workerInput(), nil)
		var d appsv1.Deployment
		json.Unmarshal(objByKind(t, r, "Deployment"), &d)

		if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
			t.Fatalf("strategy = %q, want RollingUpdate", d.Spec.Strategy.Type)
		}
		ru := d.Spec.Strategy.RollingUpdate
		if ru == nil || ru.MaxUnavailable.IntValue() != 0 || ru.MaxSurge.IntValue() != 1 {
			t.Fatalf("rollingUpdate: %+v", ru)
		}
		if d.Spec.Template.Spec.Containers[0].StartupProbe != nil {
			t.Fatalf("startupProbe should be nil without health path")
		}
	})
}

// TestRenderAutoscaleWeb covers D2: a web app with AutoMin>0 gets an
// autoscaling/v2 HPA targeting its Deployment, and the Deployment's
// spec.replicas key is omitted entirely (released to the HPA controller).
func TestRenderAutoscaleWeb(t *testing.T) {
	in := testInput()
	in.AutoMin, in.AutoMax, in.AutoCPU = 1, 5, 70
	r := mustRender(t, in, nil)

	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := json.Unmarshal(objByKind(t, r, "HorizontalPodAutoscaler"), &hpa); err != nil {
		t.Fatal(err)
	}
	if hpa.Spec.ScaleTargetRef.Kind != "Deployment" || hpa.Spec.ScaleTargetRef.Name != "api" {
		t.Fatalf("scaleTargetRef: %+v", hpa.Spec.ScaleTargetRef)
	}
	if hpa.Spec.MinReplicas == nil || *hpa.Spec.MinReplicas != 1 || hpa.Spec.MaxReplicas != 5 {
		t.Fatalf("min/max: %+v %d", hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas)
	}
	if len(hpa.Spec.Metrics) != 1 || hpa.Spec.Metrics[0].Resource == nil ||
		hpa.Spec.Metrics[0].Resource.Name != corev1.ResourceCPU ||
		hpa.Spec.Metrics[0].Resource.Target.AverageUtilization == nil ||
		*hpa.Spec.Metrics[0].Resource.Target.AverageUtilization != 70 {
		t.Fatalf("metrics: %+v", hpa.Spec.Metrics)
	}

	depJSON := objByKind(t, r, "Deployment")
	if strings.Contains(string(depJSON), `"replicas"`) {
		t.Fatalf("deployment must omit replicas when autoscale is on: %s", depJSON)
	}
}

// TestRenderAutoscaleOffMeansNoHPA pins the default (AutoMin 0, the zero
// value): no HPA rendered, and the Deployment keeps its explicit replicas.
func TestRenderAutoscaleOffMeansNoHPA(t *testing.T) {
	r := mustRender(t, testInput(), nil)
	if _, err := findKind(r, "HorizontalPodAutoscaler"); err == nil {
		t.Fatalf("autoscale off must not render an HPA, got %+v", r.Objects)
	}
	var d appsv1.Deployment
	json.Unmarshal(objByKind(t, r, "Deployment"), &d)
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Fatalf("replicas: %v", d.Spec.Replicas)
	}
}

// TestRenderAutoscaleWorker mirrors the web case for worker apps.
func TestRenderAutoscaleWorker(t *testing.T) {
	in := workerInput()
	in.AutoMin, in.AutoMax, in.AutoCPU = 2, 8, 60
	r := mustRender(t, in, nil)

	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := json.Unmarshal(objByKind(t, r, "HorizontalPodAutoscaler"), &hpa); err != nil {
		t.Fatal(err)
	}
	if hpa.Spec.ScaleTargetRef.Name != "worker" || *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 8 {
		t.Fatalf("hpa: %+v", hpa.Spec)
	}

	depJSON := objByKind(t, r, "Deployment")
	if strings.Contains(string(depJSON), `"replicas"`) {
		t.Fatalf("deployment must omit replicas when autoscale is on: %s", depJSON)
	}
}

// TestRenderAutoscaleIgnoredForModelAndCron checks AutoMin has no effect
// outside web/worker: model and cron apps never render an HPA even with
// AutoMin set.
func TestRenderAutoscaleIgnoredForModelAndCron(t *testing.T) {
	m := Input{
		AppName: "llm", Namespace: "ns", Image: "ignored:0", Host: "llm.example.com",
		Kind: "model", Replicas: 1, ModelSource: "hf:org/name/model.gguf",
		AutoMin: 1, AutoMax: 5, AutoCPU: 70,
	}
	rm := mustRender(t, m, nil)
	if _, err := findKind(rm, "HorizontalPodAutoscaler"); err == nil {
		t.Fatalf("model apps must never render an HPA, got %+v", rm.Objects)
	}

	c := cronInput()
	c.AutoMin, c.AutoMax, c.AutoCPU = 1, 5, 70
	rc := mustRender(t, c, nil)
	if _, err := findKind(rc, "HorizontalPodAutoscaler"); err == nil {
		t.Fatalf("cron apps must never render an HPA, got %+v", rc.Objects)
	}
}

// TestRenderPDBPresentAtReplicas2 covers D4: a web app with Replicas>=2 gets
// an automatic PodDisruptionBudget capping voluntary disruption at 1.
func TestRenderPDBPresentAtReplicas2(t *testing.T) {
	r := mustRender(t, testInput(), nil) // testInput: Replicas 2
	var pdb policyv1.PodDisruptionBudget
	if err := json.Unmarshal(objByKind(t, r, "PodDisruptionBudget"), &pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("maxUnavailable: %+v", pdb.Spec.MaxUnavailable)
	}
	if pdb.Spec.Selector == nil || pdb.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != "api" {
		t.Fatalf("selector: %+v", pdb.Spec.Selector)
	}
}

// TestRenderPDBAbsentAtReplicas1 covers D4: a single-replica web app gets no
// PDB — maxUnavailable:1 on it would block every node drain forever.
func TestRenderPDBAbsentAtReplicas1(t *testing.T) {
	in := testInput()
	in.Replicas = 1
	r := mustRender(t, in, nil)
	if _, err := findKind(r, "PodDisruptionBudget"); err == nil {
		t.Fatalf("replicas=1 must not render a PDB, got %+v", r.Objects)
	}
}

// TestRenderPDBPresentAtAutoMin2 covers D4: an autoscaling web app judges the
// PDB threshold against AutoMin, not the (irrelevant, HPA-owned) Replicas
// field — AutoMin=2 renders a PDB even though Replicas is left at 1.
func TestRenderPDBPresentAtAutoMin2(t *testing.T) {
	in := testInput()
	in.Replicas = 1
	in.CPUMilli = 250
	in.AutoMin, in.AutoMax, in.AutoCPU = 2, 5, 70
	r := mustRender(t, in, nil)
	if _, err := findKind(r, "PodDisruptionBudget"); err != nil {
		t.Fatalf("AutoMin=2 must render a PDB even with Replicas=1: %v", err)
	}
}

// TestRenderPDBIgnoredForModelAndCron checks the automatic PDB has no effect
// outside web/worker: model and cron apps never render a PDB even at
// Replicas>=2.
func TestRenderPDBIgnoredForModelAndCron(t *testing.T) {
	m := Input{
		AppName: "llm", Namespace: "ns", Image: "ignored:0", Host: "llm.example.com",
		Kind: "model", Replicas: 2, ModelSource: "hf:org/name/model.gguf",
	}
	rm := mustRender(t, m, nil)
	if _, err := findKind(rm, "PodDisruptionBudget"); err == nil {
		t.Fatalf("model apps must never render a PDB, got %+v", rm.Objects)
	}

	// cron has no meaningful Replicas/AutoMin at all; confirm it stays PDB-free.
	c := cronInput()
	rc := mustRender(t, c, nil)
	if _, err := findKind(rc, "PodDisruptionBudget"); err == nil {
		t.Fatalf("cron apps must never render a PDB, got %+v", rc.Objects)
	}
}

// TestProjectQuotaObjectsCPUOnly covers D4's ProjectQuotaObjects with only a
// CPU quota set: the ResourceQuota's Hard map gets limits.cpu only, and the
// LimitRange still rides along with its container defaults.
func TestProjectQuotaObjectsCPUOnly(t *testing.T) {
	objs, err := ProjectQuotaObjects("luncur-proj", 4000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var rq corev1.ResourceQuota
	var lr corev1.LimitRange
	for _, o := range objs {
		switch o.Kind {
		case "ResourceQuota":
			if err := json.Unmarshal(o.JSON, &rq); err != nil {
				t.Fatal(err)
			}
		case "LimitRange":
			if err := json.Unmarshal(o.JSON, &lr); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rq.Name != ProjectQuotaName || rq.Namespace != "luncur-proj" {
		t.Fatalf("resourcequota meta: %+v", rq.ObjectMeta)
	}
	wantCPU := *resource.NewMilliQuantity(4000, resource.DecimalSI)
	if got, ok := rq.Spec.Hard[corev1.ResourceName("limits.cpu")]; !ok || got.Cmp(wantCPU) != 0 {
		t.Fatalf("limits.cpu: %+v", rq.Spec.Hard)
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceName("limits.memory")]; ok {
		t.Fatalf("limits.memory should be absent when memMB=0: %+v", rq.Spec.Hard)
	}
	if lr.Name != LimitRangeName || len(lr.Spec.Limits) != 1 {
		t.Fatalf("limitrange: %+v", lr)
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Fatalf("limitrange item type: %q", item.Type)
	}
	if _, ok := item.Default[corev1.ResourceCPU]; !ok {
		t.Fatalf("limitrange default cpu missing: %+v", item.Default)
	}
	if _, ok := item.DefaultRequest[corev1.ResourceMemory]; !ok {
		t.Fatalf("limitrange defaultRequest memory missing: %+v", item.DefaultRequest)
	}
}

// TestProjectQuotaObjectsMemOnly mirrors the CPU-only case for memory alone.
func TestProjectQuotaObjectsMemOnly(t *testing.T) {
	objs, err := ProjectQuotaObjects("luncur-proj", 0, 8192)
	if err != nil {
		t.Fatal(err)
	}
	var rq corev1.ResourceQuota
	for _, o := range objs {
		if o.Kind == "ResourceQuota" {
			if err := json.Unmarshal(o.JSON, &rq); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceName("limits.cpu")]; ok {
		t.Fatalf("limits.cpu should be absent when cpuMilli=0: %+v", rq.Spec.Hard)
	}
	wantMem := *resource.NewQuantity(8192*1024*1024, resource.BinarySI)
	if got, ok := rq.Spec.Hard[corev1.ResourceName("limits.memory")]; !ok || got.Cmp(wantMem) != 0 {
		t.Fatalf("limits.memory: %+v", rq.Spec.Hard)
	}
}

// TestProjectQuotaObjectsBoth covers both quotas set together.
func TestProjectQuotaObjectsBoth(t *testing.T) {
	objs, err := ProjectQuotaObjects("luncur-proj", 2000, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var rq corev1.ResourceQuota
	for _, o := range objs {
		if o.Kind == "ResourceQuota" {
			if err := json.Unmarshal(o.JSON, &rq); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceName("limits.cpu")]; !ok {
		t.Fatalf("limits.cpu missing: %+v", rq.Spec.Hard)
	}
	if _, ok := rq.Spec.Hard[corev1.ResourceName("limits.memory")]; !ok {
		t.Fatalf("limits.memory missing: %+v", rq.Spec.Hard)
	}
}
