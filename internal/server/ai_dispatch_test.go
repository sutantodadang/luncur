package server

import (
	"context"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/aitools"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

func aiTestServer(t *testing.T) *server {
	t.Helper()
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return newServer(Deps{Store: newTestStore(t), Sealer: sealer, ExternalIP: "1.2.3.4"})
}

// dummyArgs fills every parameter of a tool with a placeholder value.
func dummyArgs(tool aitools.Tool) map[string]any {
	args := map[string]any{}
	for _, p := range tool.Params {
		switch p.Type {
		case "integer":
			args[p.Name] = float64(1)
		case "boolean":
			args[p.Name] = true
		case "object":
			args[p.Name] = map[string]any{"metadata": map[string]any{"labels": map[string]any{"a": "b"}}}
		default:
			if len(p.Enum) > 0 {
				args[p.Name] = p.Enum[0]
			} else {
				args[p.Name] = "nosuch"
			}
		}
	}
	return args
}

// Every registry tool must resolve to a real API route, both in its
// project-default form and (for env-scoped tools) its env-qualified form:
// the catch-all answers "no such endpoint", a wrong method 405.
func TestAIToolsMapToRealRoutes(t *testing.T) {
	s := aiTestServer(t)
	admin, err := s.st.CreateUser("root@b.co", "pw-123456", "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range aitools.All() {
		variants := []map[string]any{dummyArgs(tool)}
		if tool.EnvScoped {
			withEnv := dummyArgs(tool)
			withEnv["env"] = "staging"
			variants = append(variants, withEnv)
		}
		for _, args := range variants {
			out, _ := s.aiCallTool(context.Background(), admin, tool, args)
			if strings.Contains(out, "no such endpoint") || strings.Contains(out, "HTTP 405") {
				r, _ := tool.BuildRequest(args)
				t.Errorf("%s -> %s %s: %s", tool.Name, r.Method, r.Path, out)
			}
		}
	}
}

// Act within role: a viewer's tool call is refused exactly like their API
// call would be, and a member's succeeds and is audited "via ai".
func TestAIToolCallsRunAsTheCaller(t *testing.T) {
	s := aiTestServer(t)
	p, err := s.st.CreateProject("shop")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = seedDefaultEnv(t, s.st, p)
	if _, err := s.st.CreateAppInEnv(mustEnv(t, s.st, p).ID, "web", 8080, "web", ""); err != nil {
		t.Fatal(err)
	}
	viewer, _ := s.st.CreateUser("v@b.co", "pw-123456", "member")
	member, _ := s.st.CreateUser("m@b.co", "pw-123456", "member")
	if err := s.st.AddMember(p.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := s.st.AddMember(p.ID, member.ID, "member"); err != nil {
		t.Fatal(err)
	}
	setEnv, _ := aitools.Find("set_env")
	args := map[string]any{"project": "shop", "app": "web", "key": "MODE", "value": "fast"}

	out, isErr := s.aiCallTool(context.Background(), viewer, setEnv, args)
	if !isErr || !strings.Contains(out, "HTTP 403") {
		t.Fatalf("viewer set_env = %q (err=%v), want a 403", out, isErr)
	}
	if s.aiCanWrite(viewer, "shop") {
		t.Fatal("aiCanWrite(viewer) = true")
	}

	if out, isErr := s.aiCallTool(context.Background(), member, setEnv, args); isErr {
		t.Fatalf("member set_env failed: %s", out)
	}
	entries, err := s.st.ListAudit(10, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.UserEmail == "m@b.co" && strings.HasSuffix(e.Action, "(via ai)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no 'via ai' audit row for the member's call: %+v", entries)
	}

	// A tool call made inside another audited request must not overwrite
	// that request's audit row (it gets its own).
	outer := &auditInfo{Email: "m@b.co", Pattern: "POST /v1/ai/chat"}
	ctx := context.WithValue(context.Background(), auditCtxKey{}, outer)
	getApp, _ := aitools.Find("get_app")
	s.aiCallTool(ctx, member, getApp, map[string]any{"project": "shop", "app": "web"})
	s.aiCallTool(ctx, member, setEnv, map[string]any{"project": "shop", "app": "web", "key": "B", "value": "2"})
	if outer.Pattern != "POST /v1/ai/chat" {
		t.Fatalf("outer audit pattern clobbered to %q", outer.Pattern)
	}

	// Values never reach the model.
	listKeys, _ := aitools.Find("list_env_keys")
	out, _ = s.aiCallTool(context.Background(), member, listKeys, map[string]any{"project": "shop", "app": "web"})
	if strings.Contains(out, "fast") || !strings.Contains(out, "MODE") {
		t.Fatalf("list_env_keys = %q", out)
	}
}

func mustEnv(t *testing.T, st *store.Store, p store.Project) store.Environment {
	t.Helper()
	env, err := st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		t.Fatal(err)
	}
	return env
}
