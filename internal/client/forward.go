package client

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// bufConn is a net.Conn whose reads drain the handshake's buffered reader
// first — bytes the server sent right after the 101 must not be lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Forward opens one raw TCP tunnel to the app's in-cluster service via the
// server's luncur-tunnel upgrade endpoint. One call per local connection.
func (c *Client) Forward(project, app string, port int) (net.Conn, error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	addr := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	var conn net.Conn
	if u.Scheme == "https" {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, nil)
	} else {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/v1/projects/%s/apps/%s/forward?port=%d",
		url.PathEscape(project), url.PathEscape(app), port)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: luncur-tunnel/1\r\n\r\n",
		path, u.Hostname(), c.token)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
			return nil, fmt.Errorf("%s (%s)", env.Error.Message, env.Error.Code)
		}
		return nil, fmt.Errorf("forward failed: %s", resp.Status)
	}
	return &bufConn{Conn: conn, r: br}, nil
}
