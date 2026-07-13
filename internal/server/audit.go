package server

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// auditInfo is planted into the request context by auditMiddleware (as a
// pointer) so inner layers — authed, uiPage, handleLogin, the webhook
// trigger — can fill in who made the request and which route matched. Data
// flows down through ctx as usual; the pointer lets those inner fill sites
// hand information back up to the middleware without changing every
// handler's signature.
type auditInfo struct {
	Email   string
	Pattern string // r.Pattern, as seen inside the matched handler
}

type auditCtxKey struct{}

// auditFrom returns the auditInfo planted in ctx, or nil when auditMiddleware
// didn't plant one (GET/HEAD/OPTIONS requests skip it entirely). Fill sites
// must nil-check before writing.
func auditFrom(ctx context.Context) *auditInfo {
	v, _ := ctx.Value(auditCtxKey{}).(*auditInfo)
	return v
}

// statusRecorder captures the status code a handler wrote so auditMiddleware
// can decide whether the request succeeded, without altering the response
// actually sent to the client.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController
// (and anything type-asserting http.Flusher/http.Hijacker through it) can
// still reach it — not that GET requests, which is all that matters for
// SSE/log-streaming, ever pass through this recorder in the first place.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// auditRetentionDays reads the audit_retention_days setting, defaulting to
// 90 when unset or invalid. 0 means keep forever.
func (s *server) auditRetentionDays() int {
	const defaultDays = 90
	v, err := s.st.GetSetting("audit_retention_days")
	if err != nil {
		return defaultDays
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultDays
	}
	return n
}

// auditMiddleware records one audit_log row per successful mutating request
// (POST/PUT/DELETE, a resolved user, response status < 400). GET/HEAD/OPTIONS
// requests pass straight through untouched — no context value planted, no
// writer wrapping — so runtime log streaming's http.Flusher type assertion
// (logs.go/sse.go) keeps working exactly as before.
//
// Fill sites (authed, uiPage, handleLogin, handleWebhookTrigger) write the
// resolved user's email and the matched route pattern into the planted
// auditInfo; this middleware only reads it back out afterward.
func (s *server) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		info := &auditInfo{}
		ctx := context.WithValue(r.Context(), auditCtxKey{}, info)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		if info.Email == "" || rec.status >= 400 {
			return
		}

		action := info.Pattern
		if action == "" {
			action = r.Method + " " + r.URL.Path
		}
		// Invite tokens ride in the path (DELETE /v1/invites/{token}) — the
		// raw secret must never be persisted, so a token-bearing route
		// stores its pattern instead of the actual path.
		target := r.URL.Path
		if strings.Contains(info.Pattern, "{token}") {
			target = info.Pattern
		}
		if err := s.st.AppendAudit(info.Email, action, target); err != nil {
			log.Printf("audit append: %v", err)
		}

		if days := s.auditRetentionDays(); days > 0 {
			if _, err := s.st.PruneAudit(days); err != nil {
				log.Printf("audit prune: %v", err)
			}
		}
	})
}

// auditEntryJSON is one row of the GET /v1/audit response.
type auditEntryJSON struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"created_at"`
	UserEmail string `json:"user_email"`
	Action    string `json:"action"`
	Target    string `json:"target"`
}

// handleListAudit is the admin-only API listing of the audit log.
func (s *server) handleListAudit(w http.ResponseWriter, r *http.Request, _ store.User) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	entries, err := s.st.ListAudit(limit, offset, q.Get("user"), q.Get("contains"))
	if err != nil {
		log.Printf("list audit: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]auditEntryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, auditEntryJSON{
			ID: e.ID, CreatedAt: e.CreatedAt, UserEmail: e.UserEmail,
			Action: e.Action, Target: e.Target,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// auditPageSize is how many audit rows one UI page shows.
const auditPageSize = 50

// handleUIAudit shows the audit log to admins only, paginated and filterable
// by user and a substring match. Query params: user, contains, page (0-based).
// We fetch one extra row to know whether a next page exists without a separate
// COUNT query.
func (s *server) handleUIAudit(w http.ResponseWriter, r *http.Request, u store.User) {
	if !s.uiAdmin(w, u) {
		return
	}
	q := r.URL.Query()
	user := strings.TrimSpace(q.Get("user"))
	contains := strings.TrimSpace(q.Get("contains"))
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}

	entries, err := s.st.ListAudit(auditPageSize+1, page*auditPageSize, user, contains)
	if err != nil {
		log.Printf("ui audit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hasNext := len(entries) > auditPageSize
	if hasNext {
		entries = entries[:auditPageSize]
	}

	s.renderPage(w, "audit.html", map[string]any{
		"User": u, "Entries": entries,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin",
		"FilterUser": user, "FilterContains": contains,
		"Page": page, "PageDisplay": page + 1, "PrevPage": page - 1, "NextPage": page + 1,
		"HasPrev": page > 0, "HasNext": hasNext,
		"Filtered": user != "" || contains != "",
	})
}
