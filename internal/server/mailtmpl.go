package server

import (
	"bytes"
	"html/template"
	"strings"
	"time"
)

// mailShellTmpl is the shared HTML chrome for every outbound luncur email:
// a header with the luncur wordmark, a body slot, and a footer. Inline
// styles only — mail clients strip <style> tags and rarely load
// @font-face, hence the system-font stack. Light theme throughout (dark
// backgrounds render badly across mail clients).
var mailShellTmpl = template.Must(template.New("mailShell").Parse(`<!doctype html>
<html>
<body style="margin:0;padding:0;background:#F4F4F2;font-family:-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#F4F4F2;padding:24px 0;">
<tr><td align="center">
<table role="presentation" width="480" cellpadding="0" cellspacing="0" style="background:#FFFFFF;border:1px solid #E2E2DE;border-radius:8px;overflow:hidden;">
<tr><td style="padding:20px 24px;border-bottom:1px solid #E2E2DE;">
<span style="font-size:18px;font-weight:700;color:#111114;">luncur</span>
</td></tr>
<tr><td style="padding:24px;color:#33333A;font-size:14px;line-height:1.5;">
{{.}}
</td></tr>
<tr><td style="padding:16px 24px;border-top:1px solid #E2E2DE;color:#75757E;font-size:12px;">
This is an automated message from your luncur install.
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>
`))

// renderMailShell wraps body — an HTML fragment already produced by another
// html/template execution, so any user/event data inside it was escaped at
// that point — in the shared header/footer chrome.
func renderMailShell(body template.HTML) (string, error) {
	var buf bytes.Buffer
	if err := mailShellTmpl.Execute(&buf, body); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// inviteBodyTmpl renders the invite email's body fragment. Role, Link, and
// ExpiresAt are all attacker/user-influenced (role is admin-chosen but
// still passes through normal escaping here; link embeds an invite token)
// so they go through html/template's normal escaping — never string concat.
var inviteBodyTmpl = template.Must(template.New("inviteBody").Parse(`
<p style="margin:0 0 16px;font-size:16px;color:#111114;font-weight:600;">You're invited to luncur</p>
<p style="margin:0 0 16px;">You've been invited to join luncur as <strong>{{.Role}}</strong>.</p>
<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 16px;">
<tr><td style="background:#FF6A00;border-radius:6px;">
<a href="{{.Link}}" style="display:inline-block;padding:10px 20px;color:#ffffff;text-decoration:none;font-weight:600;font-size:14px;">Register</a>
</td></tr>
</table>
<p style="margin:0 0 8px;color:#75757E;font-size:13px;">Or paste this link into your browser:</p>
<p style="margin:0 0 16px;word-break:break-all;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;"><a href="{{.Link}}" style="color:#33333A;">{{.Link}}</a></p>
<p style="margin:0;color:#75757E;font-size:13px;">This link is single-use and expires {{.ExpiresAt}}.</p>
`))

// renderInviteHTML builds the invite email's HTML body: escaped fragment
// wrapped in the shared shell.
func renderInviteHTML(role, link, expiresAt string) (string, error) {
	var frag bytes.Buffer
	data := struct{ Role, Link, ExpiresAt string }{role, link, expiresAt}
	if err := inviteBodyTmpl.Execute(&frag, data); err != nil {
		return "", err
	}
	return renderMailShell(template.HTML(frag.String()))
}

// notifyStatusColor picks the heading color for a notification email by
// event outcome: fail (red) for any *_failed event, warn (amber) for
// app_unhealthy, ok (green) for the success events, neutral otherwise.
func notifyStatusColor(event string) string {
	switch {
	case strings.HasSuffix(event, "_failed"):
		return "#D93336"
	case event == "app_unhealthy":
		return "#B87A00"
	case event == "deploy_success", event == "cert_issued":
		return "#1FA55C"
	default:
		return "#33333A"
	}
}

// notifyBodyTmpl renders a notification email's body fragment: a
// status-colored heading (the human-readable notifyMessage line) plus a
// small field table. Project/App/URL/Err are event data (Err in particular
// may carry arbitrary upstream error text) and always go through
// html/template's normal escaping.
var notifyBodyTmpl = template.Must(template.New("notifyBody").Parse(`
<p style="margin:0 0 12px;font-size:16px;font-weight:600;color:{{.Color}};">{{.Message}}</p>
<table role="presentation" cellpadding="0" cellspacing="0" style="width:100%;border-collapse:collapse;font-size:13px;">
{{if .Project}}<tr><td style="padding:4px 8px 4px 0;color:#75757E;">Project</td><td style="padding:4px 0;color:#33333A;">{{.Project}}</td></tr>{{end}}
{{if .App}}<tr><td style="padding:4px 8px 4px 0;color:#75757E;">App</td><td style="padding:4px 0;color:#33333A;">{{.App}}</td></tr>{{end}}
{{if .URL}}<tr><td style="padding:4px 8px 4px 0;color:#75757E;">URL</td><td style="padding:4px 0;color:#33333A;word-break:break-all;"><a href="{{.URL}}" style="color:#33333A;">{{.URL}}</a></td></tr>{{end}}
{{if .Err}}<tr><td style="padding:4px 8px 4px 0;color:#75757E;">Error</td><td style="padding:4px 0;color:#D93336;word-break:break-all;">{{.Err}}</td></tr>{{end}}
<tr><td style="padding:4px 8px 4px 0;color:#75757E;">Time</td><td style="padding:4px 0;color:#33333A;">{{.Time}}</td></tr>
</table>
`))

// renderNotifyHTML builds a notification email's HTML body for ev: escaped
// fragment wrapped in the shared shell.
func renderNotifyHTML(ev notifyEvent) (string, error) {
	data := struct {
		Color   template.CSS // one of a fixed set of hex constants we control, never user input
		Message string
		Project string
		App     string
		URL     string
		Err     string
		Time    string
	}{
		Color:   template.CSS(notifyStatusColor(ev.Event)),
		Message: notifyMessage(ev),
		Project: ev.Project,
		App:     ev.App,
		URL:     ev.URL,
		Err:     ev.Err,
		Time:    time.Now().UTC().Format(time.RFC3339),
	}
	var frag bytes.Buffer
	if err := notifyBodyTmpl.Execute(&frag, data); err != nil {
		return "", err
	}
	return renderMailShell(template.HTML(frag.String()))
}
