package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

func TestAppPolicyAPI(t *testing.T) {
	t.Parallel()
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/web/apps", admin, `{"name":"bg","kind":"worker"}`).Body.Close()

	get := func(app string) store.AppPolicy {
		t.Helper()
		resp := doAuthed(t, "GET", srv.URL+"/v1/projects/web/apps/"+app+"/policy", admin, "")
		defer resp.Body.Close()
		var p store.AppPolicy
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get("api"); !p.AutoRollback || p.RolloutTimeout != 300 || p.Strategy != "rolling" || p.Security != "baseline" {
		t.Fatalf("defaults = %+v", p)
	}

	resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/api/policy", admin,
		`{"auto_rollback":false,"strategy":"canary","canary_steps":"5,25,60","canary_interval":30}`)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("put = %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	p := get("api")
	if p.AutoRollback || p.Strategy != "canary" || store.FormatCanarySteps(p.CanarySteps) != "5,25,60" || p.CanaryInterval != 30 || p.RolloutTimeout != 300 {
		t.Fatalf("after partial put = %+v", p)
	}

	for _, c := range []struct{ app, body, want string }{
		{"api", `{"canary_steps":"50,10"}`, "must increase"},
		{"api", `{"rollout_timeout":5}`, "rollout timeout"},
		{"api", `{"security":"yolo"}`, "security must be one of"},
		{"bg", `{"strategy":"canary"}`, "need a web app"},
	} {
		resp := doAuthed(t, "PUT", srv.URL+"/v1/projects/web/apps/"+c.app+"/policy", admin, c.body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), c.want) {
			t.Errorf("%s %s = %d %s, want 400 %q", c.app, c.body, resp.StatusCode, b, c.want)
		}
	}
}

func TestUIWireRolloutCard(t *testing.T) {
	u := newUIAI(t, nil)
	page := u.get(t, "/ui/projects/shop/apps/web?tab=wire")
	for _, want := range []string{`id="rollout-policy"`, `name="auto_rollback"`, `name="strategy"`, "luncur app set web --project shop"} {
		if !strings.Contains(page, want) {
			t.Fatalf("wire tab missing %q", want)
		}
	}
	resp, _ := u.htmx(t, "/ui/projects/shop/apps/web/policy", map[string][]string{
		"auto_rollback_present": {"1"}, "rollout_timeout": {"120"}, "security": {"relaxed"}, "strategy": {"bluegreen"},
	})
	if resp.StatusCode >= 400 {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	p, err := u.s.st.GetAppPolicy(u.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.AutoRollback || p.RolloutTimeout != 120 || p.Security != "relaxed" || p.Strategy != "bluegreen" {
		t.Fatalf("saved policy = %+v", p)
	}
}
