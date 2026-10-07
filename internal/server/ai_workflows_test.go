package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/ai"
	"github.com/sutantodadang/luncur/internal/build"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

// fakeAI is a scripted ai.Provider: reply(n, req) answers the n-th call.
type fakeAI struct {
	mu    sync.Mutex
	reqs  []ai.Request
	reply func(n int, req ai.Request) ai.Response
}

func (f *fakeAI) Name() string { return "fake/model" }
func (f *fakeAI) Chat(_ context.Context, req ai.Request) (ai.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	r := f.reply(len(f.reqs)-1, req)
	if r.Usage.Total() == 0 {
		r.Usage = ai.Usage{InputTokens: 100, OutputTokens: 10}
	}
	return r, nil
}

func text(s string) ai.Response {
	return ai.Response{Message: ai.Message{Role: ai.RoleAssistant, Text: s}, StopReason: ai.StopEnd}
}

func toolCall(id, name, input string) ai.Response {
	return ai.Response{Message: ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: id, Name: name, Input: json.RawMessage(input)}}}, StopReason: ai.StopToolUse}
}

type aiFixture struct {
	s       *server
	p       store.Project
	env     store.Environment
	app     store.App
	admin   store.User
	member  store.User
	viewer  store.User
	dataDir string
}

func newAIFixture(t *testing.T, f *fakeAI) aiFixture {
	t.Helper()
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	s := newServer(Deps{Store: newTestStore(t), Sealer: sealer, ExternalIP: "1.2.3.4", DataDir: dataDir})
	if f != nil {
		s.aiProviderFn = func() (ai.Provider, error) { return f, nil }
	}
	p, err := s.st.CreateProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	p, env := seedDefaultEnv(t, s.st, p)
	a, err := s.st.CreateAppInEnv(env.ID, "web", 8080, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	fx := aiFixture{s: s, p: p, env: env, app: a, dataDir: dataDir}
	fx.admin, _ = s.st.CreateUser("root@b.co", "pw-123456", "admin")
	fx.member, _ = s.st.CreateUser("m@b.co", "pw-123456", "member")
	fx.viewer, _ = s.st.CreateUser("v@b.co", "pw-123456", "member")
	_ = s.st.AddMember(p.ID, fx.member.ID, "member")
	_ = s.st.AddMember(p.ID, fx.viewer.ID, "viewer")
	return fx
}

func (fx aiFixture) setSecretEnv(t *testing.T, key, value string) {
	t.Helper()
	sealed, err := fx.s.sealer.Seal([]byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.s.st.SetEnv(fx.app.ID, key, sealed); err != nil {
		t.Fatal(err)
	}
}

func TestAIExplainGathersRedactedContextWithoutTools(t *testing.T) {
	f := &fakeAI{reply: func(int, ai.Request) ai.Response {
		return text(`{"broke":"The deploy failed","why":"The app can't reach Stripe","next_command":"luncur redeploy web --project shop","confidence":"medium"}`)
	}}
	fx := newAIFixture(t, f)
	fx.setSecretEnv(t, "STRIPE_KEY", "sk_live_TOPSECRET42")
	d, err := fx.s.st.CreateDeployment(fx.app.ID, "failed", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := build.NewSource(fx.dataDir)
	if err := os.WriteFile(src.LogPath(d.ID), []byte("step 3/7\nerror: auth failed for key sk_live_TOPSECRET42\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, usage, err := fx.s.aiExplain(context.Background(), fx.viewer, aiExplainInput{Project: fx.p, Env: fx.env, App: fx.app})
	if err != nil {
		t.Fatal(err)
	}
	if got.Broke != "The deploy failed" || got.NextCommand == "" || usage.Total() == 0 {
		t.Fatalf("diagnosis = %+v usage=%+v", got, usage)
	}
	req := f.reqs[0]
	if len(req.Tools) != 0 || req.JSONSchema == nil || req.Effort != "low" {
		t.Fatalf("explain request: tools=%d schema=%v effort=%q — must be one tool-free structured call", len(req.Tools), req.JSONSchema != nil, req.Effort)
	}
	ctxText := req.Messages[0].Text
	if !strings.Contains(ctxText, "error: auth failed") || !strings.Contains(ctxText, "[REDACTED]") || strings.Contains(ctxText, "TOPSECRET") {
		t.Fatalf("context not gathered/redacted:\n%s", ctxText)
	}
}

func TestAIChatActsWithinRoleAndKeepsConversation(t *testing.T) {
	f := &fakeAI{reply: func(n int, req ai.Request) ai.Response {
		switch n {
		case 0:
			return toolCall("t1", "set_env", `{"app":"web","key":"MODE","value":"fast"}`)
		case 1:
			return text("Set MODE on web.")
		default:
			return text("Nothing else to do.")
		}
	}}
	fx := newAIFixture(t, f)

	res, err := fx.s.aiChat(context.Background(), fx.member, "", "shop", "", "set MODE=fast on web")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "Set MODE on web." || len(res.Actions) != 1 || !res.Actions[0].OK || !res.Actions[0].Mutating {
		t.Fatalf("chat = %+v", res)
	}
	if res.Actions[0].CLI != "luncur env set web MODE=<value> --project shop" {
		t.Fatalf("cli echo = %q", res.Actions[0].CLI)
	}
	if env, _ := fx.s.st.ListEnv(fx.app.ID); env["MODE"] == nil {
		t.Fatal("MODE was not set")
	}

	// Same conversation: the next request carries the history.
	if _, err := fx.s.aiChat(context.Background(), fx.member, res.ConversationID, "", "", "anything else?"); err != nil {
		t.Fatal(err)
	}
	if got := len(f.reqs[2].Messages); got != 5 {
		t.Fatalf("follow-up request has %d messages, want 5 (user, tool call, results, reply, user)", got)
	}
	if turns := fx.s.aiChatTurns(res.ConversationID, fx.member); len(turns) != 2 {
		t.Fatalf("turns = %d", len(turns))
	}
	if fx.s.aiChatTurns(res.ConversationID, fx.viewer) != nil {
		t.Fatal("another user can read the conversation")
	}
}

func TestAIChatViewerGetsReadOnlyTools(t *testing.T) {
	f := &fakeAI{reply: func(int, ai.Request) ai.Response { return text("ok") }}
	fx := newAIFixture(t, f)
	if _, err := fx.s.aiChat(context.Background(), fx.viewer, "", "shop", "", "what's running?"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range f.reqs[0].Tools {
		if tool.Name == "set_env" || tool.Name == "delete_app" || tool.Name == "doctor" {
			t.Fatalf("viewer was offered %s", tool.Name)
		}
	}
	if len(f.reqs[0].Tools) == 0 {
		t.Fatal("viewer got no tools at all")
	}
}

func TestAIGenerateLoopsUntilValid(t *testing.T) {
	f := &fakeAI{reply: func(n int, req ai.Request) ai.Response {
		switch n {
		case 0:
			return toolCall("s1", "submit", `{"content":"steps:\n  Bad_Name:\n    notify: hi\n"}`)
		default:
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) == 0 || !last.ToolResults[0].IsError {
				t.Errorf("model didn't see the validation error: %+v", last)
			}
			return toolCall("s2", "submit", `{"content":"steps:\n  ping:\n    notify: training finished\n"}`)
		}
	}}
	fx := newAIFixture(t, f)
	res, err := fx.s.aiGenerate(context.Background(), fx.member, aiGenerateInput{Kind: "pipeline", Description: "notify when done", Project: &fx.p})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 2 || !strings.Contains(res.Content, "ping:") {
		t.Fatalf("generate = %+v", res)
	}
}

func TestAIBudgetAndDisabled(t *testing.T) {
	f := &fakeAI{reply: func(int, ai.Request) ai.Response { return text("{}") }}
	fx := newAIFixture(t, f)
	if err := fx.s.st.SetSetting(settingAIDailyBudget, "50"); err != nil {
		t.Fatal(err)
	}
	if err := fx.s.st.AddAIUsage(fx.admin.ID, "chat", 40, 20); err != nil {
		t.Fatal(err)
	}
	_, _, err := fx.s.aiExplain(context.Background(), fx.admin, aiExplainInput{Project: fx.p, Env: fx.env, App: fx.app})
	if !errors.Is(err, errAIBudget) || len(f.reqs) != 0 {
		t.Fatalf("err = %v calls=%d, want budget stop before any call", err, len(f.reqs))
	}

	off := newAIFixture(t, nil) // no provider configured
	if _, err := off.s.aiSetup(context.Background()); !errors.Is(err, errAIDisabled) {
		t.Fatalf("aiSetup without settings = %v, want errAIDisabled", err)
	}
	srv := httptest.NewServer(off.s.handler())
	defer srv.Close()
	tok, _ := off.s.st.CreateToken(off.admin.ID, "t")
	resp := doAuthed(t, "POST", srv.URL+"/v1/ai/explain", tok, `{"project":"shop","app":"web"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "ai_disabled") {
		t.Fatalf("explain while disabled = %d %s", resp.StatusCode, body)
	}
}

func TestAISetupFromSettings(t *testing.T) {
	fx := newAIFixture(t, nil)
	if err := fx.s.setSetting(settingAIProvider, "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.aiSetup(context.Background()); !errors.Is(err, errAIDisabled) {
		t.Fatalf("claude without key = %v, want errAIDisabled", err)
	}
	if err := fx.s.setSetting(settingAIKey, "sk-ant-test"); err != nil {
		t.Fatal(err)
	}
	cfg, err := fx.s.aiSetup(context.Background())
	if err != nil || cfg.provider.Name() != "claude/"+ai.DefaultClaudeModel {
		t.Fatalf("cfg=%v err=%v", cfg.provider, err)
	}
	if v, _ := fx.s.st.GetSetting(settingAIKey); !strings.HasPrefix(v, "sealed:") {
		t.Fatalf("ai_api_key stored as %q, want sealed", v)
	}

	// A luncur model app as the backend.
	if _, err := fx.s.st.CreateAppInEnv(fx.env.ID, "chat", 0, "model", ""); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{settingAIProvider: "openai", settingAIModel: "gemma", settingAIBaseURL: "app:shop/chat"} {
		if err := fx.s.setSetting(k, v); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	cfg, err = fx.s.aiSetup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oa, ok := cfg.provider.(*ai.OpenAICompat)
	if !ok || oa.BaseURL != "http://chat."+fx.env.Namespace+":80/v1" {
		t.Fatalf("provider = %#v", cfg.provider)
	}
}

func TestAINotifySummaryAppendsDiagnosis(t *testing.T) {
	f := &fakeAI{reply: func(int, ai.Request) ai.Response {
		return text(`{"broke":"Build failed","why":"Missing package.json.","next_command":"luncur redeploy web --project shop","confidence":"high"}`)
	}}
	fx := newAIFixture(t, f)
	var mu sync.Mutex
	var got []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got, _ = io.ReadAll(r.Body)
		mu.Unlock()
	}))
	defer hook.Close()
	setSealedNotifyURL(t, fx.s, hook.URL)
	if err := fx.s.st.SetSetting(settingAINotify, "on"); err != nil {
		t.Fatal(err)
	}
	d, _ := fx.s.st.CreateDeployment(fx.app.ID, "failed", "", 0)

	fx.s.notify(notifyEvent{Event: "deploy_failed", Project: "shop", App: "web", DeployID: d.ID, Seq: d.Seq, Err: "build failed"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		body := got
		mu.Unlock()
		if body != nil {
			var p genericNotifyPayload
			_ = json.Unmarshal(body, &p)
			if !strings.Contains(p.AI, "Missing package.json.") || !strings.Contains(p.AI, "luncur redeploy web") {
				t.Fatalf("payload ai = %q", p.AI)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no notification delivered")
}
