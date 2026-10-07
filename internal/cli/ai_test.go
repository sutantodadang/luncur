package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/aitools"
)

func TestAIExplainCommandPrintsErrorContract(t *testing.T) {
	var got map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ai/explain", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"broke": "The build failed", "why": "npm can't find package.json",
			"next_command": "luncur redeploy web --project shop", "confidence": "high",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setCLIConfig(t, srv.URL)

	out, err := run(t, "ai", "explain", "web", "--project", "shop", "--env", "staging")
	if err != nil {
		t.Fatalf("ai explain: %v (%s)", err, out)
	}
	for _, want := range []string{"what broke   The build failed", "likely why   npm can't find package.json", "next         $ luncur redeploy web --project shop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if got["project"] != "shop" || got["env"] != "staging" || got["app"] != "web" {
		t.Fatalf("request = %v", got)
	}
}

func TestAIAskCommandShowsActionsAsCLI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ai/chat", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"conversation_id": "c123", "reply": "Scaled web to 3.",
			"actions": []map[string]any{{"tool": "scale_app", "cli": "luncur scale web --replicas 3 --project shop", "mutating": true, "ok": true}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setCLIConfig(t, srv.URL)

	out, err := run(t, "ai", "ask", "scale", "web", "to", "3", "--project", "shop")
	if err != nil {
		t.Fatalf("ai ask: %v (%s)", err, out)
	}
	for _, want := range []string{"[ok ] $ luncur scale web --replicas 3 --project shop", "Scaled web to 3.", "--conversation c123"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestServeMCP(t *testing.T) {
	tools := aitools.Filter(false, true)
	var calls []string
	exec := func(tool aitools.Tool, args map[string]any) (string, bool) {
		b, _ := json.Marshal(args)
		calls = append(calls, tool.Name+" "+string(b))
		return `[{"name":"web"}]`, false
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_apps","arguments":{"project":"shop"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"doctor","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"bogus"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out, tools, exec); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad json line %q", sc.Text())
		}
		resps = append(resps, m)
	}
	if len(resps) != 5 {
		t.Fatalf("got %d responses, want 5 (the notification gets none): %v", len(resps), resps)
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize = %v", init)
	}
	listed := resps[1]["result"].(map[string]any)["tools"].([]any)
	if len(listed) != len(tools) {
		t.Fatalf("listed %d tools, want %d", len(listed), len(tools))
	}
	for _, raw := range listed {
		tool := raw.(map[string]any)
		ann := tool["annotations"].(map[string]any)
		if tool["name"] == "delete_app" && (ann["destructiveHint"] != true || ann["readOnlyHint"] != false) {
			t.Fatalf("delete_app annotations = %v", ann)
		}
		if tool["name"] == "list_apps" && ann["readOnlyHint"] != true {
			t.Fatalf("list_apps annotations = %v", ann)
		}
	}
	call := resps[2]["result"].(map[string]any)
	if call["isError"] != false || !strings.Contains(call["content"].([]any)[0].(map[string]any)["text"].(string), "web") {
		t.Fatalf("tools/call = %v", call)
	}
	if len(calls) != 1 || calls[0] != `list_apps {"project":"shop"}` {
		t.Fatalf("calls = %v", calls)
	}
	if resps[3]["error"] == nil {
		t.Fatalf("admin-only doctor callable by a non-admin MCP session: %v", resps[3])
	}
	if e := resps[4]["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Fatalf("unknown method = %v", e)
	}
}

// End to end through the real client: tool calls reach the server with the
// user's token and the right method/path/body.
func TestMCPExecutorUsesAPIWithToken(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	setCLIConfig(t, srv.URL)
	c, err := apiClient()
	if err != nil {
		t.Fatal(err)
	}
	tool, _ := aitools.Find("scale_app")
	req, err := tool.BuildRequest(map[string]any{"project": "shop", "env": "staging", "app": "web", "replicas": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := c.Call(req.Method, req.Path, req.Body)
	if err != nil || status != 200 {
		t.Fatalf("call: %d %v", status, err)
	}
	if out, isErr := tool.ShapeResponse(status, body); isErr || out != "OK (HTTP 200)" {
		t.Fatalf("shape = %q %v", out, isErr)
	}
	if gotAuth != "Bearer test-token" || gotPath != "POST /v1/projects/shop/envs/staging/apps/web/scale" || gotBody != `{"replicas":2}` {
		t.Fatalf("server saw %q %q %q", gotAuth, gotPath, gotBody)
	}
}
