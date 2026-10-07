package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/ai"
)

// uiAI wires an aiFixture to a real HTTP stack with an admin session.
type uiAI struct {
	aiFixture
	srv    *httptest.Server
	client *http.Client
	csrf   *http.Cookie
	sess   *http.Cookie
}

func newUIAI(t *testing.T, f *fakeAI) uiAI {
	t.Helper()
	fx := newAIFixture(t, f)
	srv := httptest.NewServer(fx.s.handler())
	t.Cleanup(srv.Close)
	client := noRedirectClient()
	return uiAI{aiFixture: fx, srv: srv, client: client, csrf: uiCSRF(t, client, srv.URL), sess: uiSessionCookie(t, fx.s.st, fx.admin.ID)}
}

func (u uiAI) get(t *testing.T, path string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", u.srv.URL+path, nil)
	req.AddCookie(u.sess)
	resp, err := u.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

// htmx posts a form the way htmx does (HX-Request header).
func (u uiAI) htmx(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	form.Set("_csrf", u.csrf.Value)
	req, _ := http.NewRequest("POST", u.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.AddCookie(u.csrf)
	req.AddCookie(u.sess)
	resp, err := u.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestUIAssistantPageOffAndOn(t *testing.T) {
	off := newUIAI(t, nil)
	page := off.get(t, "/ui/assistant")
	if !strings.Contains(page, "The assistant is off.") || !strings.Contains(page, "luncur config set ai_provider claude") {
		t.Fatalf("disabled assistant page:\n%s", page)
	}
	if !strings.Contains(page, `href="/ui/assistant"`) {
		t.Fatal("sidebar has no Assistant link")
	}

	on := newUIAI(t, &fakeAI{reply: func(int, ai.Request) ai.Response { return text("ok") }})
	page = on.get(t, "/ui/assistant")
	for _, want := range []string{`id="ai-ask-form"`, "luncur ai ask", "Generate config", "claude mcp add luncur -- luncur mcp"} {
		if !strings.Contains(page, want) {
			t.Fatalf("assistant page missing %q", want)
		}
	}
}

func TestUIAssistantAskRendersActionsAsCLIEcho(t *testing.T) {
	f := &fakeAI{reply: func(n int, req ai.Request) ai.Response {
		if n == 0 {
			return toolCall("t1", "set_env", `{"app":"web","key":"MODE","value":"fast"}`)
		}
		return text("Set MODE on web.")
	}}
	u := newUIAI(t, f)
	resp, body := u.htmx(t, "/ui/assistant", url.Values{"project": {"shop"}, "message": {"set MODE=fast on web"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ask = %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{`id="ai-transcript"`, "chip-ok", "luncur env set web MODE=&lt;value&gt; --project shop", "Set MODE on web.", `name="conversation_id"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("transcript missing %q:\n%s", want, body)
		}
	}
}

func TestUIExplainButtonAndFragment(t *testing.T) {
	f := &fakeAI{reply: func(int, ai.Request) ai.Response {
		return text(`{"broke":"The build failed","why":"No Dockerfile or buildpack match.","next_command":"luncur redeploy web --project shop","confidence":"high"}`)
	}}
	u := newUIAI(t, f)
	if _, err := u.s.st.CreateDeployment(u.app.ID, "failed", "", 0); err != nil {
		t.Fatal(err)
	}
	ship := u.get(t, "/ui/projects/shop/apps/web?tab=ship")
	if !strings.Contains(ship, "/ui/projects/shop/apps/web/explain") || !strings.Contains(ship, "luncur ai explain web --project shop") {
		t.Fatalf("ship tab has no explain action:\n%s", ship)
	}
	resp, body := u.htmx(t, "/ui/projects/shop/apps/web/explain", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explain = %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{"The build failed", "No Dockerfile or buildpack match.", "$ luncur redeploy web --project shop", "high confidence"} {
		if !strings.Contains(body, want) {
			t.Fatalf("explain fragment missing %q:\n%s", want, body)
		}
	}
}

func TestUIGenerateFillsEditorOrReportsError(t *testing.T) {
	calls := 0
	f := &fakeAI{reply: func(int, ai.Request) ai.Response {
		calls++
		return toolCall("s", "submit", `{"content":"steps:\n  ping:\n    notify: done\n"}`)
	}}
	u := newUIAI(t, f)
	resp, body := u.htmx(t, "/ui/ai/generate", url.Values{
		"target": {"pipeline-yaml"}, "project": {"shop"}, "description": {"notify when done"}, "yaml": {"steps: {}"},
	})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<textarea id="pipeline-yaml"`) || !strings.Contains(body, `name="yaml"`) || !strings.Contains(body, "notify: done") {
		t.Fatalf("generate = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(f.reqs[0].Messages[0].Text, "steps: {}") {
		t.Fatal("the current editor content wasn't sent as the file to modify")
	}

	resp, body = u.htmx(t, "/ui/ai/generate", url.Values{"target": {"pipeline-yaml"}, "project": {"shop"}, "description": {""}})
	if resp.Header.Get("HX-Retarget") != "#pipeline-yaml-err" || !strings.Contains(body, "describe what the file should do") {
		t.Fatalf("empty description: retarget=%q body=%q", resp.Header.Get("HX-Retarget"), body)
	}
	if calls != 1 {
		t.Fatalf("model called %d times, want 1", calls)
	}
}

func TestUISettingsHasAIGroup(t *testing.T) {
	u := newUIAI(t, nil)
	page := u.get(t, "/ui/settings")
	for _, want := range []string{"AI assistant", `value="ai_provider"`, `value="ai_api_key"`, `value="ai_notify"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("settings page missing %q", want)
		}
	}
}
