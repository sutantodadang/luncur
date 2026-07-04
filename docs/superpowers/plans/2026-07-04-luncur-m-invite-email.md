# luncur Plan M — Invite Email Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `POST /v1/invites` (and the UI invite form / `luncur invite create`) can optionally email the invite link over SMTP configured via install settings.

**Architecture:** New stdlib-only `internal/mail` package (Mailer interface + net/smtp STARTTLS impl). Five new `smtp_*` settings keys; `smtp_pass` joins `backup_s3_secret_key` as a sealed write-only key (generalized into a `sealedKeys` map + `sealedSetting` helper). The server gets an injectable `mailer func() (mail.Mailer, error)` field (default: build `mail.SMTP` from settings) so tests fake the whole send. Email failure never blocks invite creation: response gains `emailed:false` + `warning`.

**Tech Stack:** Go stdlib only (`net/smtp`, `crypto/tls`). No new Go module dependencies.

**Branch:** `plan-m` off `main`.

## Global Constraints (from Phase 4 spec)

- Single Go module, one binary from `cmd/luncur`. **No new Go module dependencies.**
- API error envelope unchanged (`writeError` with code + message).
- Secrets in settings use the existing sealed write-only pattern: sealed at rest (`sealed:` + hex), reads return `"(set)"`.
- Tests must not require a network or real SMTP: `Mailer` faked in server/UI tests; the SMTP impl itself is tested against a local plaintext fake SMTP listener (127.0.0.1, no TLS — `SMTP.Send` only calls StartTLS when the server advertises it).
- Conventional commits. Before **every** commit: `go build ./... && go vet ./... && go test ./...` — all green.
- Settings (all optional; `smtp_pass` sealed write-only): `smtp_host`, `smtp_port` (default 587), `smtp_user`, `smtp_pass`, `smtp_from`.

---

### Task 1: `internal/mail` package

**Files:**
- Create: `internal/mail/mail.go`
- Test: `internal/mail/mail_test.go`

**Interfaces:**
- Consumes: nothing (stdlib only).
- Produces: `type Mailer interface { Send(to, subject, body string) error }`; `var ErrUnconfigured = errors.New("smtp is not configured")`; `type SMTP struct { Host string; Port int; User, Pass, From string }` implementing `Mailer`; `func Message(from, to, subject, body string) []byte`. Task 3 imports all of these.

- [ ] **Step 1: Create branch**

```bash
git checkout -b plan-m
```

- [ ] **Step 2: Write the failing tests**

Create `internal/mail/mail_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/mail/`
Expected: FAIL — package does not exist / `Message` undefined.

- [ ] **Step 4: Write the implementation**

Create `internal/mail/mail.go`:

```go
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/mail/`
Expected: PASS (both tests).

- [ ] **Step 6: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/mail/
git commit -m "feat: internal/mail — stdlib SMTP mailer with STARTTLS"
```

---

### Task 2: SMTP settings keys (`smtp_pass` sealed)

**Files:**
- Modify: `internal/server/settings.go`
- Test: `internal/server/settings_test.go`

**Interfaces:**
- Consumes: existing `settableKeys`, `handleGetSetting`, `handleSetSetting`, `s3SecretKey` in `internal/server/settings.go`; `s.sealer` (`*secret.Sealer`).
- Produces: settings keys `smtp_host|smtp_port|smtp_user|smtp_pass|smtp_from` accepted by `PUT/GET /v1/settings/{key}`; `sealedKeys map[string]bool`; `func (s *server) sealedSetting(key string) (string, error)` (unseals any sealed key — Task 3 uses it for `smtp_pass`; `s3SecretKey` delegates to it).

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/settings_test.go`:

```go
// TestSettingsSMTPKeys: plain smtp_* keys round-trip; smtp_port must be a
// valid port number.
func TestSettingsSMTPKeys(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_host", admin, `{"value":"mail.example.com"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put smtp_host: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_port", admin, `{"value":"70000"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("put smtp_port 70000: want 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_port", admin, `{"value":"587"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put smtp_port 587: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv.URL+"/v1/settings/smtp_host", admin, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get smtp_host: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "mail.example.com" {
		t.Fatalf("smtp_host = %q, want mail.example.com", out.Value)
	}
}

// TestSettingsSMTPPassSealed mirrors TestSettingsBackupS3SecretKey for
// smtp_pass: 503 without a sealer; sealed at rest; GET masks to "(set)".
func TestSettingsSMTPPassSealed(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "PUT", srv.URL+"/v1/settings/smtp_pass", admin, `{"value":"hunter2"}`)
	if resp.StatusCode != 503 {
		t.Fatalf("put without sealer: want 503, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st2 := newTestStore(t)
	srv2 := newHTTPTest(t, Deps{Store: st2, Sealer: sealer})
	admin2 := seedUserToken(t, st2, "root@b.co", "admin")

	resp = doAuthed(t, "PUT", srv2.URL+"/v1/settings/smtp_pass", admin2, `{"value":"hunter2"}`)
	if resp.StatusCode != 204 {
		t.Fatalf("put with sealer: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doAuthed(t, "GET", srv2.URL+"/v1/settings/smtp_pass", admin2, "")
	if resp.StatusCode != 200 {
		t.Fatalf("get: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out.Value != "(set)" {
		t.Fatalf("get value = %q, want (set)", out.Value)
	}

	raw, err := st2.GetSetting("smtp_pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "sealed:") {
		t.Fatalf("raw setting = %q, want sealed: prefix", raw)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestSettingsSMTP' -v`
Expected: FAIL — `PUT /v1/settings/smtp_host` returns 400 "unknown setting".

- [ ] **Step 3: Implement**

In `internal/server/settings.go`:

1. Add to `settableKeys` (after the `registry_keep` entry):

```go
	"smtp_host": func(v string) bool { return v != "" },
	"smtp_port": func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n > 0 && n < 65536
	},
	"smtp_user": func(v string) bool { return v != "" },
	"smtp_pass": func(v string) bool { return v != "" },
	"smtp_from": func(v string) bool { return v != "" },
```

2. Add below `settableKeys`:

```go
// sealedKeys are write-only secrets: sealed at rest with the install
// sealer, and GET returns "(set)" instead of the value.
var sealedKeys = map[string]bool{
	"backup_s3_secret_key": true,
	"smtp_pass":            true,
}
```

3. In `handleGetSetting`, replace

```go
	if key == "backup_s3_secret_key" {
		v = "(set)"
	}
```

with

```go
	if sealedKeys[key] {
		v = "(set)"
	}
```

4. In `handleSetSetting`, replace `if key == "backup_s3_secret_key" {` with `if sealedKeys[key] {`.

5. Replace the whole `s3SecretKey` function with a generic helper plus a thin wrapper:

```go
// sealedSetting unseals a write-only sealed setting (see sealedKeys).
func (s *server) sealedSetting(key string) (string, error) {
	v, err := s.st.GetSetting(key)
	if err != nil {
		return "", err
	}
	raw, ok := strings.CutPrefix(v, "sealed:")
	if !ok {
		return "", fmt.Errorf("%s is not sealed", key)
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return "", err
	}
	if s.sealer == nil {
		return "", errSealerUnavailable
	}
	plain, err := s.sealer.Open(b)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// s3SecretKey unseals the write-only backup_s3_secret_key setting.
func (s *server) s3SecretKey() (string, error) {
	return s.sealedSetting("backup_s3_secret_key")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestSettings' -v`
Expected: PASS — all settings tests including the two new ones and the existing `TestSettingsBackupS3SecretKey` (the refactor must not break it).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/settings.go internal/server/settings_test.go
git commit -m "feat: smtp_* install settings, smtp_pass sealed write-only"
```

---

### Task 3: server mailer wiring + API email path

**Files:**
- Create: `internal/server/mail.go`
- Modify: `internal/server/server.go` (server struct + `newServer`)
- Modify: `internal/server/invites.go` (`handleCreateInvite`)
- Test: `internal/server/invites_test.go`

**Interfaces:**
- Consumes: `mail.Mailer`, `mail.SMTP`, `mail.ErrUnconfigured` (Task 1); `s.sealedSetting` (Task 2); `store.Invite{Token, Role, ExpiresAt}`; `s.st.GetSetting`, `store.ErrNotFound`.
- Produces: server field `mailer func() (mail.Mailer, error)` (tests override it); `func (s *server) emailInvite(r *http.Request, addr string, inv store.Invite) error` (Task 5 reuses it from the UI handler); `POST /v1/invites` accepts `"email"` and responds with `emailed` (+ `warning` on failure).

- [ ] **Step 1: Write the failing tests**

Append to `internal/server/invites_test.go` (add imports `net/http/httptest`, `strings`, `github.com/sutantodadang/luncur/internal/mail`, `github.com/sutantodadang/luncur/internal/store`):

```go
// fakeMailer records the one message it was asked to send.
type fakeMailer struct {
	to, subject, body string
	err               error
}

func (f *fakeMailer) Send(to, subject, body string) error {
	f.to, f.subject, f.body = to, subject, body
	return f.err
}

// mailerServer builds a test server whose mailer factory is overridden.
func mailerServer(t *testing.T, m mail.Mailer, merr error) (*httptest.Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	s := newServer(Deps{Store: st})
	s.mailer = func() (mail.Mailer, error) { return m, merr }
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func TestInviteEmailSent(t *testing.T) {
	fm := &fakeMailer{}
	srv, st := mailerServer(t, fm, nil)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"role":"member","email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != true {
		t.Fatalf("emailed = %v, want true", out["emailed"])
	}
	if _, ok := out["warning"]; ok {
		t.Fatalf("unexpected warning: %v", out["warning"])
	}
	if fm.to != "new@b.co" {
		t.Fatalf("mail to = %q, want new@b.co", fm.to)
	}
	if !strings.Contains(fm.body, "http://") || !strings.Contains(fm.body, "/ui/register?token="+out["token"].(string)) {
		t.Fatalf("mail body missing absolute register link:\n%s", fm.body)
	}
}

func TestInviteEmailSendFailure(t *testing.T) {
	fm := &fakeMailer{err: fmt.Errorf("connection refused")}
	srv, st := mailerServer(t, fm, nil)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("send failure must not block creation: want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != false {
		t.Fatalf("emailed = %v, want false", out["emailed"])
	}
	w, _ := out["warning"].(string)
	if !strings.Contains(w, "connection refused") {
		t.Fatalf("warning = %q, want send error in it", w)
	}

	// Invite still exists.
	invs, err := st.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("invites = %d, want 1", len(invs))
	}
}

func TestInviteEmailUnconfigured(t *testing.T) {
	// Real default mailer factory, no smtp_host setting -> ErrUnconfigured.
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"email":"new@b.co"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if out["emailed"] != false {
		t.Fatalf("emailed = %v, want false", out["emailed"])
	}
	w, _ := out["warning"].(string)
	if !strings.Contains(w, "smtp is not configured") {
		t.Fatalf("warning = %q, want smtp is not configured", w)
	}
}

func TestInviteNoEmailFieldNoEmailedKey(t *testing.T) {
	srv, st := testServer(t)
	admin := seedUserToken(t, st, "root@b.co", "admin")

	resp := doAuthed(t, "POST", srv.URL+"/v1/invites", admin, `{"role":"member"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := out["emailed"]; ok {
		t.Fatalf("emailed key present without email request: %v", out)
	}
}
```

(`fmt` is already imported by `invites_test.go`; `json` too. Add any missing imports.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestInviteEmail|TestInviteNoEmail' -v`
Expected: FAIL — compile error: `s.mailer` undefined.

- [ ] **Step 3: Implement**

1. Create `internal/server/mail.go`:

```go
package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/sutantodadang/luncur/internal/mail"
	"github.com/sutantodadang/luncur/internal/store"
)

// smtpMailer is the default mailer factory: it builds a mail.SMTP from
// the smtp_* install settings. mail.ErrUnconfigured when smtp_host is
// unset.
func (s *server) smtpMailer() (mail.Mailer, error) {
	host, err := s.st.GetSetting("smtp_host")
	if errors.Is(err, store.ErrNotFound) {
		return nil, mail.ErrUnconfigured
	}
	if err != nil {
		return nil, err
	}

	port := 587
	if v, err := s.st.GetSetting("smtp_port"); err == nil {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}

	user, _ := s.st.GetSetting("smtp_user")
	pass := ""
	if user != "" {
		if pass, err = s.sealedSetting("smtp_pass"); err != nil {
			return nil, err
		}
	}

	from, err := s.st.GetSetting("smtp_from")
	if errors.Is(err, store.ErrNotFound) {
		from = user
		if from == "" {
			from = "luncur@" + host
		}
	} else if err != nil {
		return nil, err
	}

	return mail.SMTP{Host: host, Port: port, User: user, Pass: pass, From: from}, nil
}

// requestBaseURL reconstructs the externally visible base URL of the
// request; luncur may sit behind a TLS-terminating proxy, so honor
// X-Forwarded-Proto.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// emailInvite sends the registration link for inv to addr. Shared by the
// API and UI invite-create handlers.
func (s *server) emailInvite(r *http.Request, addr string, inv store.Invite) error {
	m, err := s.mailer()
	if err != nil {
		return err
	}
	link := requestBaseURL(r) + "/ui/register?token=" + inv.Token
	body := "You have been invited to luncur (role " + inv.Role + ").\n\n" +
		"Register here: " + link + "\n\n" +
		"The link is single-use and expires " + inv.ExpiresAt + ".\n"
	return m.Send(addr, "You're invited to luncur", body)
}
```

2. In `internal/server/server.go`:
   - Add import `"github.com/sutantodadang/luncur/internal/mail"`.
   - Add a field to the `server` struct, after the `nowFn` field (it follows the same "injectable in tests" pattern):

```go
	// mailer builds the invite Mailer from settings; tests override it.
	mailer func() (mail.Mailer, error)
```

   - In `newServer`, after `s := &server{...}` is built (next to `s.execer` wiring), add:

```go
	s.mailer = s.smtpMailer
```

3. In `internal/server/invites.go`, replace `handleCreateInvite`:

```go
func (s *server) handleCreateInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Role  string `json:"role"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	inv, err := s.st.CreateInvite(req.Role, u.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	out := inviteJSON(inv)
	if req.Email != "" {
		if err := s.emailInvite(r, req.Email, inv); err != nil {
			log.Printf("invite email to %s: %v", req.Email, err)
			out["emailed"] = false
			out["warning"] = "email not sent: " + err.Error()
		} else {
			out["emailed"] = true
		}
	}
	writeJSON(w, http.StatusCreated, out)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestInvite' -v`
Expected: PASS — the four new tests plus the existing `TestInviteEndpointsAdminOnly`.

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/mail.go internal/server/server.go internal/server/invites.go internal/server/invites_test.go
git commit -m "feat: POST /v1/invites optionally emails the invite link"
```

---

### Task 4: client + CLI `--email`

**Files:**
- Modify: `internal/client/client.go` (`InviteInfo`, `CreateInvite`)
- Modify: `internal/cli/invite.go` (create subcommand)
- Test: `internal/cli/commands_test.go`

**Interfaces:**
- Consumes: API contract from Task 3 (`email` request field; `emailed` bool + `warning` string response fields).
- Produces: `func (c *Client) CreateInvite(role, email string) (InviteInfo, error)`; `InviteInfo` gains `Emailed bool` + `Warning string`; `luncur invite create --email addr`.

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/commands_test.go`:

```go
// TestInviteCreateEmailWarning: --email against a server without SMTP
// configured still creates the invite and prints the warning.
func TestInviteCreateEmailWarning(t *testing.T) {
	srv := testEnv(t)
	if out, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatalf("login: %v (%s)", err, out)
	}

	out, err := run(t, "invite", "create", "--role", "member", "--email", "new@b.co")
	if err != nil {
		t.Fatalf("invite create --email: %v (%s)", err, out)
	}
	if !strings.Contains(out, "/ui/register?token=") {
		t.Fatalf("missing invite link:\n%s", out)
	}
	if !strings.Contains(out, "warning:") || !strings.Contains(out, "smtp is not configured") {
		t.Fatalf("want unconfigured-SMTP warning, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestInviteCreateEmailWarning -v`
Expected: FAIL — `unknown flag: --email`.

- [ ] **Step 3: Implement**

1. In `internal/client/client.go`, extend `InviteInfo` and `CreateInvite`:

```go
// InviteInfo is one registration invite as returned by the API.
type InviteInfo struct {
	Token     string `json:"token"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
	Path      string `json:"path"`
	Used      bool   `json:"used"`
	Emailed   bool   `json:"emailed"`
	Warning   string `json:"warning"`
}

func (c *Client) CreateInvite(role, email string) (InviteInfo, error) {
	var out InviteInfo
	body := map[string]string{"role": role}
	if email != "" {
		body["email"] = email
	}
	err := c.do("POST", "/v1/invites", body, &out)
	return out, err
}
```

2. In `internal/cli/invite.go`, in the `create` subcommand: add an `email` flag variable next to `role`, pass it through, and report the outcome. The create block becomes:

```go
	var role, email string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a single-use invite link",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			inv, err := c.CreateInvite(role, email)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cmd.Printf("invite created (role %s, expires %s):\n%s%s\n",
				inv.Role, inv.ExpiresAt, cfg.Server, inv.Path)
			if email != "" {
				if inv.Emailed {
					cmd.Printf("emailed to %s\n", email)
				} else {
					cmd.Printf("warning: %s\n", inv.Warning)
				}
			}
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", "member", "role for the invited user (admin|member)")
	create.Flags().StringVar(&email, "email", "", "email the invite link to this address")
```

(Remove the old standalone `var role string` line — `role` now declares together with `email`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestInvite' -v`
Expected: PASS — new test and the existing invite flow test (which calls `create` without `--email` and must be unaffected).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/client/client.go internal/cli/invite.go internal/cli/commands_test.go
git commit -m "feat: luncur invite create --email"
```

---

### Task 5: UI email field + mail note

**Files:**
- Modify: `internal/server/ui.go` (`handleUIUsers`, `handleUIInviteCreate`)
- Modify: `internal/server/templates/users.html`
- Test: `internal/server/ui_test.go`

**Interfaces:**
- Consumes: `s.emailInvite` (Task 3), `fakeMailer` + pattern from `invites_test.go` (same package), UI test helpers `uiSessionCookie`, `uiCSRF`, `uiPost`, `noRedirectClient`.
- Produces: invite form accepts optional `email`; redirect to `/ui/users?mail=sent|failed`; users page renders the note.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/ui_test.go` (imports already cover `net/http/httptest`, `url`, `strings`; add `github.com/sutantodadang/luncur/internal/mail` if not present):

```go
// TestUIInviteEmailNote: the invite form with an email posts, sends via
// the mailer, and redirects to a page that shows the outcome note.
func TestUIInviteEmailNote(t *testing.T) {
	st := newTestStore(t)
	s := newServer(Deps{Store: st})
	fm := &fakeMailer{}
	s.mailer = func() (mail.Mailer, error) { return fm, nil }
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	admin, err := st.CreateUser("root@b.co", "password123", "admin")
	if err != nil {
		t.Fatal(err)
	}
	client := noRedirectClient()
	adminCk := uiSessionCookie(t, st, admin.ID)
	csrfCk := uiCSRF(t, client, srv.URL)

	resp := uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, adminCk,
		url.Values{"role": {"member"}, "email": {"new@b.co"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("invite post: want 303, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/ui/users?mail=sent" {
		t.Fatalf("redirect = %q, want /ui/users?mail=sent", loc)
	}
	if fm.to != "new@b.co" {
		t.Fatalf("mail to = %q, want new@b.co", fm.to)
	}

	// The redirected-to page renders the note.
	req, err := http.NewRequest("GET", srv.URL+loc, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(adminCk)
	page, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, err := io.ReadAll(page.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "invite emailed") {
		t.Fatalf("users page missing mail note:\n%s", body)
	}

	// Failure path: mailer errors -> ?mail=failed, invite still created.
	fm.err = fmt.Errorf("boom")
	resp = uiPost(t, client, srv.URL+"/ui/users/invite", csrfCk, adminCk,
		url.Values{"role": {"member"}, "email": {"x@b.co"}})
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/ui/users?mail=failed" {
		t.Fatalf("redirect = %q, want /ui/users?mail=failed", got)
	}
	invs, err := st.ListInvites()
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("invites = %d, want 2", len(invs))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestUIInviteEmailNote -v`
Expected: FAIL — redirect is `/ui/users` (no `?mail=sent`), note missing.

- [ ] **Step 3: Implement**

1. In `internal/server/ui.go`, `handleUIUsers`: derive the note from the query and pass it to the template — replace the `s.renderPage(...)` call with:

```go
	var mailNote string
	switch r.URL.Query().Get("mail") {
	case "sent":
		mailNote = "invite emailed"
	case "failed":
		mailNote = "invite created, but the email failed — copy the link below"
	}
	s.renderPage(w, "users.html", map[string]any{
		"User": u, "Users": users, "Invites": rows, "Self": u.ID,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"MailNote": mailNote,
	})
```

2. In `handleUIInviteCreate`: capture the invite, send when an email was given, encode the outcome in the redirect. Replace the tail of the handler (from the `CreateInvite` call down):

```go
	inv, err := s.st.CreateInvite(role, u.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dest := "/ui/users"
	if email := strings.TrimSpace(r.PostFormValue("email")); email != "" {
		if err := s.emailInvite(r, email, inv); err != nil {
			log.Printf("ui invite email to %s: %v", email, err)
			dest += "?mail=failed"
		} else {
			dest += "?mail=sent"
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
```

3. In `internal/server/templates/users.html`: render the note above the invite form and add the email input. Replace the invite-create form block with:

```html
{{if .MailNote}}<p><em>{{.MailNote}}</em></p>{{end}}
<form method="post" action="/ui/users/invite">
  <input type="hidden" name="_csrf" value="{{.CSRF}}">
  <label>role <select name="role"><option>member</option><option>admin</option></select></label>
  <label>email <input name="email" size="28" placeholder="optional — email the link"></label>
  <button type="submit">create invite</button>
</form>
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/server/ -run 'TestUIInviteEmailNote|TestUIUsersPageAdminOnly' -v`
Expected: PASS — new test, and the existing users-page test (invite create without email must keep redirecting to plain `/ui/users`).

- [ ] **Step 5: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add internal/server/ui.go internal/server/templates/users.html internal/server/ui_test.go
git commit -m "feat: invite email field + outcome note in the users UI"
```

---

### Task 6: docs

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README**

1. Line ~57: `luncur invite create [--role admin|member]` → add the flag and note:

```
luncur invite create [--role admin|member] [--email addr]  # prints a one-time /ui/register link; --email sends it via SMTP
```

2. After the backup S3 settings block (~line 305), add an SMTP settings block:

```sh
luncur config set smtp_host mail.example.com   # unset = invite emails disabled
luncur config set smtp_port 587                # optional, default 587
luncur config set smtp_user luncur@example.com # optional; enables PLAIN auth
luncur config set smtp_pass ...                # write-only: reads show "(set)"
luncur config set smtp_from luncur@example.com # optional, defaults to smtp_user
```

with a sentence: STARTTLS is used when the server offers it; a send failure (or unconfigured SMTP) never blocks invite creation — the API returns `emailed:false` plus a warning and the link can be copied as before.

3. Line ~418: remove the "No email delivery for invites" limitation bullet (or reword to reflect it now exists).

4. Users-page paragraph (~line 177-180): mention the optional email field on the invite form.

- [ ] **Step 2: Full verify + commit**

```bash
go build ./... && go vet ./... && go test ./...
git add README.md
git commit -m "docs: invite email settings and CLI flag"
```

---

## Manual verification (owner's VPS, after merge)

Per the Phase 4 test strategy: configure real SMTP (`smtp_host/port/user/pass/from`), run `luncur invite create --email <real addr>`, confirm delivery and that the emailed link registers a user. Also create an invite from the UI with an email and confirm the "invite emailed" note.

## Self-review notes

- Spec coverage: settings keys (Task 2), `internal/mail` + sentinel (Task 1), API `email` + `emailed` + warning (Task 3), CLI `--email` (Task 4), UI field + flash note (Task 5), docs (Task 6). Error-handling table row "unconfigured or send failure → invite still created, emailed:false + warning" covered by `TestInviteEmailSendFailure` / `TestInviteEmailUnconfigured` / UI failure path.
- `emailed` appears in the response only when `email` was requested (`TestInviteNoEmailFieldNoEmailedKey`); `InviteInfo.Emailed/Warning` zero-values cover the absent case for list responses.
- Type consistency: `mailer func() (mail.Mailer, error)` is the single seam used by API tests (Task 3) and UI tests (Task 5); `emailInvite(r, addr, inv)` shared by both handlers.
