package server

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/sutantodadang/luncur/internal/ai"
	"github.com/sutantodadang/luncur/internal/store"
)

// aiConfigured is a cheap "show AI controls?" check for page renders: a
// provider is selected (full validation happens when a control is used).
func (s *server) aiConfigured() bool {
	if s.aiProviderFn != nil {
		return true
	}
	p := s.aiSetting(settingAIProvider)
	return p == "claude" || p == "openai"
}

// aiErrorText is the one-line error a UI fragment shows.
func aiErrorText(err error) string {
	switch {
	case errors.Is(err, errAIDisabled), errors.Is(err, errAIBudget), errors.Is(err, ai.ErrRefused):
		return err.Error()
	}
	return "the AI request failed: " + err.Error()
}

// handleUIAIExplain renders the 3-line error contract for a failed deploy or
// run, as an htmx fragment. Read-only, so viewers may use it too.
func (s *server) handleUIAIExplain(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	env, ok := s.uiAppEnv(w, a)
	if !ok {
		return
	}
	data := map[string]any{}
	d, usage, err := s.aiExplain(r.Context(), u, aiExplainInput{
		Project: p, Env: env, App: a, Deploy: r.PostFormValue("deploy_id"), Run: r.PostFormValue("run_id"),
	})
	s.aiRecordUsage(u.ID, "explain", usage)
	if err != nil {
		data["Error"] = aiErrorText(err)
	} else {
		data["D"] = d
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "aiexplain", data); err != nil {
		log.Printf("render aiexplain: %v", err)
	}
}

// assistantData builds the assistant page's view model.
func (s *server) assistantData(w http.ResponseWriter, r *http.Request, u store.User, convID string) map[string]any {
	data := map[string]any{
		"User": u, "IsAdmin": u.Role == "admin", "CSRF": s.csrf(w, r),
		"AIEnabled": s.aiConfigured(), "ConversationID": convID,
		"Turns":   s.aiChatTurns(convID, u),
		"Project": r.URL.Query().Get("project"), "Env": r.URL.Query().Get("env"),
	}
	if !s.aiConfigured() {
		_, err := s.aiSetup(r.Context())
		if err != nil {
			data["Reason"] = err.Error()
		}
	}
	var names []string
	if u.Role == "admin" {
		if ps, err := s.st.ListProjects(); err == nil {
			for _, p := range ps {
				names = append(names, p.Name)
			}
		}
	} else if ps, err := s.st.ListProjectsFor(u.ID); err == nil {
		for _, p := range ps {
			names = append(names, p.Name)
		}
	}
	data["Projects"] = names
	if today, err := s.st.AITokensToday(); err == nil {
		data["TodayTokens"] = today
	}
	return data
}

// handleUIAssistant is the assistant page: chat transcript, ask form,
// generate form, usage and the `luncur mcp` setup snippet.
func (s *server) handleUIAssistant(w http.ResponseWriter, r *http.Request, u store.User) {
	data := s.assistantData(w, r, u, r.URL.Query().Get("c"))
	// ?q= prefills the ask box (e.g. the Insights page's "ask the assistant").
	data["Prefill"] = r.URL.Query().Get("q")
	s.renderPage(w, r, "assistant.html", data)
}

// handleUIAssistantAsk runs one chat turn and re-renders the transcript
// (htmx) or redirects back to the page.
func (s *server) handleUIAssistantAsk(w http.ResponseWriter, r *http.Request, u store.User) {
	project := strings.TrimSpace(r.PostFormValue("project"))
	env := strings.TrimSpace(r.PostFormValue("env"))
	message := strings.TrimSpace(r.PostFormValue("message"))
	if project != "" {
		if _, ok := s.uiProjectByName(w, u, project); !ok {
			return
		}
	}
	convID := r.PostFormValue("conversation_id")
	if message != "" {
		res, err := s.aiChat(r.Context(), u, convID, project, env, message)
		if res.ConversationID != "" {
			convID = res.ConversationID
		}
		if err != nil && res.ConversationID == "" {
			flash(w, "err", aiErrorText(err))
		}
	}
	if r.Header.Get("HX-Request") == "" {
		q := url.Values{"c": {convID}, "project": {project}, "env": {env}}
		http.Redirect(w, r, "/ui/assistant?"+q.Encode(), http.StatusSeeOther)
		return
	}
	data := map[string]any{"ConversationID": convID, "Turns": s.aiChatTurns(convID, u)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "aitranscript", data); err != nil {
		log.Printf("render aitranscript: %v", err)
	}
}

// uiGenTargets are the editors a generated file may be swapped into: the
// fragment re-renders the same textarea (id, name, classes) filled in.
var uiGenTargets = map[string]struct{ Name, Class, Rows, Kind string }{
	"pipeline-yaml": {Name: "yaml", Class: "input mb-3 h-72 font-mono text-xs", Rows: "24", Kind: "pipeline"},
	"sweep-params":  {Name: "params_yaml", Class: "input w-full font-mono text-xs", Rows: "6", Kind: "params"},
}

// handleUIAIGenerate generates a validated file. With a known target it
// returns that editor's textarea filled in (hx-swap outerHTML); otherwise a
// read-only result block for the assistant page.
func (s *server) handleUIAIGenerate(w http.ResponseWriter, r *http.Request, u store.User) {
	target := r.PostFormValue("target")
	tgt, inline := uiGenTargets[target]
	kind := r.PostFormValue("kind")
	current := ""
	if inline {
		kind = tgt.Kind
		current = r.PostFormValue(tgt.Name)
	}
	in := aiGenerateInput{
		Kind: kind, Description: strings.TrimSpace(r.PostFormValue("description")),
		Current: current, OverrideKind: r.PostFormValue("override_kind"),
	}
	var genErr error
	if in.Description == "" {
		genErr = errors.New("describe what the file should do")
	}
	if project := strings.TrimSpace(r.PostFormValue("project")); project != "" && genErr == nil {
		p, ok := s.uiProjectByName(w, u, project)
		if !ok {
			return
		}
		env, err := s.uiEnvByName(p, r.PostFormValue("env"))
		if err != nil {
			genErr = err
		} else {
			in.Project = &p
			in.Context = s.aiGenerateContext(p, env, in.Kind, r.PostFormValue("app"), in.OverrideKind)
		}
	}
	var res aiGenerateResult
	if genErr == nil {
		res, genErr = s.aiGenerate(r.Context(), u, in)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if inline {
		if genErr != nil {
			// Keep the user's editor untouched; show the error next to it.
			w.Header().Set("HX-Retarget", "#"+target+"-err")
			w.Header().Set("HX-Reswap", "innerHTML")
			_, _ = w.Write([]byte(template.HTMLEscapeString(aiErrorText(genErr))))
			return
		}
		data := map[string]any{"ID": target, "Name": tgt.Name, "Class": tgt.Class, "Rows": tgt.Rows, "Content": res.Content}
		if err := s.tmpl.ExecuteTemplate(w, "aigenfield", data); err != nil {
			log.Printf("render aigenfield: %v", err)
		}
		return
	}
	data := map[string]any{"Kind": in.Kind, "Content": res.Content, "Attempts": res.Attempts}
	if genErr != nil {
		data["Error"] = aiErrorText(genErr)
	}
	if err := s.tmpl.ExecuteTemplate(w, "aigenresult", data); err != nil {
		log.Printf("render aigenresult: %v", err)
	}
}

// uiProjectByName is uiProject for a project named in a form field rather
// than the path (same membership rule, same not-found answer).
func (s *server) uiProjectByName(w http.ResponseWriter, u store.User, name string) (store.Project, bool) {
	p, err := s.st.GetProject(name)
	if err == nil && u.Role != "admin" {
		if ok, merr := s.st.IsMember(p.ID, u.ID); merr != nil || !ok {
			err = store.ErrNotFound
		}
	}
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Project{}, false
	}
	return p, true
}

// uiEnvByName resolves an environment name ("" = the project's default).
func (s *server) uiEnvByName(p store.Project, name string) (store.Environment, error) {
	if name == "" {
		name = p.DefaultEnv
	}
	return s.st.GetEnvironment(p.ID, name)
}
