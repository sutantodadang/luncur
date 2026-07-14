package server

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sutantodadang/luncur/internal/mail"
	"github.com/sutantodadang/luncur/internal/secret"
)

// mailMsg is one message recorded by recordingMailer.
type mailMsg struct{ to, subject, text, html string }

// recordingMailer is a mail.Mailer test double that records every Send and
// also delivers each one on a channel, so tests can wait for N async sends
// (sendNotifyEmail always fires from notify()'s goroutine).
type recordingMailer struct {
	mu   sync.Mutex
	sent []mailMsg
	ch   chan mailMsg
}

func newRecordingMailer(buf int) *recordingMailer {
	return &recordingMailer{ch: make(chan mailMsg, buf)}
}

func (m *recordingMailer) Send(to, subject, text, html string) error {
	msg := mailMsg{to, subject, text, html}
	m.mu.Lock()
	m.sent = append(m.sent, msg)
	m.mu.Unlock()
	m.ch <- msg
	return nil
}

func recvMail(t *testing.T, ch chan mailMsg, timeout time.Duration) mailMsg {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(timeout):
		t.Fatal("timed out waiting for notification email")
		return mailMsg{}
	}
}

func TestValidNotifyEvents(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false},
		{"deploy_failed", true},
		{"deploy_failed,cert_failed", true},
		{" deploy_failed , cert_failed ", true},
		{"deploy_failed,bogus", false},
		{"bogus", false},
		{",", false},
	}
	for _, c := range cases {
		if got := validNotifyEvents(c.v); got != c.want {
			t.Errorf("validNotifyEvents(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestParseNotifyEvents(t *testing.T) {
	got := parseNotifyEvents(" deploy_success ,cert_failed")
	if !got["deploy_success"] || !got["cert_failed"] || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestBuildNotifyPayloadGeneric(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	b, err := buildNotifyPayload("generic", "", notifyEvent{
		Event: "deploy_success", Project: "web", App: "api", DeployID: "7", URL: "http://api.example.com",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"event": "deploy_success", "project": "web", "app": "api",
		"deploy_id": "7", "status": "live", "url": "http://api.example.com",
		"time": "2026-07-04T12:00:00Z",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("field %s = %v, want %v (full: %v)", k, out[k], v, out)
		}
	}
	if _, ok := out["error"]; ok {
		t.Errorf("error field should be omitted, got %v", out)
	}

	// deploy_id omitted when 0 (cert events); error present when non-empty.
	b, err = buildNotifyPayload("generic", "", notifyEvent{
		Event: "cert_failed", Project: "web", App: "api", URL: "host.example.com", Err: "boom",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["deploy_id"]; ok {
		t.Errorf("deploy_id should be omitted for cert events, got %v", out)
	}
	if out["status"] != "failed" || out["error"] != "boom" {
		t.Errorf("got %v", out)
	}
}

func TestBuildNotifyPayloadDiscordSlackTelegram(t *testing.T) {
	now := time.Now()
	ev := notifyEvent{Event: "deploy_success", Project: "web", App: "api", DeployID: "10", Seq: 1, URL: "http://x"}

	b, err := buildNotifyPayload("discord", "", ev, now)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Content != "✅ web/api deploy #1 live — http://x" {
		t.Fatalf("discord content = %q", d.Content)
	}

	b, err = buildNotifyPayload("slack", "", ev, now)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Text != d.Content {
		t.Fatalf("slack text = %q", s.Text)
	}

	b, err = buildNotifyPayload("telegram", "123456", ev, now)
	if err != nil {
		t.Fatal(err)
	}
	var tg struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(b, &tg); err != nil {
		t.Fatal(err)
	}
	if tg.ChatID != "123456" || tg.Text != d.Content {
		t.Fatalf("telegram = %+v", tg)
	}
}

func TestBuildNotifyPayloadMessages(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ev   notifyEvent
		want string
	}{
		{notifyEvent{Event: "deploy_success", Project: "p", App: "a", DeployID: "30", Seq: 3, URL: "http://u"}, "✅ p/a deploy #3 live — http://u"},
		{notifyEvent{Event: "deploy_failed", Project: "p", App: "a", DeployID: "30", Seq: 3, Err: "oops"}, "❌ p/a deploy #3 failed: oops"},
		{notifyEvent{Event: "cert_issued", URL: "host"}, "🔒 host cert issued"},
		{notifyEvent{Event: "cert_failed", URL: "host", Err: "oops"}, "⚠️ host cert failed: oops"},
	}
	for _, c := range cases {
		b, err := buildNotifyPayload("discord", "", c.ev, now)
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Content string `json:"content"`
		}
		json.Unmarshal(b, &d)
		if d.Content != c.want {
			t.Errorf("event %s: got %q want %q", c.ev.Event, d.Content, c.want)
		}
	}
}

func TestBuildNotifyPayloadUnknownFormat(t *testing.T) {
	if _, err := buildNotifyPayload("bogus", "", notifyEvent{Event: "deploy_success"}, time.Now()); err == nil {
		t.Fatal("want error for unknown format")
	}
}

func captureHandler(ch chan []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- b
		w.WriteHeader(http.StatusOK)
	}
}

func recvNotify(t *testing.T, ch chan []byte, timeout time.Duration) []byte {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(timeout):
		t.Fatal("timed out waiting for notification POST")
		return nil
	}
}

func setSealedNotifyURL(t *testing.T, s *server, url string) {
	t.Helper()
	sealed, err := s.sealer.Seal([]byte(url))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetSetting("notify_url", "sealed:"+hex.EncodeToString(sealed)); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyDefaultEventsFiltering(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)

	// default events = deploy_failed,cert_failed,app_unhealthy,backup_failed
	// -> deploy_success dropped.
	s.notify(notifyEvent{Event: "deploy_success", Project: "p", App: "a", DeployID: "1", URL: "http://x"})
	select {
	case b := <-ch:
		t.Fatalf("unexpected notification sent for deploy_success under default events: %s", b)
	case <-time.After(200 * time.Millisecond):
	}

	// deploy_failed delivered under default events.
	s.notify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a", DeployID: "1", Err: "boom"})
	b := recvNotify(t, ch, 2*time.Second)
	if !strings.Contains(string(b), `"event":"deploy_failed"`) {
		t.Fatalf("body = %s", b)
	}
}

func TestNotifyExplicitEventsCSVHonored(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("notify_events", "deploy_success,cert_issued"); err != nil {
		t.Fatal(err)
	}

	s.notify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a"})
	select {
	case b := <-ch:
		t.Fatalf("unexpected notification for deploy_failed with explicit events: %s", b)
	case <-time.After(200 * time.Millisecond):
	}

	s.notify(notifyEvent{Event: "deploy_success", Project: "p", App: "a", DeployID: "2", URL: "http://y"})
	recvNotify(t, ch, 2*time.Second)
}

func TestNotifyUnsetURLNoop(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})

	done := make(chan struct{})
	go func() {
		s.notify(notifyEvent{Event: "deploy_failed"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify() blocked with unset notify_url")
	}
}

func TestNotifyTelegramFormat(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("notify_format", "telegram"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_telegram_chat", "999"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_events", "deploy_success"); err != nil {
		t.Fatal(err)
	}

	s.notify(notifyEvent{Event: "deploy_success", Project: "p", App: "a", DeployID: "5", URL: "http://z"})
	b := recvNotify(t, ch, 2*time.Second)
	var out struct {
		ChatID string `json:"chat_id"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ChatID != "999" {
		t.Fatalf("chat_id = %q", out.ChatID)
	}
}

func TestNotifyTelegramMissingChatSkipsSend(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("notify_format", "telegram"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_events", "deploy_success"); err != nil {
		t.Fatal(err)
	}

	s.notify(notifyEvent{Event: "deploy_success", Project: "p", App: "a"})
	select {
	case b := <-ch:
		t.Fatalf("unexpected send with telegram chat unset: %s", b)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestNotifyErrTailTruncated(t *testing.T) {
	ch := make(chan []byte, 4)
	ts := httptest.NewServer(captureHandler(ch))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}

	longErr := strings.Repeat("x", 500)
	s.notify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a", Err: longErr})
	b := recvNotify(t, ch, 2*time.Second)
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Error) != errTailLimit {
		t.Fatalf("error len = %d, want %d", len(out.Error), errTailLimit)
	}
}

func TestNotifyPostFailureLoggedNotFatal(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, slow.URL)
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}
	s.httpClient = &http.Client{Timeout: 50 * time.Millisecond}

	done := make(chan struct{})
	go func() {
		s.sendNotify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a", Err: "x"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendNotify did not return after client timeout")
	}
}

func TestNotify500LoggedNotFatal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	setSealedNotifyURL(t, s, ts.URL)
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.sendNotify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a", Err: "x"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sendNotify blocked on 500 response")
	}
}

// TestNotifyEmailFormatSendsHTMLToEachRecipient: notify_format=email works
// with no notify_url set at all, delivers to every notify_email address,
// and a *_failed event's HTML carries the fail color plus an
// html-escaped error string (never raw-concatenated).
func TestNotifyEmailFormatSendsHTMLToEachRecipient(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	rm := newRecordingMailer(4)
	s.mailer = func() (mail.Mailer, error) { return rm, nil }

	if err := st.SetSetting("notify_format", "email"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_email", "a@x.co, b@y.co"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}

	s.notify(notifyEvent{
		Event: "deploy_failed", Project: "p", App: "a", DeployID: "1", Seq: 2,
		Err: "<script>boom</script>",
	})

	m1 := recvMail(t, rm.ch, 2*time.Second)
	m2 := recvMail(t, rm.ch, 2*time.Second)
	got := map[string]mailMsg{m1.to: m1, m2.to: m2}
	if _, ok := got["a@x.co"]; !ok {
		t.Fatalf("no mail sent to a@x.co: %+v", got)
	}
	if _, ok := got["b@y.co"]; !ok {
		t.Fatalf("no mail sent to b@y.co: %+v", got)
	}

	msg := got["a@x.co"]
	if !strings.Contains(msg.subject, "deploy_failed") || !strings.Contains(msg.subject, "p/a") {
		t.Fatalf("subject = %q", msg.subject)
	}
	if !strings.Contains(msg.html, "#D93336") {
		t.Fatalf("html missing fail color:\n%s", msg.html)
	}
	if strings.Contains(msg.html, "<script>boom</script>") {
		t.Fatalf("html contains raw unescaped error:\n%s", msg.html)
	}
	if !strings.Contains(msg.html, "&lt;script&gt;boom&lt;/script&gt;") {
		t.Fatalf("html missing escaped error:\n%s", msg.html)
	}
}

// TestNotifyEmailFormatSkipsNonSubscribedEvent: the notify_events
// subscription filter applies to the email format exactly like the webhook
// formats.
func TestNotifyEmailFormatSkipsNonSubscribedEvent(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	rm := newRecordingMailer(4)
	s.mailer = func() (mail.Mailer, error) { return rm, nil }

	if err := st.SetSetting("notify_format", "email"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_email", "a@x.co"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_events", "deploy_success"); err != nil {
		t.Fatal(err)
	}

	s.notify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a"})
	select {
	case m := <-rm.ch:
		t.Fatalf("unexpected email for non-subscribed event: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestNotifyEmailFormatSkipsWithoutRecipients: notify_format=email with
// notify_email unset is a no-op, not a panic or a send to "".
func TestNotifyEmailFormatSkipsWithoutRecipients(t *testing.T) {
	st := newTestStore(t)
	sealer, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(Deps{Store: st, Sealer: sealer, ExternalIP: "1.2.3.4"})
	rm := newRecordingMailer(4)
	s.mailer = func() (mail.Mailer, error) { return rm, nil }

	if err := st.SetSetting("notify_format", "email"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("notify_events", "deploy_failed"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		s.notify(notifyEvent{Event: "deploy_failed", Project: "p", App: "a"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify() blocked with notify_email unset")
	}
	select {
	case m := <-rm.ch:
		t.Fatalf("unexpected email with notify_email unset: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
