package server

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// pipelineRunChipClass maps a run's status the same way sweepChipClass maps
// a sweep's: done reads success, running in-progress, failed an error, and
// stopped (deliberate, not an error) muted.
func pipelineRunChipClass(status string) string {
	switch status {
	case "done":
		return "chip-ok"
	case "running":
		return "chip-warn"
	case "failed":
		return "chip-bad"
	default: // stopped
		return "chip-muted"
	}
}

// pipelineStepChipClass maps a step row's state the same way trialChipClass
// maps a trial's: done succeeded, running in-progress, failed an error,
// pending/skipped muted (queued, or deliberately not run — fail-fast skip).
func pipelineStepChipClass(state string) string {
	switch state {
	case "done":
		return "chip-ok"
	case "running":
		return "chip-warn"
	case "failed":
		return "chip-bad"
	default: // pending, skipped
		return "chip-muted"
	}
}

// uiPipelineCardRow is apps.html's Pipelines-card row: one per project
// pipeline, with its newest run's status folded into a chip (HasRun false =
// "never run", the table's own muted chip case).
type uiPipelineCardRow struct {
	Name          string
	Engine        string
	Cron          string
	HasRun        bool
	LastRunStatus string
	LastRunChip   string
}

// uiPipelineCardRows builds the Pipelines card's table rows for a project,
// normalizing an unset Engine to "native" for display (mirrors pipeline ls's
// CLI table — pipelinecmd.go's pipelineListCmd).
func (s *server) uiPipelineCardRows(p store.Project) ([]uiPipelineCardRow, error) {
	pipelines, err := s.st.ListPipelines(p.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]uiPipelineCardRow, 0, len(pipelines))
	for _, pl := range pipelines {
		engine := pl.Engine
		if engine == "" {
			engine = "native"
		}
		row := uiPipelineCardRow{Name: pl.Name, Engine: engine, Cron: pl.Cron}
		runs, err := s.st.ListPipelineRuns(pl.ID)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			row.HasRun = true
			row.LastRunStatus = runs[0].Status // ListPipelineRuns is newest first
			row.LastRunChip = pipelineRunChipClass(runs[0].Status)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// uiPipelineRunRow is the detail page's run-history table row.
type uiPipelineRunRow struct {
	ID         string
	Trigger    string
	Status     string
	ChipClass  string
	StartedAt  string
	FinishedAt string
}

func uiPipelineRunRows(runs []store.PipelineRun) []uiPipelineRunRow {
	rows := make([]uiPipelineRunRow, 0, len(runs))
	for _, run := range runs {
		finished := ""
		if run.FinishedAt.Valid {
			finished = run.FinishedAt.String
		}
		rows = append(rows, uiPipelineRunRow{
			ID: run.ID, Trigger: run.Trigger, Status: run.Status,
			ChipClass: pipelineRunChipClass(run.Status),
			StartedAt: run.StartedAt, FinishedAt: finished,
		})
	}
	return rows
}

// uiPipelineStepRow is the run-detail step table's row (topo order, same as
// ListRunSteps).
type uiPipelineStepRow struct {
	Name      string
	Kind      string
	State     string
	ChipClass string
	Attempt   int
	Detail    string
	Duration  string
}

// uiPipelineStepDuration is pipelineStepDuration's (internal/cli) UI twin:
// best-effort elapsed wall time for one step row — "-" for a step that never
// launched (pending/skipped) or a timestamp that fails to parse.
func uiPipelineStepDuration(st store.PipelineRunStep) string {
	if !st.StartedAt.Valid || st.StartedAt.String == "" {
		return "-"
	}
	started, err := time.Parse(pipelineStartedAtLayout, st.StartedAt.String)
	if err != nil {
		return "-"
	}
	end := time.Now().UTC()
	if st.FinishedAt.Valid {
		if finished, err := time.Parse(pipelineStartedAtLayout, st.FinishedAt.String); err == nil {
			end = finished
		}
	}
	return end.Sub(started).Round(time.Second).String()
}

func uiPipelineStepRows(steps []store.PipelineRunStep) []uiPipelineStepRow {
	rows := make([]uiPipelineStepRow, 0, len(steps))
	for _, st := range steps {
		rows = append(rows, uiPipelineStepRow{
			Name: st.Name, Kind: st.Kind, State: st.State,
			ChipClass: pipelineStepChipClass(st.State),
			Attempt:   st.Attempt, Detail: st.Detail,
			Duration: uiPipelineStepDuration(st),
		})
	}
	return rows
}

// uiPipelineRunData is the "pipelinesteps" fragment's view model — the
// detail page's current-run section and the standalone poll endpoint's
// response share this exact shape (sweeptrials/uiSweepData's pattern).
// ProjectName/PipelineName/CSRF are carried on the struct itself so the
// fragment renders identically whether included inline or executed
// standalone.
type uiPipelineRunData struct {
	ProjectName  string
	PipelineName string
	CSRF         string
	ID           string
	Status       string
	ChipClass    string
	Trigger      string
	Warning      string
	Steps        []uiPipelineStepRow
	// Polling is true while Status == "running" — see "pipelinesteps" in
	// pipeline.html for the self-terminating hx-get idiom this drives
	// (same as "sweeptrials"/"statuschip").
	Polling bool
}

func uiPipelineRunDataFrom(run store.PipelineRun, steps []store.PipelineRunStep) uiPipelineRunData {
	return uiPipelineRunData{
		ID: run.ID, Status: run.Status, ChipClass: pipelineRunChipClass(run.Status),
		Trigger: run.Trigger, Warning: run.Warning,
		Steps:   uiPipelineStepRows(steps),
		Polling: run.Status == "running",
	}
}

// uiRequirePipeline is requirePipeline's UI twin: 404s plain text instead of
// a JSON envelope.
func (s *server) uiRequirePipeline(w http.ResponseWriter, p store.Project, name string) (store.Pipeline, bool) {
	pl, err := s.st.GetPipeline(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.Pipeline{}, false
	}
	if err != nil {
		log.Printf("ui get pipeline: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Pipeline{}, false
	}
	return pl, true
}

// uiRequirePipelineRun is uiRequireSweep's pipeline twin: loads a run by id,
// 404ing (plain text) if it doesn't belong to pipeline pl.
func (s *server) uiRequirePipelineRun(w http.ResponseWriter, pl store.Pipeline, id string) (store.PipelineRun, bool) {
	run, err := s.st.GetPipelineRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.PipelineID != pl.ID) {
		http.Error(w, "not found", http.StatusNotFound)
		return store.PipelineRun{}, false
	}
	if err != nil {
		log.Printf("ui get pipeline run: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.PipelineRun{}, false
	}
	return run, true
}

// pipelineUIRedirectErr maps createPipeline/updatePipeline/startPipelineRun's
// sentinel errors (the same ones writePipelineRequestError maps to JSON
// codes) to a friendly ?err= redirect using the existing UI error-banner
// mechanism — including errArgoNotInstalled, so an argo run started from the
// UI with the CRD missing surfaces "luncur argo install" with no special
// casing. Returns true if it wrote the redirect, so the caller can just
// return; any other error is the caller's to log + 500.
func pipelineUIRedirectErr(w http.ResponseWriter, r *http.Request, redirectURL string, err error) bool {
	switch {
	case errors.Is(err, errArgoNotInstalled),
		errors.Is(err, errPipelineEngineUnavailable),
		errors.Is(err, errPipelineAppMismatch),
		errors.Is(err, errBadPipelineRequest):
		http.Redirect(w, r, redirectURL+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return true
	default:
		return false
	}
}

// handleUIPipeline is the pipeline detail page: the yaml/cron/engine editor,
// the webhook rotate control, the run-history table, and — for the most
// recent run only — its live step-table fragment (Sweep card's
// most-recent-only convention, renderAppDetail's doc comment).
func (s *server) handleUIPipeline(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}

	runs, err := s.st.ListPipelineRuns(pl.ID)
	if err != nil {
		log.Printf("ui pipeline runs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	csrf := s.csrf(w, r)
	var currentRun *uiPipelineRunData
	if len(runs) > 0 {
		steps, err := s.st.ListRunSteps(runs[0].ID)
		if err != nil {
			log.Printf("ui pipeline run steps: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		d := uiPipelineRunDataFrom(runs[0], steps)
		d.ProjectName, d.PipelineName, d.CSRF = p.Name, pl.Name, csrf
		currentRun = &d
	}

	engine := pl.Engine
	if engine == "" {
		engine = "native"
	}

	s.renderPage(w, "pipeline.html", map[string]any{
		"User": u, "Project": p, "Pipeline": pl, "Engine": engine,
		"Runs": uiPipelineRunRows(runs), "CurrentRun": currentRun,
		"WebhookEnabled": pl.WebhookSecret != nil,
		"WebhookURL":     "http://" + r.Host + pipelineWebhookPath(pl.ID),
		"Warning":        firstNonEmpty(r.URL.Query().Get("warn"), r.URL.Query().Get("err")),
		"CSRF":           csrf, "IsAdmin": u.Role == "admin",
	})
}

// handleUIPipelineCreate is createPipeline's UI twin: same shared
// compile+validate+persist core the API's handleCreatePipeline uses, form
// POST instead of a JSON body, redirect to the new pipeline's detail page on
// success or back to the project page (?err=) on a known validation failure
// — same idiom as handleUISweepCreate.
func (s *server) handleUIPipelineCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var createdBy sql.NullInt64
	if u.ID != 0 {
		createdBy = sql.NullInt64{Int64: u.ID, Valid: true}
	}
	pl, err := s.createPipeline(p, r.PostFormValue("name"), r.PostFormValue("yaml"), r.PostFormValue("engine"), r.PostFormValue("cron"), createdBy)
	if err != nil {
		if pipelineUIRedirectErr(w, r, "/ui/projects/"+p.Name, err) {
			return
		}
		log.Printf("ui create pipeline: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "pipeline created")
	http.Redirect(w, r, "/ui/projects/"+p.Name+"/pipelines/"+pl.Name, http.StatusSeeOther)
}

// handleUIPipelineUpdate is updatePipeline's UI twin: the detail page's
// editor always submits yaml/cron/engine together (unlike the JSON API's
// optional-pointer fields), so all three are always re-validated together.
func (s *server) handleUIPipelineUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	yamlStr := r.PostFormValue("yaml")
	cron := r.PostFormValue("cron")
	engine := r.PostFormValue("engine")
	redirectURL := "/ui/projects/" + p.Name + "/pipelines/" + pl.Name
	if _, err := s.updatePipeline(p, pl, &yamlStr, &engine, &cron); err != nil {
		if pipelineUIRedirectErr(w, r, redirectURL, err) {
			return
		}
		log.Printf("ui update pipeline: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "pipeline saved")
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// handleUIPipelineRun is startPipelineRun's UI twin (trigger "manual"): same
// shared core the API's handleCreatePipelineRun and the cron/webhook
// triggers use, redirect back to the detail page instead of a 202 body —
// same idiom as handleUIRunCreate/handleUISweepCreate. An argo run with the
// CRD missing (errArgoNotInstalled) redirects with a readable message via
// pipelineUIRedirectErr, same as any other pipeline validation failure — no
// special-casing (spec's "surface via the existing htmx error banner").
func (s *server) handleUIPipelineRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	redirectURL := "/ui/projects/" + p.Name + "/pipelines/" + pl.Name
	if _, _, err := s.startPipelineRun(r.Context(), pl, "manual"); err != nil {
		if pipelineUIRedirectErr(w, r, redirectURL, err) {
			return
		}
		log.Printf("ui start pipeline run: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "run started")
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// handleUIPipelineRunStop is stopPipelineRun's UI twin: same idempotent core
// as handleStopPipelineRun (API), redirect back to the detail page instead
// of a JSON body — same idiom as handleUISweepStop.
func (s *server) handleUIPipelineRunStop(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	run, ok := s.uiRequirePipelineRun(w, pl, r.PathValue("id"))
	if !ok {
		return
	}
	if run.Status == "running" {
		if err := s.stopPipelineRun(r.Context(), run); err != nil {
			log.Printf("ui stop pipeline run %s: %v", run.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	flash(w, "ok", "run stopped")
	http.Redirect(w, r, "/ui/projects/"+p.Name+"/pipelines/"+pl.Name, http.StatusSeeOther)
}

// handleUIPipelineRunSteps is the detail page's polling fragment endpoint:
// the "pipelinesteps" template block re-fetches every 15s while the run is
// running (same self-terminating hx-trigger idiom as "sweeptrials"),
// rendering only the step table, warning banner, and stop control — not the
// full page.
func (s *server) handleUIPipelineRunSteps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	run, ok := s.uiRequirePipelineRun(w, pl, r.PathValue("id"))
	if !ok {
		return
	}
	steps, err := s.st.ListRunSteps(run.ID)
	if err != nil {
		log.Printf("ui list pipeline run steps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := uiPipelineRunDataFrom(run, steps)
	data.ProjectName, data.PipelineName, data.CSRF = p.Name, pl.Name, s.csrf(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "pipelinesteps", data); err != nil {
		log.Printf("render pipelinesteps: %v", err)
	}
}

// handleUIPipelineWebhookSecret is generatePipelineWebhookSecret's UI twin:
// same core the API's handleGeneratePipelineWebhookSecret uses (always
// rotates), but renders a small standalone fragment carrying the freshly
// generated URL+secret instead of a JSON body or a full-page redirect — the
// secret must never appear in a URL/query string, and the detail page's own
// GET render (handleUIPipeline) never re-renders it, so this fragment is the
// only place it's ever shown.
func (s *server) handleUIPipelineWebhookSecret(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	pl, ok := s.uiRequirePipeline(w, p, r.PathValue("name"))
	if !ok {
		return
	}
	secretHex, err := s.generatePipelineWebhookSecret(pl)
	if err != nil {
		if errors.Is(err, errSealerUnavailable) {
			http.Error(w, errSealerUnavailable.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("ui generate pipeline webhook secret: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"WebhookURL":        "http://" + r.Host + pipelineWebhookPath(pl.ID),
		"WebhookSecretOnce": secretHex,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "pipelinewebhooksecret", data); err != nil {
		log.Printf("render pipelinewebhooksecret: %v", err)
	}
}
