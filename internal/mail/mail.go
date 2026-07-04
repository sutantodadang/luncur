// Package mail sends plain-text email over SMTP with STARTTLS (when the
// server offers it). luncur's SMTP settings live in the settings store;
// the server builds an SMTP mailer from them per send.
package mail

import (
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"strconv"
	"strings"
)

// Mailer sends one plain-text message.
type Mailer interface {
	Send(to, subject, body string) error
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

func (m SMTP) Send(to, subject, body string) error {
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
	if _, err := w.Write(Message(m.From, to, subject, body)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Message renders a minimal RFC 5322 plain-text message with CRLF line
// endings. No Date header — the submission server stamps one.
func Message(from, to, subject, body string) []byte {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	return []byte("From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		body)
}
