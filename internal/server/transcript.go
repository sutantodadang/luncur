package server

import (
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// splitAction splits an audit_log row's Action ("POST /ui/...") into its
// HTTP method and route pattern. Every audit row is "METHOD pattern" (see
// auditMiddleware — GET/HEAD/OPTIONS never reach the audit log at all), but
// a defensive fallback keeps this safe regardless.
func splitAction(action string) (method, pattern string) {
	method, pattern, ok := strings.Cut(action, " ")
	if !ok {
		return "", action
	}
	return method, pattern
}

// pathVarsFrom pairs each {name} placeholder in a route pattern's path with
// the literal segment at the same position in the actual request path
// recorded as the audit row's Target. The pattern is exactly what routed
// the request that produced target, so segment counts always line up; a
// short target just yields fewer vars, which callers treat as "value
// unknown" (empty string) rather than erroring.
func pathVarsFrom(patternPath, target string) map[string]string {
	pSegs := strings.Split(strings.Trim(patternPath, "/"), "/")
	tSegs := strings.Split(strings.Trim(target, "/"), "/")
	vars := make(map[string]string, len(pSegs))
	for i, seg := range pSegs {
		if i >= len(tSegs) {
			break
		}
		if len(seg) > 1 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			name := strings.TrimSuffix(seg[1:len(seg)-1], "...")
			vars[name] = tSegs[i]
		}
	}
	return vars
}

// auditToCLI maps one audited UI mutation (method + route pattern, e.g.
// "POST" and "/ui/projects/{project}/apps/{app}/scale") to the CLI command
// a terminal user would run for the same effect — the Session Transcript
// panel's core (DESIGN.md "Session Transcript"). Audit rows record only the
// route pattern and the literal request path, never form bodies, so
// anything a form field would have carried (replica counts, env keys,
// hostnames, addon types, ...) is rendered as a "<placeholder>" rather than
// invented: the transcript is a runbook skeleton, not a perfect replay.
// Unmapped routes return false; the caller skips those rows instead of
// rendering the raw route pattern.
func auditToCLI(method, pattern string, vars map[string]string) (string, bool) {
	p, a := vars["project"], vars["app"]
	switch method + " " + pattern {
	case "POST /ui/projects/{project}/apps/{app}/deploy":
		return "luncur deploy " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/redeploy":
		return "luncur redeploy " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/rollback":
		return "luncur rollback " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/scale":
		return "luncur scale " + a + " <n> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/autoscale":
		return "luncur autoscale " + a + " --min <n> --max <n> --cpu <n> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/env":
		return "luncur env set " + a + " <KEY=VALUE> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/env/bulk":
		return "luncur env push " + a + " <file> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/env/delete":
		return "luncur env unset " + a + " <key> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/domains":
		return "luncur domain add " + a + " <hostname> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/domains/delete":
		return "luncur domain remove " + a + " <hostname> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/domains/retry":
		return "luncur domain retry " + a + " <hostname> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/volumes":
		return "luncur volume add " + a + " <path> --size <n> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/volumes/remove":
		return "luncur volume remove " + a + " <name> --project " + p, true
	case "POST /ui/projects/{project}/addons":
		return "luncur addon create <type> --project " + p, true
	case "POST /ui/projects/{project}/addons/delete":
		return "luncur addon remove <name> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/addons/attach":
		return "luncur addon attach <name> " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/addons/detach":
		return "luncur addon detach <name> " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/git-token":
		return "luncur app git-token set " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/git-token/clear":
		return "luncur app git-token clear " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/webhook":
		return "luncur webhook enable " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/webhook/disable":
		return "luncur webhook disable " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/pause":
		return "luncur app pause " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/resume":
		return "luncur app resume " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/run-now":
		return "luncur app run-now " + a + " --project " + p, true
	case "POST /ui/projects/{project}/apps":
		return "luncur app create <name> --project " + p, true
	case "POST /ui/projects/{project}/apps/{app}/destroy":
		return "luncur destroy " + a + " --project " + p, true
	case "POST /ui/projects":
		return "luncur project create <name>", true
	case "POST /ui/projects/{project}/rename":
		return "luncur project rename " + p + " <new-name>", true
	default:
		return "", false
	}
}

// tickerSummary turns one audit row into the short human line the event
// ticker shows (DESIGN.md "Event ticker"). It reuses auditToCLI's mapping
// where available — stripping the "luncur " prefix and any flags leaves a
// clean "<verb> <app>" phrase — and falls back to a generic verb built from
// the route's last path segment for everything auditToCLI doesn't cover
// (logins, settings, invites, ...), so the ticker never shows a raw route
// pattern.
func tickerSummary(action, target string) string {
	method, pattern := splitAction(action)
	vars := pathVarsFrom(pattern, target)
	if cmd, ok := auditToCLI(method, pattern, vars); ok {
		cmd = strings.TrimPrefix(cmd, "luncur ")
		if i := strings.Index(cmd, " --"); i >= 0 {
			cmd = cmd[:i]
		}
		return cmd
	}
	verb := pattern
	if i := strings.LastIndex(verb, "/"); i >= 0 {
		verb = verb[i+1:]
	}
	verb = strings.ReplaceAll(strings.Trim(verb, "{}"), "-", " ")
	if verb == "" {
		verb = "update"
	}
	if a := vars["app"]; a != "" {
		return verb + " " + a
	}
	if p := vars["project"]; p != "" {
		return verb + " " + p
	}
	return verb
}

// tickerData feeds the "ticker-msg" fragment template — the initial poll
// (via hx-trigger="load") and every subsequent 15s tick render the exact
// same shape, which is what keeps the self-perpetuating hx-get/hx-trigger
// contract alive across outerHTML swaps (same idiom as "statuschip").
type tickerData struct {
	TS       string // "HH:MM:SS"
	Text     string
	TSMillis int64
	Kind     string // "err" colors Text fail-red; anything else is terminal phosphor
}

// handleUITicker is the event ticker's poll target (DESIGN.md "Event
// ticker"): the single latest audit row the requesting user may see —
// admins see the whole log, everyone else sees only their own actions, the
// same boundary /ui/audit itself enforces. SECURITY: this scoping is load
// bearing, not cosmetic — leaking another user's row here would defeat the
// non-admin restriction the audit page already guarantees.
func (s *server) handleUITicker(w http.ResponseWriter, r *http.Request, u store.User) {
	scopeUser := u.Email
	if u.Role == "admin" {
		scopeUser = ""
	}
	entries, err := s.st.ListAudit(1, 0, scopeUser, "")
	if err != nil {
		log.Printf("ui ticker: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := tickerData{Kind: "ok"}
	if len(entries) > 0 {
		e := entries[0]
		if len(e.CreatedAt) >= 19 {
			data.TS = e.CreatedAt[11:19]
		}
		data.Text = tickerSummary(e.Action, e.Target)
		if t, err := time.Parse("2006-01-02 15:04:05", e.CreatedAt); err == nil {
			data.TSMillis = t.UnixMilli()
		}
	} else {
		data.Text = "no activity yet"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "ticker-msg", data); err != nil {
		log.Printf("render ticker-msg: %v", err)
	}
}

// transcriptWindow is how far back the Session Transcript panel looks:
// there's no real session table to key off (DESIGN.md: "no real session
// correlation"), so a rolling window of the user's own audit rows stands in
// for "this session".
const transcriptWindow = 4 * time.Hour

// transcriptMaxRows caps how many mapped commands the panel renders.
const transcriptMaxRows = 50

// transcriptLine is one row of the rendered runbook: a CLI command plus the
// "# HH:MM" comment timestamp shown above it, so the whole <pre> stays a
// valid, copyable shell script.
type transcriptLine struct {
	TS      string
	Command string
}

// handleUITranscript renders the Session Transcript fragment (DESIGN.md
// "Session Transcript"): the current user's own audit rows from the last
// transcriptWindow, translated to CLI commands via auditToCLI, chronological
// (oldest first) so the panel reads top-to-bottom like a runbook you could
// paste into a shell. SECURITY: scoped strictly to u.Email regardless of
// role — this is the one place besides /ui/audit the audit log surfaces in
// the UI, so a non-admin must only ever see their own actions here.
func (s *server) handleUITranscript(w http.ResponseWriter, r *http.Request, u store.User) {
	entries, err := s.st.ListAudit(200, 0, u.Email, "")
	if err != nil {
		log.Printf("ui transcript: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cutoff := time.Now().UTC().Add(-transcriptWindow).Format("2006-01-02 15:04:05")

	lines := make([]transcriptLine, 0, transcriptMaxRows)
	for _, e := range entries { // ListAudit is newest-first
		if e.CreatedAt < cutoff {
			break
		}
		method, pattern := splitAction(e.Action)
		cmd, ok := auditToCLI(method, pattern, pathVarsFrom(pattern, e.Target))
		if !ok {
			continue
		}
		ts := e.CreatedAt
		if len(ts) >= 16 {
			ts = ts[11:16]
		}
		lines = append(lines, transcriptLine{TS: ts, Command: cmd})
		if len(lines) == transcriptMaxRows {
			break
		}
	}
	slices.Reverse(lines) // newest-first -> chronological script order

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "transcript-body", map[string]any{"Lines": lines}); err != nil {
		log.Printf("render transcript-body: %v", err)
	}
}
