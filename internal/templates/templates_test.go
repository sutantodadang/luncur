package templates

import (
	"strings"
	"testing"
)

func TestCatalogParsesAndResolves(t *testing.T) {
	all := All()
	if len(all) != 6 {
		t.Fatalf("catalog has %d templates, want 6", len(all))
	}
	for _, tpl := range all {
		conns := map[string]Conn{}
		for _, a := range tpl.Addons {
			conns[a.Key] = Conn{Host: "web-db.ns", Port: "5432", User: "app", Password: "pw", Database: "app", URL: "postgresql://app:pw@web-db.ns:5432/app"}
		}
		env, err := tpl.Resolve("http://web.example", "web.example", conns, map[string]string{"EXTRA": "1"})
		if err != nil {
			t.Fatalf("%s: %v", tpl.Name, err)
		}
		for k, v := range env {
			if strings.Contains(v, "${") {
				t.Errorf("%s: %s unresolved: %s", tpl.Name, k, v)
			}
		}
		if env["EXTRA"] != "1" {
			t.Errorf("%s: user env lost", tpl.Name)
		}
	}
	n8n, ok := Find("n8n")
	if !ok {
		t.Fatal("n8n missing")
	}
	env, _ := n8n.Resolve("http://a", "a", map[string]Conn{"db": {Host: "h", Password: "p"}}, nil)
	if env["DB_POSTGRESDB_HOST"] != "h" || len(env["N8N_ENCRYPTION_KEY"]) != 32 || env["WEBHOOK_URL"] != "http://a/" {
		t.Fatalf("n8n env = %v", env)
	}
}

func TestValidateRejects(t *testing.T) {
	base := Template{Name: "x1", Title: "X", Description: "d", App: App{Image: "x:1", Port: 80}}
	for name, mut := range map[string]func(*Template){
		"latest":      func(t *Template) { t.App.Image = "x:latest" },
		"untagged":    func(t *Template) { t.App.Image = "x" },
		"bad addon":   func(t *Template) { t.Env = map[string]string{"A": "${addon.db.host}"} },
		"bad field":   func(t *Template) { t.Addons = []Addon{{Key: "db", Type: "postgres"}}; t.Env = map[string]string{"A": "${addon.db.nope}"} },
		"short rand":  func(t *Template) { t.Env = map[string]string{"A": "${random:2}"} },
		"unknown":     func(t *Template) { t.Env = map[string]string{"A": "${foo}"} },
		"addon type":  func(t *Template) { t.Addons = []Addon{{Key: "db", Type: "mysql"}} },
		"volume path": func(t *Template) { t.Volumes = []Volume{{Name: "data", Path: "data", SizeGB: 1}} },
	} {
		tpl := base
		mut(&tpl)
		if tpl.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}
