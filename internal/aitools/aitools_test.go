package aitools

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustTool(t *testing.T, name string) Tool {
	t.Helper()
	tool, ok := Find(name)
	if !ok {
		t.Fatalf("no tool %q", name)
	}
	return tool
}

func TestBuildRequestPathsEnvQueryBody(t *testing.T) {
	cases := []struct {
		tool       string
		args       string
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{"scale_app", `{"project":"shop","app":"web","replicas":3}`, "POST", "/v1/projects/shop/apps/web/scale", `{"replicas":3}`},
		{"scale_app", `{"project":"shop","env":"staging","app":"web","replicas":2}`, "POST", "/v1/projects/shop/envs/staging/apps/web/scale", `{"replicas":2}`},
		{"app_logs", `{"project":"shop","app":"web","tail":200}`, "GET", "/v1/projects/shop/apps/web/logs?tail=200", ""},
		{"delete_volume", `{"project":"shop","app":"web","name":"data","purge":true}`, "DELETE", "/v1/projects/shop/apps/web/volumes/data?purge=1", ""},
		{"delete_volume", `{"project":"shop","app":"web","name":"data","purge":false}`, "DELETE", "/v1/projects/shop/apps/web/volumes/data", ""},
		{"set_override", `{"project":"shop","app":"web","kind":"Deployment","patch":{"spec":{"replicas":2}}}`, "PUT", "/v1/projects/shop/apps/web/overrides/Deployment", `{"spec":{"replicas":2}}`},
		{"redeploy", `{"project":"shop","app":"web"}`, "POST", "/v1/projects/shop/apps/web/redeploy", `{}`},
		{"remove_domain", `{"project":"shop","app":"web","hostname":"a b.example.com"}`, "DELETE", "/v1/projects/shop/apps/web/domains/a%20b.example.com", ""},
		{"delete_env", `{"project":"shop","env":"qa"}`, "DELETE", "/v1/projects/shop/envs/qa", ""},
	}
	for _, c := range cases {
		var args map[string]any
		if err := json.Unmarshal([]byte(c.args), &args); err != nil {
			t.Fatal(err)
		}
		r, err := mustTool(t, c.tool).BuildRequest(args)
		if err != nil {
			t.Errorf("%s %s: %v", c.tool, c.args, err)
			continue
		}
		if r.Method != c.wantMethod || r.Path != c.wantPath || string(r.Body) != c.wantBody {
			t.Errorf("%s %s = %s %s %s, want %s %s %s", c.tool, c.args, r.Method, r.Path, r.Body, c.wantMethod, c.wantPath, c.wantBody)
		}
	}
}

func TestBuildRequestMissingRequired(t *testing.T) {
	if _, err := mustTool(t, "set_env").BuildRequest(map[string]any{"project": "shop", "app": "web", "key": "A"}); err == nil || !strings.Contains(err.Error(), `"value"`) {
		t.Fatalf("err = %v, want missing value", err)
	}
	if _, err := mustTool(t, "get_app").BuildRequest(map[string]any{"project": "shop", "app": ""}); err == nil {
		t.Fatal("empty path argument accepted")
	}
}

func TestCLIEcho(t *testing.T) {
	got := mustTool(t, "scale_app").CLIEcho(map[string]any{"project": "shop", "env": "production", "app": "web", "replicas": float64(3)})
	if got != "luncur scale web --replicas 3 --project shop --env production" {
		t.Fatalf("echo = %q", got)
	}
	got = mustTool(t, "set_env").CLIEcho(map[string]any{"project": "shop", "app": "web", "key": "API_URL", "value": "secret"})
	if strings.Contains(got, "secret") || got != "luncur env set web API_URL=<value> --project shop" {
		t.Fatalf("echo = %q (must not leak the value)", got)
	}
	if got := mustTool(t, "get_pipeline").CLIEcho(map[string]any{"project": "ml", "name": "nightly"}); got != "# API: GET /v1/projects/ml/pipelines/nightly" {
		t.Fatalf("echo = %q", got)
	}
}

func TestShapeResponse(t *testing.T) {
	out, isErr := mustTool(t, "list_env_keys").ShapeResponse(200, []byte(`{"DATABASE_URL":"postgres://u:pw@db","B":"x"}`))
	if isErr || strings.Contains(out, "pw@db") || !strings.Contains(out, `"DATABASE_URL"`) {
		t.Fatalf("env shape = %q", out)
	}
	out, _ = mustTool(t, "app_logs").ShapeResponse(200, []byte("data: [web-1] hello\n\ndata: [web-1] bye\n\nevent: end\ndata: eof\n\n"))
	if out != "[web-1] hello\n[web-1] bye" {
		t.Fatalf("sse shape = %q", out)
	}
	out, isErr = mustTool(t, "get_app").ShapeResponse(403, []byte(`{"error":{"code":"forbidden","message":"viewers cannot modify"}}`))
	if !isErr || out != "HTTP 403 forbidden: viewers cannot modify" {
		t.Fatalf("error shape = %q %v", out, isErr)
	}
}

func TestFilterAndSchemas(t *testing.T) {
	for _, tool := range Filter(false, false) {
		if tool.Mutating || tool.Admin {
			t.Errorf("read-only caller got %s", tool.Name)
		}
	}
	for _, tool := range Filter(false, true) {
		if tool.Admin {
			t.Errorf("non-admin got %s", tool.Name)
		}
	}
	if len(Filter(true, false)) != len(All()) {
		t.Error("admin should get every tool")
	}
	seen := map[string]bool{}
	for _, tool := range All() {
		if seen[tool.Name] {
			t.Errorf("duplicate tool %s", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		s := tool.Schema()
		for _, r := range s["required"].([]string) {
			if _, ok := s["properties"].(map[string]any)[r]; !ok {
				t.Errorf("%s requires undeclared %s", tool.Name, r)
			}
		}
		if tool.Destructive && !tool.Mutating {
			t.Errorf("%s is destructive but not mutating", tool.Name)
		}
	}
}
