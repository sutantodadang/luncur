package server

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

func renderedDeployment(t *testing.T, r render.Rendered) appsv1.Deployment {
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
	t.Fatal("no Deployment")
	return appsv1.Deployment{}
}

// Settings and the app's policy reach render: default requests, TCP probe,
// security level, progress deadline — and each escape hatch turns its
// knob off.
func TestHardenInputFromSettingsAndPolicy(t *testing.T) {
	s := aiTestServer(t)
	p, _ := s.st.CreateProject("shop")
	p, env := seedDefaultEnv(t, s.st, p)
	a, err := s.st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}

	d := renderedDeployment(t, mustRenderApp(t, s, p, env, a))
	c := d.Spec.Template.Spec.Containers[0]
	if c.Resources.Requests.Cpu().MilliValue() != 50 || c.Resources.Requests.Memory().Value() != 64<<20 {
		t.Errorf("default requests = %v", c.Resources.Requests)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil {
		t.Error("default TCP probe missing")
	}
	if d.Spec.Template.Spec.SecurityContext == nil || *d.Spec.ProgressDeadlineSeconds != 300 || *d.Spec.RevisionHistoryLimit != 5 {
		t.Error("baseline security / deadline / history missing")
	}
	if d.Spec.Template.Spec.PriorityClassName != "" {
		t.Error("priority class referenced before the classes exist")
	}

	s.st.SetSetting("default_cpu_request", "0")
	s.st.SetSetting("default_memory_request", "128Mi")
	pol := store.DefaultAppPolicy(a.ID)
	pol.Probe, pol.Security, pol.RolloutTimeout = "off", "relaxed", 600
	if err := s.st.SetAppPolicy(pol); err != nil {
		t.Fatal(err)
	}
	s.hardening.priorityReady.Store(true)
	d = renderedDeployment(t, mustRenderApp(t, s, p, env, a))
	c = d.Spec.Template.Spec.Containers[0]
	if _, ok := c.Resources.Requests["cpu"]; ok || c.Resources.Requests.Memory().Value() != 128<<20 {
		t.Errorf("tuned requests = %v", c.Resources.Requests)
	}
	if c.ReadinessProbe != nil || d.Spec.Template.Spec.SecurityContext != nil || *d.Spec.ProgressDeadlineSeconds != 600 {
		t.Error("escape hatches not honored")
	}
	if d.Spec.Template.Spec.PriorityClassName != render.PriorityClassApp {
		t.Errorf("priority = %q", d.Spec.Template.Spec.PriorityClassName)
	}
}

func mustRenderApp(t *testing.T, s *server, p store.Project, env store.Environment, a store.App) render.Rendered {
	t.Helper()
	r, err := s.renderApp(p, env, a, "nginx:1", true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
