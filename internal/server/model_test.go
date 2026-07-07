package server

import (
	"encoding/json"
	"testing"
)

func TestModelAppAPI(t *testing.T) {
	srv, st, actions := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()

	// Bad runtime combination fails before the row exists.
	resp := doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin,
		`{"name":"m1","kind":"model","model_source":"hf:org/name"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("non-gguf cpu: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GGUF on CPU auto-deploys with llama.cpp.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin,
		`{"name":"gemma","kind":"model","model_source":"hf:unsloth/gemma-3n-E4B-it-GGUF/gemma-3n-E4B-it-Q4_K_M.gguf"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: want 201, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	var app map[string]any
	json.NewDecoder(resp.Body).Decode(&app)
	resp.Body.Close()
	if app["status"] != "deploying" || app["model_source"] == "" {
		t.Fatalf("create response: %+v", app)
	}

	// Built-in runtime: deploying an explicit image is a user error.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/gemma/deploy", admin, `{"image":"nginx:1"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("builtin+image: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty deploy re-applies the runtime image.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/gemma/deploy", admin, `{}`)
	if resp.StatusCode != 200 {
		t.Fatalf("redeploy: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	// Custom runtime requires an image at deploy.
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps", admin,
		`{"name":"custom1","kind":"model","model_source":"s3:m/w.bin","runtime":"custom"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("custom create: want 201, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/custom1/deploy", admin, `{}`)
	if resp.StatusCode != 400 {
		t.Fatalf("custom no image: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doAuthed(t, "POST", srv.URL+"/v1/projects/ml/apps/custom1/deploy", admin, `{"image":"me/serve:1"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("custom deploy: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	_ = actions
}
