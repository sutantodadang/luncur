package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/secret"
	"github.com/sutantodadang/luncur/internal/store"
)

func TestProjectS3API(t *testing.T) {
	srv, st, _ := kubeServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"ml"}`).Body.Close()

	// Unset -> 404.
	resp := doAuthed(t, "GET", srv.URL+"/v1/projects/ml/s3", admin, "")
	if resp.StatusCode != 404 {
		t.Fatalf("unset get: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing fields -> 400.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/s3", admin, `{"endpoint":"https://s3.example.com"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("partial set: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Set.
	resp = doAuthed(t, "PUT", srv.URL+"/v1/projects/ml/s3", admin,
		`{"endpoint":"https://s3.example.com","region":"eu-1","bucket":"models","access_key":"AK","secret_key":"SK"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("set: want 200, got %d (%s)", resp.StatusCode, mustReadBody(t, resp))
	}
	resp.Body.Close()

	// Get echoes non-secret fields + access key, never the secret.
	resp = doAuthed(t, "GET", srv.URL+"/v1/projects/ml/s3", admin, "")
	body := mustReadBody(t, resp)
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["endpoint"] != "https://s3.example.com" || got["bucket"] != "models" || got["access_key"] != "AK" {
		t.Fatalf("get: %s", body)
	}
	if strings.Contains(string(body), "SK") {
		t.Fatalf("secret leaked: %s", body)
	}

	// Delete.
	resp = doAuthed(t, "DELETE", srv.URL+"/v1/projects/ml/s3", admin, "")
	if resp.StatusCode != 204 {
		t.Fatalf("delete: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestRenderS3EnvInjection drives renderApp directly: an opted-in app's env
// Secret must carry LUNCUR_S3_* from the project config, and user env wins.
func TestRenderS3EnvInjection(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})

	p, _ := st.CreateProject("ml")
	p, env := seedDefaultEnv(t, st, p)
	a, err := st.CreateApp(p.ID, "train", 0, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppEnvironmentID(a.ID, env.ID); err != nil {
		t.Fatal(err)
	}
	ak, _ := sealer.Seal([]byte("AK"))
	sk, _ := sealer.Seal([]byte("SK"))
	if err := st.SetProjectS3(store.ProjectS3{
		ProjectID: p.ID, Endpoint: "https://s3.example.com", Bucket: "models",
		AccessKeyEnc: ak, SecretKeyEnc: sk,
	}); err != nil {
		t.Fatal(err)
	}

	// Not opted in: no injection.
	r, err := s.renderApp(p, env, a, "img:1", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(renderedJSON(r), "LUNCUR_S3_ENDPOINT") {
		t.Fatal("injection must be opt-in")
	}

	// Opted in: injected.
	if err := st.SetInjectS3(a.ID, true); err != nil {
		t.Fatal(err)
	}
	a.InjectS3 = true
	r, err = s.renderApp(p, env, a, "img:1", false)
	if err != nil {
		t.Fatal(err)
	}
	out := renderedJSON(r)
	for _, want := range []string{
		`"LUNCUR_S3_ENDPOINT":"https://s3.example.com"`,
		`"LUNCUR_S3_KEY":"AK"`,
		`"LUNCUR_S3_SECRET":"SK"`,
		`"LUNCUR_S3_BUCKET":"models"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}

	// User env wins per key.
	sealed, _ := sealer.Seal([]byte("mine"))
	if err := st.SetEnv(a.ID, "LUNCUR_S3_BUCKET", sealed); err != nil {
		t.Fatal(err)
	}
	r, err = s.renderApp(p, env, a, "img:1", false)
	if err != nil {
		t.Fatal(err)
	}
	out = renderedJSON(r)
	if !strings.Contains(out, `"LUNCUR_S3_BUCKET":"mine"`) {
		t.Fatalf("user env must win:\n%s", out)
	}
}

// renderedJSON flattens every rendered object's JSON for substring asserts.
func renderedJSON(r render.Rendered) string {
	var b strings.Builder
	for _, o := range r.Objects {
		b.Write(o.JSON)
		b.WriteByte('\n')
	}
	return b.String()
}
