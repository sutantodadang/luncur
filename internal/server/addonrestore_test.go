package server

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

// seedRestoreAddon creates a project (if not already present) and an addon
// of the given type inside it, sealing throwaway credentials the same way
// production provisioning does. restoreAddon itself never reads them (the
// pod's own env holds the real secrets) but CreateAddon requires non-nil
// CredsEnc.
func seedRestoreAddon(t *testing.T, srv *server, st *store.Store, project, typ, name string) store.Addon {
	t.Helper()
	p, err := st.GetProject(project)
	if errors.Is(err, store.ErrNotFound) {
		p, err = st.CreateProject(project)
	}
	if err != nil {
		t.Fatal(err)
	}
	creds, err := newAddonCreds(typ)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := srv.sealCreds(creds)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateAddon(p.ID, typ, name, "1", 1, sealed)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRestoreAddonPostgres(t *testing.T) {
	exec := &fakeExecer{}
	srv, st, _ := backupServer(t, exec)
	a := seedRestoreAddon(t, srv, st, "proj", "postgres", "db1")

	if err := srv.restoreAddon(context.Background(), a, []byte("PGDUMPDATA")); err != nil {
		t.Fatal(err)
	}
	if string(exec.stdin) != "PGDUMPDATA" {
		t.Fatalf("stdin = %q, want the dump bytes", exec.stdin)
	}
	joined := strings.Join(exec.cmd, " ")
	if !strings.Contains(joined, "pg_restore") {
		t.Fatalf("cmd = %q, want pg_restore", joined)
	}
}

func TestRestoreAddonRedis(t *testing.T) {
	exec := &fakeExecer{}
	srv, st, _ := backupServer(t, exec)
	a := seedRestoreAddon(t, srv, st, "proj", "redis", "cache1")

	if err := srv.restoreAddon(context.Background(), a, []byte("RDBBYTES")); err != nil {
		t.Fatal(err)
	}
	if string(exec.stdin) != "RDBBYTES" {
		t.Fatalf("stdin = %q, want the dump bytes", exec.stdin)
	}
	joined := strings.Join(exec.cmd, " ")
	if !strings.Contains(joined, "cat > /data/dump.rdb") || !strings.Contains(joined, "DEBUG RELOAD") {
		t.Fatalf("cmd = %q, want cat+DEBUG RELOAD", joined)
	}
}

func TestRestoreAddonUnsupportedType(t *testing.T) {
	exec := &fakeExecer{}
	srv, st, _ := backupServer(t, exec)
	a := seedRestoreAddon(t, srv, st, "proj", "minio", "store1")

	err := srv.restoreAddon(context.Background(), a, []byte("X"))
	if !errors.Is(err, errRestoreUnsupported) {
		t.Fatalf("err = %v, want errRestoreUnsupported", err)
	}
}

// restoreTestServer builds an HTTP-backed server whose kube client is the
// fake-dynamic one from addonTestServer (so requireKube passes and addon
// creation over HTTP works), but with execer swapped to exec so restore's
// pods/exec calls are captured instead of hitting a real cluster.
func restoreTestServer(t *testing.T, exec *fakeExecer) (*server, *httptest.Server, *store.Store) {
	t.Helper()
	s, srv, st, _ := addonTestServer(t)
	if exec != nil {
		s.execer = exec
	}
	return s, srv, st
}

func doAuthedMultipart(t *testing.T, url, token string, data []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("dump", "dump")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandleRestoreAddon(t *testing.T) {
	exec := &fakeExecer{}
	s, srv, st := restoreTestServer(t, exec)
	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", srv.URL+"/v1/projects", admin, `{"name":"proj"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin,
		`{"type":"postgres","name":"pg1"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin,
		`{"type":"redis","name":"rd1"}`).Body.Close()
	doAuthed(t, "POST", srv.URL+"/v1/projects/proj/addons", admin,
		`{"type":"minio","name":"mn1"}`).Body.Close()

	// postgres: 200, dump piped on stdin, pg_restore invoked.
	resp := doAuthedMultipart(t, srv.URL+"/v1/projects/proj/addons/pg1/restore", admin, []byte("PGDUMPDATA"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("postgres restore: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if string(exec.stdin) != "PGDUMPDATA" {
		t.Fatalf("stdin = %q", exec.stdin)
	}
	if !strings.Contains(strings.Join(exec.cmd, " "), "pg_restore") {
		t.Fatalf("cmd = %v, want pg_restore", exec.cmd)
	}

	// redis: 200, dump piped on stdin, cat+DEBUG RELOAD invoked.
	resp = doAuthedMultipart(t, srv.URL+"/v1/projects/proj/addons/rd1/restore", admin, []byte("RDBBYTES"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redis restore: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if string(exec.stdin) != "RDBBYTES" {
		t.Fatalf("stdin = %q", exec.stdin)
	}
	joined := strings.Join(exec.cmd, " ")
	if !strings.Contains(joined, "cat > /data/dump.rdb") || !strings.Contains(joined, "DEBUG RELOAD") {
		t.Fatalf("cmd = %v, want cat+DEBUG RELOAD", exec.cmd)
	}

	// minio: no logical dump -> 400 unsupported.
	resp = doAuthedMultipart(t, srv.URL+"/v1/projects/proj/addons/mn1/restore", admin, []byte("X"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("minio restore: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// unknown addon -> 404.
	resp = doAuthedMultipart(t, srv.URL+"/v1/projects/proj/addons/nope/restore", admin, []byte("X"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown addon: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// empty dump -> 400.
	resp = doAuthedMultipart(t, srv.URL+"/v1/projects/proj/addons/pg1/restore", admin, []byte(""))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty dump: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	_ = s
}
