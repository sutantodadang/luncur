package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/secret"
)

// Installing n8n creates the app, its volume, a postgres addon attached
// to it, env resolved from the addon's credentials, and a deploy.
func TestTemplateInstallEndToEnd(t *testing.T) {
	oldWait, oldPoll := templateAddonWait, templateAddonPoll
	templateAddonWait, templateAddonPoll = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { templateAddonWait, templateAddonPoll = oldWait, oldPoll })

	st := newTestStore(t)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podMetricsGVR: "PodMetricsList"})
	dyn.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, nil })
	sealer, _ := secret.New(make([]byte, 32))
	srv := newHTTPTest(t, Deps{Store: st, Sealer: sealer, Kube: kube.NewFromDynamic(dyn), ExternalIP: "1.2.3.4"})
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"shop"}`).Body.Close()

	resp := doAuthed(t, "GET", srv.URL+"/v1/templates", admin, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"name":"n8n"`) || !strings.Contains(string(b), `"name":"vaultwarden"`) {
		t.Fatalf("templates = %s", b)
	}

	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/shop/templates/n8n/install", admin, `{"app_name":"flows"}`)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install = %d %s", resp.StatusCode, b)
	}
	var res installResult
	json.Unmarshal(b, &res)
	if res.App != "flows" || res.Seq != 1 || len(res.Steps) < 5 {
		t.Fatalf("result = %+v", res)
	}

	p, _ := st.GetProject("shop")
	a, err := st.GetApp(p.ID, "flows")
	if err != nil || a.HealthPath != "/healthz" || a.MemoryMB != 512 {
		t.Fatalf("app = %+v %v", a, err)
	}
	vols, _ := st.ListVolumes(a.ID)
	if len(vols) != 1 || vols[0].Path != "/home/node/.n8n" {
		t.Fatalf("volumes = %+v", vols)
	}
	addons, _ := st.AddonsForApp(a.ID)
	if len(addons) != 1 || addons[0].Name != "flows-db" || addons[0].Type != "postgres" {
		t.Fatalf("addons = %+v", addons)
	}
	envs, _ := st.ListEnv(a.ID)
	keys := map[string]bool{}
	for k := range envs {
		keys[k] = true
	}
	for _, k := range []string{"DB_POSTGRESDB_HOST", "DB_POSTGRESDB_PASSWORD", "N8N_ENCRYPTION_KEY", "WEBHOOK_URL"} {
		if !keys[k] {
			t.Fatalf("env missing %s: %v", k, keys)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, _ := st.LatestDeployment(a.ID)
		if d.Status == "live" && d.ImageRef == "n8nio/n8n:2.42.4" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("template deploy = %+v", d)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Installing again under the same name is refused with the steps so far.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/shop/templates/n8n/install", admin, `{"app_name":"flows"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate install = %d", resp.StatusCode)
	}
	if resp := doAuthed(t, "POST", srv.URL+"/v1/projects/shop/templates/nope/install", admin, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown template = %d", resp.StatusCode)
	}
}
