package client

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForwardUpgradeEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/web/apps/api/forward" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("port") != "3000" {
			t.Errorf("port %s", r.URL.Query().Get("port"))
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: luncur-tunnel/1\r\nConnection: Upgrade\r\n\r\n")
		buf.Flush()
		b := make([]byte, 4)
		io.ReadFull(buf, b)
		conn.Write(b)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	conn, err := c.Forward("web", "api", 3000)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(bufio.NewReader(conn), got); err != nil || string(got) != "ping" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestForwardErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":{"code":"no_service","message":"only web apps have a Service to forward to"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if _, err := c.Forward("web", "wrk", 3000); err == nil || !strings.Contains(err.Error(), "no_service") {
		t.Fatalf("want no_service error, got %v", err)
	}
}
