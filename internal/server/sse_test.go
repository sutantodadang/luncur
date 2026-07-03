package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/build"
)

// TestDeployLogsFollow checks the ?follow=1 SSE path: a deployment already
// in a terminal state ("failed") with a written log file should replay the
// file as data events then immediately emit the end event with the status.
func TestDeployLogsFollow(t *testing.T) {
	st := newTestStore(t)
	dataDir := t.TempDir()
	handler := New(Deps{Store: st, ExternalIP: "1.2.3.4", DataDir: dataDir})

	admin := seedUserToken(t, st, "root@b.co", "admin")

	p, err := st.CreateProject("web")
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.CreateApp(p.ID, "api", 8080)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(a.ID, "failed", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	src, err := build.NewSource(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src.LogPath(d.ID), []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("/v1/projects/web/apps/api/deploys/%d/logs?follow=1", d.ID)
	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"data: line one\n",
		"data: line two\n",
		"event: end\n",
		"data: failed\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}
