package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// claudeFake answers /v1/messages with canned bodies in order, recording
// each request body and header set.
type claudeFake struct {
	mu      sync.Mutex
	replies []string
	bodies  []map[string]any
	headers []http.Header
}

func (f *claudeFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		f.bodies = append(f.bodies, m)
		f.headers = append(f.headers, r.Header.Clone())
		if len(f.replies) == 0 {
			http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no more replies"}}`, http.StatusBadRequest)
			return
		}
		reply := f.replies[0]
		f.replies = f.replies[1:]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const claudeToolUseReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
 "content":[{"type":"thinking","thinking":"","signature":"sig-abc"},
            {"type":"text","text":"Let me look."},
            {"type":"tool_use","id":"tu_1","name":"list_apps","input":{"project":"shop"}}],
 "stop_reason":"tool_use","stop_details":null,
 "usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}`

const claudeFinalReply = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5-5",
 "content":[{"type":"text","text":"shop has 2 apps."}],
 "stop_reason":"end_turn","stop_details":null,
 "usage":{"input_tokens":10,"output_tokens":5}}`

func TestClaudeToolLoopRoundTrip(t *testing.T) {
	f := &claudeFake{replies: []string{claudeToolUseReply, claudeFinalReply}}
	srv := f.server(t)
	p := NewClaude(ClaudeConfig{APIKey: "k", BaseURL: srv.URL})

	req := Request{
		System:   "you are luncur",
		Messages: []Message{{Role: RoleUser, Text: "what apps does shop have?"}},
		Tools: []Tool{{Name: "list_apps", Description: "list apps", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"project": map[string]any{"type": "string"}}, "required": []string{"project"},
		}}},
		Effort: "medium",
	}
	var calls []string
	res, err := Run(context.Background(), p, req, func(_ context.Context, c ToolCall) (string, bool, bool) {
		calls = append(calls, c.Name+" "+string(c.Input))
		return `[{"name":"web"},{"name":"worker"}]`, false, false
	}, LoopConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "shop has 2 apps." || len(calls) != 1 || calls[0] != `list_apps {"project":"shop"}` {
		t.Fatalf("final=%q calls=%v", res.Final, calls)
	}
	if res.Usage.InputTokens != 160 || res.Usage.OutputTokens != 25 {
		t.Fatalf("usage = %+v, want cache reads counted as input (160/25)", res.Usage)
	}

	first := f.bodies[0]
	if first["model"] != DefaultClaudeModel {
		t.Errorf("model = %v", first["model"])
	}
	if oc, _ := first["output_config"].(map[string]any); oc["effort"] != "medium" {
		t.Errorf("output_config = %v, want effort medium", first["output_config"])
	}
	if first["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want \"default\"", first["fallbacks"])
	}
	if got := f.headers[0].Get("anthropic-beta"); !strings.Contains(got, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", got)
	}
	if _, forced := first["tool_choice"]; forced {
		t.Errorf("tool_choice sent (%v); Opus 5.5 rejects forced tool use, leave it auto", first["tool_choice"])
	}
	sys, _ := first["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("system = %v, want one cached block", first["system"])
	}

	// Second request: the assistant turn is replayed unchanged (thinking
	// block with its signature first), then the tool result.
	msgs := f.bodies[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request has %d messages, want 3", len(msgs))
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if b := asst[0].(map[string]any); b["type"] != "thinking" || b["signature"] != "sig-abc" {
		t.Errorf("assistant replay block 0 = %v, want the thinking block unchanged", b)
	}
	tr := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "tu_1" {
		t.Errorf("tool result block = %v", tr)
	}
}

func TestClaudeRefusal(t *testing.T) {
	f := &claudeFake{replies: []string{`{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5",
	 "content":[],"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},
	 "usage":{"input_tokens":1,"output_tokens":1}}`}}
	p := NewClaude(ClaudeConfig{APIKey: "k", BaseURL: f.server(t).URL})
	_, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("err = %v, want ErrRefused with explanation", err)
	}
}

func TestClaudeHaikuGetsNoEffortOrFallbacks(t *testing.T) {
	f := &claudeFake{replies: []string{claudeFinalReply}}
	p := NewClaude(ClaudeConfig{APIKey: "k", BaseURL: f.server(t).URL, Model: "claude-haiku-4-5"})
	if _, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Text: "x"}}, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.bodies[0]["output_config"]; ok {
		t.Errorf("output_config sent to haiku: %v", f.bodies[0]["output_config"])
	}
	if _, ok := f.bodies[0]["fallbacks"]; ok {
		t.Errorf("fallbacks sent to haiku")
	}
}

func TestOpenAICompatToolCalls(t *testing.T) {
	var bodies []map[string]any
	replies := []string{
		`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"list_apps","arguments":"{\"project\":\"shop\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		`{"choices":[{"message":{"content":"two apps"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		_, _ = w.Write([]byte(replies[0]))
		replies = replies[1:]
	}))
	defer srv.Close()

	p := &OpenAICompat{BaseURL: srv.URL + "/v1", APIKey: "sk", Model: "gemma"}
	res, err := Run(context.Background(), p, Request{
		System:   "sys",
		Messages: []Message{{Role: RoleUser, Text: "apps?"}},
		Tools:    []Tool{{Name: "list_apps", Schema: map[string]any{"type": "object", "properties": map[string]any{}}}},
	}, func(_ context.Context, c ToolCall) (string, bool, bool) { return "[web, worker]", false, false }, LoopConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "two apps" || res.Usage.Total() != 21 {
		t.Fatalf("final=%q usage=%+v", res.Final, res.Usage)
	}
	second := bodies[1]["messages"].([]any)
	last := second[len(second)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || last["content"] != "[web, worker]" {
		t.Fatalf("tool message = %v", last)
	}
	if second[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message = %v, want system", second[0])
	}
}

// scripted is a fake Provider replaying canned responses.
type scripted struct {
	resps []Response
	reqs  []Request
}

func (s *scripted) Name() string { return "fake" }
func (s *scripted) Chat(_ context.Context, req Request) (Response, error) {
	s.reqs = append(s.reqs, req)
	if len(s.resps) == 0 {
		return Response{Message: Message{Role: RoleAssistant, Text: "done"}, StopReason: StopEnd}, nil
	}
	r := s.resps[0]
	s.resps = s.resps[1:]
	return r, nil
}

func toolTurn(calls ...ToolCall) Response {
	return Response{Message: Message{Role: RoleAssistant, ToolCalls: calls}, StopReason: StopToolUse, Usage: Usage{InputTokens: 10, OutputTokens: 1}}
}

func TestRunStopsOnExecutorStop(t *testing.T) {
	p := &scripted{resps: []Response{toolTurn(ToolCall{ID: "1", Name: "submit", Input: json.RawMessage(`{"content":"x"}`)})}}
	res, err := Run(context.Background(), p, Request{}, func(context.Context, ToolCall) (string, bool, bool) { return "accepted", false, true }, LoopConfig{})
	if err != nil || !res.Stopped || res.Steps != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRunMaxStepsAndBudget(t *testing.T) {
	loop := func() *scripted {
		var rs []Response
		for i := 0; i < 10; i++ {
			rs = append(rs, toolTurn(ToolCall{ID: "x", Name: "t", Input: json.RawMessage(`{}`)}))
		}
		return &scripted{resps: rs}
	}
	noop := func(context.Context, ToolCall) (string, bool, bool) { return "ok", false, false }
	if _, err := Run(context.Background(), loop(), Request{}, noop, LoopConfig{MaxSteps: 3}); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("err = %v, want ErrMaxSteps", err)
	}
	over := errors.New("over budget")
	res, err := Run(context.Background(), loop(), Request{}, noop, LoopConfig{Budget: func(u Usage) error {
		if u.Total() >= 20 {
			return over
		}
		return nil
	}})
	if !errors.Is(err, over) || res.Steps != 2 {
		t.Fatalf("steps=%d err=%v, want budget stop after 2 calls", res.Steps, err)
	}
}

func TestRunRejectsInvalidToolJSONAndTruncates(t *testing.T) {
	p := &scripted{resps: []Response{toolTurn(
		ToolCall{ID: "bad", Name: "t", Input: json.RawMessage(`{"a":`)},
		ToolCall{ID: "big", Name: "t", Input: json.RawMessage(`{}`)},
	)}}
	ran := 0
	res, err := Run(context.Background(), p, Request{}, func(context.Context, ToolCall) (string, bool, bool) {
		ran++
		return strings.Repeat("x", 100) + "TAIL", false, false
	}, LoopConfig{MaxResultBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	trs := res.Messages[1].ToolResults
	if ran != 1 || !trs[0].IsError || !strings.HasSuffix(trs[1].Content, "xxxxxxTAIL") || len(trs[1].Content) > 40 {
		t.Fatalf("ran=%d results=%+v", ran, trs)
	}
}

func TestRedactorAndDecodeJSONObject(t *testing.T) {
	r := NewRedactor("sk_live_abcdef", "abc", "sk_live_abcdef_long")
	got := r.Redact("key=sk_live_abcdef_long other=sk_live_abcdef abc")
	if got != "key=[REDACTED] other=[REDACTED] abc" {
		t.Fatalf("redact = %q", got)
	}
	var v struct{ Broke string }
	if err := DecodeJSONObject("Sure!\n```json\n{\"broke\":\"build\"}\n```", &v); err != nil || v.Broke != "build" {
		t.Fatalf("decode = %+v, %v", v, err)
	}
}
