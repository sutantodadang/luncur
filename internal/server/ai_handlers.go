package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/ai"
	"github.com/sutantodadang/luncur/internal/store"
)

// writeAIError maps the assistant's sentinel errors to API responses.
func writeAIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAIDisabled):
		writeError(w, http.StatusServiceUnavailable, "ai_disabled", err.Error())
	case errors.Is(err, errAIBudget):
		writeError(w, http.StatusTooManyRequests, "ai_budget", err.Error())
	case errors.Is(err, ai.ErrRefused):
		writeError(w, http.StatusUnprocessableEntity, "ai_refused", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "ai_error", err.Error())
	}
}

// handleAIStatus reports whether the assistant is configured.
func (s *server) handleAIStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	out := map[string]any{"enabled": false}
	cfg, err := s.aiSetup(r.Context())
	if err != nil {
		out["reason"] = err.Error()
	} else {
		out["enabled"] = true
		out["provider"] = cfg.provider.Name()
		out["notify"] = s.aiSetting(settingAINotify) == "on"
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAIExplain diagnoses an app's latest (or a given) failed deploy or
// job run. Read-only: any project member, viewers included, may ask.
func (s *server) handleAIExplain(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Project  string `json:"project"`
		Env      string `json:"env"`
		App      string `json:"app"`
		DeployID string `json:"deploy_id"`
		RunID    string `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	p, env, ok := s.requireEnv(w, r, u, req.Project, req.Env)
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, env, req.App)
	if !ok {
		return
	}
	d, usage, err := s.aiExplain(r.Context(), u, aiExplainInput{Project: p, Env: env, App: a, Deploy: req.DeployID, Run: req.RunID})
	s.aiRecordUsage(u.ID, "explain", usage)
	if err != nil {
		writeAIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleAIChat runs one ops-assistant turn as the caller. Mutations made
// before a mid-turn failure are still reported (actions + error), so the
// caller always learns what changed.
func (s *server) handleAIChat(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		ConversationID string `json:"conversation_id"`
		Project        string `json:"project"`
		Env            string `json:"env"`
		Message        string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message is required")
		return
	}
	if req.Project != "" {
		if _, ok := s.requireProject(w, u, req.Project); !ok {
			return
		}
	}
	res, err := s.aiChat(r.Context(), u, req.ConversationID, req.Project, req.Env, req.Message)
	// Nothing ran: answer with the error status. Once a tool call has run,
	// the caller must see what changed, so partial results go out as 200.
	if err != nil && (errors.Is(err, errAIDisabled) || (errors.Is(err, errAIBudget) && len(res.Actions) == 0)) {
		writeAIError(w, err)
		return
	}
	out := map[string]any{"conversation_id": res.ConversationID, "reply": res.Reply, "actions": res.Actions}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAIGenerate writes a validated pipeline.yaml / params.yaml /
// override patch / Dockerfile from a description.
func (s *server) handleAIGenerate(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct {
		Kind         string `json:"kind"`
		Description  string `json:"description"`
		Current      string `json:"current"`
		Project      string `json:"project"`
		Env          string `json:"env"`
		App          string `json:"app"`
		OverrideKind string `json:"override_kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "description is required")
		return
	}
	if _, ok := aiGenKinds[req.Kind]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "kind must be pipeline, params, override or dockerfile")
		return
	}
	in := aiGenerateInput{Kind: req.Kind, Description: req.Description, Current: req.Current, OverrideKind: req.OverrideKind}
	if req.Project != "" {
		p, env, ok := s.requireEnv(w, r, u, req.Project, req.Env)
		if !ok {
			return
		}
		in.Project = &p
		in.Context = s.aiGenerateContext(p, env, req.Kind, req.App, req.OverrideKind)
	}
	res, err := s.aiGenerate(r.Context(), u, in)
	if err != nil {
		writeAIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// aiGenerateContext gives the model the names it needs: the project's apps
// for a pipeline, the app's current base manifest for an override.
func (s *server) aiGenerateContext(p store.Project, env store.Environment, kind, appName, overrideKind string) string {
	switch kind {
	case "pipeline":
		apps, err := s.st.ListAppsInEnv(env.ID)
		if err != nil {
			return ""
		}
		var b strings.Builder
		b.WriteString("apps in this environment (name: kind):\n")
		for _, a := range apps {
			fmt.Fprintf(&b, "- %s: %s\n", a.Name, a.Kind)
		}
		return b.String()
	case "override":
		if appName == "" || overrideKind == "" {
			return ""
		}
		a, err := s.st.GetAppInEnv(env.ID, appName)
		if err != nil {
			return ""
		}
		doc, err := s.editDoc(p, env, a, overrideKind, false)
		if err != nil {
			return ""
		}
		return "the app's current base " + overrideKind + " manifest:\n" + string(doc)
	}
	return ""
}

// handleAIUsage reports token usage: admins see everyone's, members their own.
func (s *server) handleAIUsage(w http.ResponseWriter, r *http.Request, u store.User) {
	days := 7
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
		days = v
	}
	rows, err := s.st.AIUsage(days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]store.AIUsageRow, 0, len(rows))
	for _, row := range rows {
		if u.Role == "admin" || row.UserEmail == u.Email {
			out = append(out, row)
		}
	}
	today, _ := s.st.AITokensToday()
	budget := int64(defaultAIDailyBudget)
	if n, err := strconv.ParseInt(s.aiSetting(settingAIDailyBudget), 10, 64); err == nil && n >= 0 {
		budget = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "today_tokens": today, "daily_budget": budget})
}
