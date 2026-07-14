// Package mail sends email over SMTP with STARTTLS (when the server offers
// it). luncur's SMTP settings live in the settings store; the server builds
// an SMTP mailer from them per send.
package mail

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
)

// Mailer sends one message. html == "" sends text/plain; otherwise it sends
// multipart/alternative with both the text and HTML parts.
type Mailer interface {
	Send(to, subject, text, html string) error
}

// ErrUnconfigured is the sentinel for "smtp_host is not set" — callers
// treat it as "email skipped", not a transport failure.
var ErrUnconfigured = errors.New("smtp is not configured")

// SMTP is a Mailer over net/smtp. It upgrades to TLS via STARTTLS when
// the server advertises it and authenticates with PLAIN when User is set.
type SMTP struct {
	Host string
	Port int
	User string // empty = no auth
	Pass string
	From string
}

func (m SMTP) Send(to, subject, text, html string) error {
	c, err := smtp.Dial(net.JoinHostPort(m.Host, strconv.Itoa(m.Port)))
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: m.Host}); err != nil {
			return err
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Pass, m.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(Message(m.From, to, subject, text, html)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// mixedBoundary is the fixed MIME boundary used for multipart/alternative
// messages. luncur has no rand-per-call plumbing here; a fixed boundary is
// fine since the text/html parts we generate never contain it, and it keeps
// Message deterministic and testable.
const mixedBoundary = "luncur-boundary"

// Message renders an RFC 5322 message with CRLF line endings. No Date
// header — the submission server stamps one.
//
// html == "" renders a text/plain body: byte-identical to luncur's original
// plain-text-only format. html != "" renders multipart/alternative with two
// parts (text/plain then text/html), each base64-encoded.
func Message(from, to, subject, text, html string) []byte {
	text = normalizeCRLF(text)
	header := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n"

	if html == "" {
		return []byte(header +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			text)
	}

	html = normalizeCRLF(html)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.SetBoundary(mixedBoundary) // fixed format, always valid — error impossible
	writeBase64Part(mw, "text/plain; charset=utf-8", text)
	writeBase64Part(mw, "text/html; charset=utf-8", html)
	_ = mw.Close() // in-memory buffer, never fails

	return []byte(header +
		`Content-Type: multipart/alternative; boundary="` + mixedBoundary + `"` + "\r\n" +
		"\r\n" +
		body.String())
}

// writeBase64Part appends one base64-encoded MIME part to mw. Errors are
// ignored: mw writes to an in-memory bytes.Buffer, which never fails.
func writeBase64Part(mw *multipart.Writer, contentType, content string) {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "base64")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return
	}
	_, _ = pw.Write([]byte(wrapBase64(content)))
}

// wrapBase64 base64-encodes s and wraps it at 76 columns with CRLF line
// endings, per RFC 2045.
func wrapBase64(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for i := 0; i < len(enc); i += 76 {
		end := min(i+76, len(enc))
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
}

// normalizeCRLF forces \r\n line endings, tolerating input that already
// uses \r\n or plain \n.
func normalizeCRLF(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}
