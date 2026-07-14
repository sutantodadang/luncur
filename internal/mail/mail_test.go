package mail

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMessage(t *testing.T) {
	got := string(Message("a@x.co", "b@y.co", "hi", "line1\nline2\n", ""))
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

// TestMessageMultipart: html != "" renders multipart/alternative with both
// parts base64-encoded, and each part decodes back to the original text.
func TestMessageMultipart(t *testing.T) {
	text := "hi there\nplain body\n"
	html := "<p>hi there</p><p>html body</p>"
	got := string(Message("a@x.co", "b@y.co", "hi", text, html))

	if !strings.Contains(got, "Content-Type: multipart/alternative;") {
		t.Fatalf("message missing multipart/alternative header:\n%s", got)
	}

	headerEnd := strings.Index(got, "\r\n\r\n")
	if headerEnd < 0 {
		t.Fatalf("message missing header/body separator:\n%s", got)
	}
	headers := got[:headerEnd]
	var contentType string
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Content-Type:") {
			contentType = strings.TrimSpace(strings.TrimPrefix(line, "Content-Type:"))
		}
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content-type %q: %v", contentType, err)
	}

	mr := multipart.NewReader(strings.NewReader(got[headerEnd+4:]), params["boundary"])
	var gotText, gotHTML string
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		b := make([]byte, 0, 256)
		buf := make([]byte, 256)
		for {
			n, rerr := part.Read(buf)
			b = append(b, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(b), "\r\n", ""))
		if err != nil {
			t.Fatalf("decode part: %v", err)
		}
		switch part.Header.Get("Content-Type") {
		case "text/plain; charset=utf-8":
			gotText = string(dec)
		case "text/html; charset=utf-8":
			gotHTML = string(dec)
		}
	}
	if gotText != normalizeCRLF(text) {
		t.Fatalf("text part = %q, want %q", gotText, normalizeCRLF(text))
	}
	if gotHTML != normalizeCRLF(html) {
		t.Fatalf("html part = %q, want %q", gotHTML, normalizeCRLF(html))
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
	if err := m.Send("new@y.co", "invite", "hello\n", ""); err != nil {
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
