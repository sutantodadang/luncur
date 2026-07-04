package mail

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMessage(t *testing.T) {
	got := string(Message("a@x.co", "b@y.co", "hi", "line1\nline2\n"))
	want := "From: a@x.co\r\n" +
		"To: b@y.co\r\n" +
		"Subject: hi\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"line1\r\nline2\r\n"
	if got != want {
		t.Fatalf("Message:\ngot  %q\nwant %q", got, want)
	}
}

// fakeSMTP speaks just enough plaintext SMTP (no TLS, no AUTH) to accept
// one message. It sends everything received in DATA on msgc when the
// client QUITs.
func fakeSMTP(t *testing.T) (addr string, msgc chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	msgc = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		say := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }
		say("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if strings.TrimRight(line, "\r\n") == "." {
					inData = false
					say("250 ok")
					continue
				}
				data.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimRight(line, "\r\n"))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				fmt.Fprintf(conn, "250-fake\r\n250 SIZE 35882577\r\n")
			case strings.HasPrefix(cmd, "DATA"):
				say("354 go")
				inData = true
			case strings.HasPrefix(cmd, "QUIT"):
				say("221 bye")
				msgc <- data.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), msgc
}

func TestSMTPSend(t *testing.T) {
	addr, msgc := fakeSMTP(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	m := SMTP{Host: host, Port: port, From: "luncur@x.co"}
	if err := m.Send("new@y.co", "invite", "hello\n"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case msg := <-msgc:
		if !strings.Contains(msg, "To: new@y.co") || !strings.Contains(msg, "Subject: invite") {
			t.Fatalf("message missing headers:\n%s", msg)
		}
		if !strings.Contains(msg, "hello") {
			t.Fatalf("message missing body:\n%s", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake SMTP server never received QUIT")
	}
}
