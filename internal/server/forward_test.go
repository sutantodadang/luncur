package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sutantodadang/luncur/internal/store"
)

// newForwardTestServer builds a *server (so tests can override fwdDial) and
// serves it through httptest.NewServer(srv.handler()) — going through the
// same handler chain (including auditMiddleware) as production, proving
// hijack passthrough works via statusRecorder.Unwrap().
func newForwardTestServer(t *testing.T) (*httptest.Server, *store.Store, *server) {
	t.Helper()
	st := newTestStore(t)
	srv := newServer(Deps{Store: st})
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts, st, srv
}

// dialForward performs the client half of the luncur-tunnel upgrade and
// returns the raw conn + buffered reader on 101.
func dialForward(t *testing.T, baseURL, token, project, app string, port int) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	addr := strings.TrimPrefix(baseURL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/v1/projects/%s/apps/%s/forward?port=%d", project, app, port)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: luncur-tunnel/1\r\n\r\n", path, addr, token)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, br, resp
}

func TestForwardTunnelEcho(t *testing.T) {
	// echo listener stands in for the in-cluster service
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); buf := make([]byte, 64); n, _ := c.Read(buf); c.Write(buf[:n]) }(c)
		}
	}()

	ts, st, srv := newForwardTestServer(t)
	srv.fwdDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api.luncur-web:3000" {
			t.Errorf("dialed %s", addr)
		}
		return net.Dial("tcp", ln.Addr().String())
	}

	admin := seedUserToken(t, st, "root@b.co", "admin")
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()

	conn, br, resp := dialForward(t, ts.URL, admin, "web", "api", 3000)
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	fmt.Fprint(conn, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo got %q err %v", buf, err)
	}
}

func TestForwardTunnelErrors(t *testing.T) {
	ts, st, srv := newForwardTestServer(t)
	srv.fwdDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("connection refused")
	}

	admin := seedUserToken(t, st, "root@b.co", "admin")
	nonMember := seedUserToken(t, st, "outsider@b.co", "member")
	doAuthed(t, "POST", ts.URL+"/v1/projects", admin, `{"name":"web"}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/web/apps", admin, `{"name":"api","port":3000}`).Body.Close()
	doAuthed(t, "POST", ts.URL+"/v1/projects/web/apps", admin, `{"name":"wrk","kind":"worker"}`).Body.Close()

	// non-member token -> 403 (requireProject's forbidden)
	resp := doAuthed(t, "GET", ts.URL+"/v1/projects/web/apps/api/forward?port=3000", nonMember, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-member: want 403, got %d", resp.StatusCode)
	}

	// worker app -> 409 no_service
	resp = doAuthed(t, "GET", ts.URL+"/v1/projects/web/apps/wrk/forward", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("worker app: want 409, got %d", resp.StatusCode)
	}

	// port=9999 -> 400 bad_port
	resp = doAuthed(t, "GET", ts.URL+"/v1/projects/web/apps/api/forward?port=9999", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad port: want 400, got %d", resp.StatusCode)
	}

	// missing Upgrade header -> 400 bad_upgrade
	resp = doAuthed(t, "GET", ts.URL+"/v1/projects/web/apps/api/forward?port=3000", admin, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing upgrade: want 400, got %d", resp.StatusCode)
	}

	// fwdDial returning error -> 502 dial_failed
	conn, _, dialResp := dialForward(t, ts.URL, admin, "web", "api", 3000)
	defer conn.Close()
	if dialResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("dial failed: want 502, got %d", dialResp.StatusCode)
	}
}
