package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// errTailLimit bounds how much of an error string rides in a notification.
const errTailLimit = 300

// defaultNotifyEvents is the notify_events value used when the setting is
// unset.
const defaultNotifyEvents = "deploy_failed,deploy_rolled_back,cert_failed,app_unhealthy,app_down,backup_failed"

// notifyEventNames are the valid values inside notify_events (CSV).
var notifyEventNames = map[string]bool{
	"deploy_success": true,
	"deploy_failed":  true,
	"cert_issued":    true,
	"cert_failed":    true,
	"pipeline":       true,
	"app_unhealthy":  true,
	"backup_failed":  true,
	// deploy_rolled_back: the rollout gate failed a deploy and auto-rollback
	// restored the previous one. app_down/app_recovered: the uptime prober
	// opened/resolved an incident.
	"deploy_rolled_back": true,
	"app_down":           true,
	"app_recovered":      true,
}

// notifyFormats are the valid values for notify_format.
var notifyFormats = map[string]bool{
	"generic":  true,
	"discord":  true,
	"slack":    true,
	"telegram": true,
	"email":    true,
}

// validNotifyEvents reports whether v is a non-empty, whitespace-tolerant
// CSV of known event names — used by settings.go's set-time validation.
func validNotifyEvents(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	for _, part := range strings.Split(v, ",") {
		name := strings.TrimSpace(part)
		if name == "" || !notifyEventNames[name] {
			return false
		}
	}
	return true
}

// parseNotifyEvents splits/normalizes a notify_events CSV value into a set.
func parseNotifyEvents(csv string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(csv, ",") {
		name := strings.TrimSpace(part)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// notifyEvent describes one deploy/cert/pipeline/health outcome to report to
// the configured notification webhook (see the notify_* settings).
type notifyEvent struct {
	Event    string // see notifyEventNames
	Project  string
	App      string
	DeployID string // "" for cert/pipeline events — internal id, kept for API consumers
	Seq      int64  // 0 for cert/pipeline events — per-app deploy number shown in human-readable text
	URL      string // app URL (deploy events) or hostname (cert events)
	Err      string // error detail; truncated to errTailLimit chars before sending
	Message  string // free-form text for event "pipeline" (notify actions + run finish summaries)
	// explicit marks a notification the user asked for directly (a pipeline
	// `notify:` step), which skips the notify_events subscription filter —
	// only the delivery channel needs to be configured.
	explicit bool
	// AI is an optional one-line AI diagnosis (ai_notify=on) appended to
	// deploy_failed / app_unhealthy messages.
	AI string
}

// notify is the best-effort entry point: it reads notify_format to pick the
// delivery gate — notify_email (plain) for format "email", sealed
// notify_url for every webhook format — bails out if the feature is off or
// this event isn't subscribed to, and otherwise spawns a single delivery
// attempt in a goroutine. Never blocks or fails the calling path.
func (s *server) notify(ev notifyEvent) {
	format, err := s.st.GetSetting("notify_format")
	if err != nil || format == "" {
		format = "generic"
	}

	if format == "email" {
		email, err := s.st.GetSetting("notify_email")
		if err != nil || email == "" {
			return // unset/unconfigured = feature off
		}
	} else {
		url, err := s.sealedSetting("notify_url")
		if err != nil || url == "" {
			return // unset/unconfigured = feature off
		}
	}

	eventsCSV, err := s.st.GetSetting("notify_events")
	if err != nil {
		eventsCSV = defaultNotifyEvents
	}
	if !ev.explicit && !parseNotifyEvents(eventsCSV)[ev.Event] {
		return
	}

	if len(ev.Err) > errTailLimit {
		ev.Err = ev.Err[len(ev.Err)-errTailLimit:]
	}

	go func() {
		if ev.Event == "deploy_failed" || ev.Event == "app_unhealthy" {
			ev.AI = s.aiNotifySummary(ev)
		}
		s.sendNotify(ev)
	}()
}

// sendNotify builds the format-specific payload and POSTs it (or, for
// notify_format=email, hands off to sendNotifyEmail): one attempt,
// s.httpClient's timeout, failures logged only — never surfaced to callers.
func (s *server) sendNotify(ev notifyEvent) {
	format, err := s.st.GetSetting("notify_format")
	if err != nil || format == "" {
		format = "generic"
	}

	if format == "email" {
		s.sendNotifyEmail(ev)
		return
	}

	url, err := s.sealedSetting("notify_url")
	if err != nil || url == "" {
		return
	}

	chat, _ := s.st.GetSetting("notify_telegram_chat")
	if format == "telegram" && chat == "" {
		log.Printf("notify: telegram format configured without notify_telegram_chat, skipping %s", ev.Event)
		return
	}

	payload, err := buildNotifyPayload(format, chat, ev, time.Now())
	if err != nil {
		log.Printf("notify: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		log.Printf("notify: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("notify: post %s: %v", ev.Event, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("notify: %s: server returned %d", ev.Event, resp.StatusCode)
	}
}

// sendNotifyEmail delivers ev as an HTML email to each address in
// notify_email (a CSV setting, not sealed — see settings.go). Best-effort:
// a missing recipient list, an unconfigured mailer (mail.ErrUnconfigured),
// or a per-recipient send failure is logged only, never surfaced — mirrors
// sendNotify's webhook path.
func (s *server) sendNotifyEmail(ev notifyEvent) {
	csv, err := s.st.GetSetting("notify_email")
	if err != nil || csv == "" {
		log.Printf("notify: email format configured without notify_email, skipping %s", ev.Event)
		return
	}
	var recipients []string
	for _, part := range strings.Split(csv, ",") {
		addr := strings.TrimSpace(part)
		if addr != "" {
			recipients = append(recipients, addr)
		}
	}
	if len(recipients) == 0 {
		return
	}

	m, err := s.mailer()
	if err != nil {
		log.Printf("notify: mailer unavailable, skipping %s: %v", ev.Event, err)
		return
	}

	subject := fmt.Sprintf("[luncur] %s", notifySubjectLine(ev))
	text := notifyMessage(ev)
	html, err := renderNotifyHTML(ev)
	if err != nil {
		log.Printf("notify: render html for %s: %v", ev.Event, err)
		return
	}

	for _, addr := range recipients {
		if err := m.Send(addr, subject, text, html); err != nil {
			log.Printf("notify: email %s to %s: %v", ev.Event, addr, err)
		}
	}
}

// notifySubjectLine renders the "event — project/app" portion of a
// notification email's subject; cert/backup events carry no App (and
// backup_failed carries no Project either).
func notifySubjectLine(ev notifyEvent) string {
	switch {
	case ev.App != "":
		return fmt.Sprintf("%s — %s/%s", ev.Event, ev.Project, ev.App)
	case ev.Project != "":
		return fmt.Sprintf("%s — %s", ev.Event, ev.Project)
	default:
		return ev.Event
	}
}

// genericNotifyPayload is the JSON body sent for notify_format=generic.
type genericNotifyPayload struct {
	Event    string `json:"event"`
	Project  string `json:"project"`
	App      string `json:"app"`
	DeployID string `json:"deploy_id,omitempty"`
	Status   string `json:"status"`
	URL      string `json:"url,omitempty"`
	Error    string `json:"error,omitempty"`
	Message  string `json:"message,omitempty"`
	AI       string `json:"ai,omitempty"`
	Time     string `json:"time"`
}

// notifyStatus maps an event name to the short status word the generic
// payload's "status" field carries. "pipeline" carries its own outcome in
// Message (a run can finish done or failed, and a notify-action fires
// mid-run) rather than in this field, so it reports the neutral "info".
func notifyStatus(event string) string {
	switch event {
	case "deploy_success":
		return "live"
	case "cert_issued":
		return "issued"
	case "pipeline":
		return "info"
	case "deploy_rolled_back":
		return "rolled_back"
	case "app_recovered":
		return "recovered"
	case "app_down":
		return "down"
	default:
		return "failed"
	}
}

// notifyMessage renders the one human-readable line used by the
// discord/slack/telegram encoders.
func notifyMessage(ev notifyEvent) string {
	msg := notifyMessageBase(ev)
	if ev.AI != "" {
		msg += "\n🤖 " + ev.AI
	}
	return msg
}

func notifyMessageBase(ev notifyEvent) string {
	switch ev.Event {
	case "deploy_success":
		return fmt.Sprintf("✅ %s/%s deploy #%d live — %s", ev.Project, ev.App, ev.Seq, ev.URL)
	case "deploy_failed":
		return fmt.Sprintf("❌ %s/%s deploy #%d failed: %s", ev.Project, ev.App, ev.Seq, ev.Err)
	case "cert_issued":
		return fmt.Sprintf("🔒 %s cert issued", ev.URL)
	case "cert_failed":
		return fmt.Sprintf("⚠️ %s cert failed: %s", ev.URL, ev.Err)
	case "pipeline":
		return fmt.Sprintf("🔧 %s/%s: %s", ev.Project, ev.App, ev.Message)
	case "app_unhealthy":
		return fmt.Sprintf("🚨 %s/%s unhealthy: %s", ev.Project, ev.App, ev.Err)
	case "backup_failed":
		return fmt.Sprintf("💾 scheduled backup failed: %s", ev.Err)
	case "deploy_rolled_back":
		return fmt.Sprintf("↩️ %s/%s rolled back: %s", ev.Project, ev.App, ev.Message)
	case "app_down":
		return fmt.Sprintf("🔴 %s/%s is down: %s", ev.Project, ev.App, ev.Err)
	case "app_recovered":
		return fmt.Sprintf("🟢 %s/%s recovered: %s", ev.Project, ev.App, ev.Message)
	default:
		return fmt.Sprintf("%s: %s/%s", ev.Event, ev.Project, ev.App)
	}
}

// buildNotifyPayload builds the wire body for one notification, per format.
// Pure and unit-testable without any HTTP involved.
func buildNotifyPayload(format, chat string, ev notifyEvent, now time.Time) ([]byte, error) {
	switch format {
	case "generic", "":
		return json.Marshal(genericNotifyPayload{
			Event:    ev.Event,
			Project:  ev.Project,
			App:      ev.App,
			DeployID: ev.DeployID,
			Status:   notifyStatus(ev.Event),
			URL:      ev.URL,
			Error:    ev.Err,
			Message:  ev.Message,
			AI:       ev.AI,
			Time:     now.UTC().Format(time.RFC3339),
		})
	case "discord":
		return json.Marshal(map[string]string{"content": notifyMessage(ev)})
	case "slack":
		return json.Marshal(map[string]string{"text": notifyMessage(ev)})
	case "telegram":
		return json.Marshal(map[string]string{"chat_id": chat, "text": notifyMessage(ev)})
	default:
		return nil, fmt.Errorf("unknown notify_format %q", format)
	}
}
