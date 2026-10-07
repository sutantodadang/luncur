package render

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func hardenBase() Input {
	return Input{AppName: "web", Namespace: "ns", Image: "nginx:1", Host: "web.example", Port: 8080, Replicas: 2}
}

func deploymentOf(t *testing.T, r Rendered) appsv1.Deployment {
	t.Helper()
	for _, o := range r.Objects {
		if o.Kind == "Deployment" {
			var d appsv1.Deployment
			if err := json.Unmarshal(o.JSON, &d); err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatal("no Deployment rendered")
	return appsv1.Deployment{}
}

// Zero knobs render exactly the pre-hardening shape.
func TestHardeningZeroValueIsUnchanged(t *testing.T) {
	r, err := Render(hardenBase(), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := deploymentOf(t, r)
	spec := d.Spec.Template.Spec
	c := spec.Containers[0]
	if spec.SecurityContext != nil || c.SecurityContext != nil || spec.AutomountServiceAccountToken != nil ||
		spec.PriorityClassName != "" || len(spec.TopologySpreadConstraints) != 0 || c.Lifecycle != nil ||
		c.ReadinessProbe != nil || c.Resources.Requests != nil || d.Spec.RevisionHistoryLimit != nil || d.Spec.ProgressDeadlineSeconds != nil {
		t.Fatalf("zero-value hardening changed the Deployment: %s", r.Objects)
	}
}

func TestHardeningKnobs(t *testing.T) {
	in := hardenBase()
	in.DefaultCPUMilli, in.DefaultMemoryMB = 50, 64
	in.TCPProbe, in.PreStopSleep, in.ProgressDeadline, in.RevisionHistory = true, 5, 300, 5
	in.Spread, in.PriorityClass, in.Security = true, PriorityClassApp, "baseline"
	in.CPUMilli = 500 // user-set CPU: limit stays, memory gets the default request
	r, err := Render(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := deploymentOf(t, r)
	spec := d.Spec.Template.Spec
	c := spec.Containers[0]
	if got := c.Resources.Requests.Cpu().MilliValue(); got != 500 {
		t.Errorf("cpu request = %dm, want the app's 500m", got)
	}
	if got := c.Resources.Requests.Memory().Value(); got != 64<<20 {
		t.Errorf("memory request = %d, want 64Mi default", got)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		t.Error("default memory leaked into limits")
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.IntVal != 8080 {
		t.Errorf("readiness = %+v, want TCP 8080", c.ReadinessProbe)
	}
	if c.LivenessProbe != nil {
		t.Error("TCP probe mode must not add liveness")
	}
	if c.Lifecycle == nil || c.Lifecycle.PreStop.Sleep == nil || c.Lifecycle.PreStop.Sleep.Seconds != 5 {
		t.Errorf("preStop = %+v", c.Lifecycle)
	}
	if *d.Spec.RevisionHistoryLimit != 5 || *d.Spec.ProgressDeadlineSeconds != 300 {
		t.Error("revision history / progress deadline not set")
	}
	if len(spec.TopologySpreadConstraints) != 2 || spec.TopologySpreadConstraints[0].WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Errorf("spread = %+v", spec.TopologySpreadConstraints)
	}
	if spec.PriorityClassName != PriorityClassApp {
		t.Errorf("priority = %q", spec.PriorityClassName)
	}
	if spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities.Drop[0] != "NET_RAW" || *spec.AutomountServiceAccountToken || spec.SecurityContext.RunAsNonRoot != nil {
		t.Errorf("baseline security wrong: pod %+v container %+v", spec.SecurityContext, c.SecurityContext)
	}

	// A health path keeps the HTTP probes; one replica gets no spread.
	in.HealthPath, in.Replicas = "/healthz", 1
	d = deploymentOf(t, mustRender(t, in, nil))
	c = d.Spec.Template.Spec.Containers[0]
	if c.ReadinessProbe.HTTPGet == nil || c.LivenessProbe == nil {
		t.Error("health path must keep HTTP readiness+liveness")
	}
	if len(d.Spec.Template.Spec.TopologySpreadConstraints) != 0 {
		t.Error("single replica must not spread")
	}
}

func TestHardeningRestrictedAndBatch(t *testing.T) {
	in := hardenBase()
	in.Kind, in.Schedule, in.Host, in.Port = "cron", "* * * * *", "", 0
	in.Security, in.PriorityClass = "restricted", PriorityClassBatch
	r := mustRender(t, in, nil)
	var cj batchv1.CronJob
	for _, o := range r.Objects {
		if o.Kind == "CronJob" {
			json.Unmarshal(o.JSON, &cj)
		}
	}
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	c := spec.Containers[0]
	if spec.PriorityClassName != PriorityClassBatch || !*spec.SecurityContext.RunAsNonRoot ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" || c.SecurityContext.Capabilities.Add[0] != "NET_BIND_SERVICE" {
		t.Fatalf("restricted cron = pod %+v container %+v", spec.SecurityContext, c.SecurityContext)
	}

	in = hardenBase()
	in.Security = "relaxed"
	if d := deploymentOf(t, mustRender(t, in, nil)); d.Spec.Template.Spec.SecurityContext != nil {
		t.Error("relaxed must render no security context")
	}
}

func TestPriorityClasses(t *testing.T) {
	objs := PriorityClasses()
	if len(objs) != 3 {
		t.Fatalf("got %d classes", len(objs))
	}
	var batch struct {
		Metadata struct{ Name string }
		Value            int32
		PreemptionPolicy string
	}
	json.Unmarshal(objs[2].JSON, &batch)
	if batch.Metadata.Name != PriorityClassBatch || batch.Value != 100 || batch.PreemptionPolicy != "Never" {
		t.Fatalf("batch class = %+v", batch)
	}
}
