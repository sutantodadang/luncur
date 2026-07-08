package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/store"
)

// uiTrialRow is the Sweeps card's per-trial table row. ChipClass follows the
// same "chip chip-ok/chip-warn/chip-bad/chip-muted" idiom the Pods card
// already uses for its Phase column (app.html) rather than a new status-*
// CSS class — pending/running/failed already have status-* rules, but done/
// pruned don't, so reusing the existing chip variants (already Tailwind-
// safelisted) needs no app.css regen.
type uiTrialRow struct {
	ID            string
	State         string
	ChipClass     string
	ParamsCompact string
	MetricValue   string // "" when not yet recorded
	RunID         string // "" when not yet launched
	Best          bool
}

// uiSweepRow is the Sweeps card's history-table row (one per sweep, newest
// first) — id/status/metric plus a Done/Total progress count and the best
// recorded value, mirroring sweepListJSON's summary.
type uiSweepRow struct {
	ID        string
	Status    string
	ChipClass string
	Metric    string
	Direction string
	Done      int
	Total     int
	Best      string
}

// uiSweepData is the most recent sweep's live detail — the "sweeptrials"
// fragment's view model, embedded in the initial page render and reused
// verbatim by the 15s poll (handleUISweepTrials). ProjectName/AppName/CSRF
// are carried on the struct itself (rather than relied on from an outer
// template scope) so the same "sweeptrials" block renders identically
// whether it's included inline or executed standalone as a fragment.
type uiSweepData struct {
	ProjectName string
	AppName     string
	CSRF        string
	ID          string
	Status      string
	ChipClass   string
	Metric      string
	Direction   string
	EarlyStop   bool
	Warning     string
	Done        int
	Total       int
	BestValue   string
	Trials      []uiTrialRow
	// Polling is true while Status == "running" — the fragment's own hx-get
	// re-fetch attributes are only emitted then (see "sweeptrials" in
	// app.html), so htmx stops re-polling on its own once the sweep lands on
	// a terminal status, same self-terminating idiom as "statuschip".
	Polling bool
}

// sweepChipClass maps a sweep's status to a chip color variant: done reads
// as success, running as in-progress, failed as an error, and stopped
// (deliberate, not an error) as muted.
func sweepChipClass(status string) string {
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

// trialChipClass maps a trial's state the same way: done succeeded, running/
// pending are in-progress-or-queued, failed is an error, pruned was killed
// on purpose (muted, not an error — see sweepPruneTrial's doc comment).
func trialChipClass(state string) string {
	switch state {
	case "done":
		return "chip-ok"
	case "running", "pending":
		return "chip-warn"
	case "failed":
		return "chip-bad"
	default: // pruned
		return "chip-muted"
	}
}

// paramsCompact renders a trial's params_json as CLI-style "k=v k2=v2"
// (sorted by key for a stable render), matching Task 6's CLI table.
func paramsCompact(paramsJSON string) string {
	var params map[string]string
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, " ")
}

// uiSweepTrialRows builds the trial table's rows, marking bestID's row.
func uiSweepTrialRows(trials []store.SweepTrial, bestID string) []uiTrialRow {
	rows := make([]uiTrialRow, 0, len(trials))
	for _, tr := range trials {
		row := uiTrialRow{
			ID: tr.ID, State: tr.State, ChipClass: trialChipClass(tr.State),
			ParamsCompact: paramsCompact(tr.ParamsJSON), Best: tr.ID == bestID,
		}
		if tr.RunID.Valid {
			row.RunID = strconv.FormatInt(tr.RunID.Int64, 10)
		}
		if tr.MetricValue.Valid {
			row.MetricValue = strconv.FormatFloat(tr.MetricValue.Float64, 'g', -1, 64)
		}
		rows = append(rows, row)
	}
	return rows
}

// sweepDoneTotal counts trials past a terminal state (done/failed/pruned)
// against the total, for the Sweeps card's "N/M" progress columns.
func sweepDoneTotal(trials []store.SweepTrial) (done, total int) {
	for _, tr := range trials {
		if tr.State == "done" || tr.State == "failed" || tr.State == "pruned" {
			done++
		}
	}
	return done, len(trials)
}

// uiSweepRowFrom builds one history-table row.
func uiSweepRowFrom(sw store.Sweep, trials []store.SweepTrial) uiSweepRow {
	_, best := sweepSummary(sw, trials)
	bestVal := ""
	if best != nil {
		bestVal = strconv.FormatFloat(best.MetricValue.Float64, 'g', -1, 64)
	}
	done, total := sweepDoneTotal(trials)
	return uiSweepRow{
		ID: sw.ID, Status: sw.Status, ChipClass: sweepChipClass(sw.Status),
		Metric: sw.Metric, Direction: sw.Direction, Done: done, Total: total, Best: bestVal,
	}
}

// uiSweepDataFrom builds the current-sweep detail view (ProjectName/AppName/
// CSRF are filled in by the caller — renderAppDetail for the initial page,
// handleUISweepTrials for the poll fragment).
func uiSweepDataFrom(sw store.Sweep, trials []store.SweepTrial) uiSweepData {
	_, best := sweepSummary(sw, trials)
	bestID, bestVal := "", ""
	if best != nil {
		bestID = best.ID
		bestVal = strconv.FormatFloat(best.MetricValue.Float64, 'g', -1, 64)
	}
	done, total := sweepDoneTotal(trials)
	return uiSweepData{
		ID: sw.ID, Status: sw.Status, ChipClass: sweepChipClass(sw.Status),
		Metric: sw.Metric, Direction: sw.Direction, EarlyStop: sw.EarlyStop, Warning: sw.Warning,
		Done: done, Total: total, BestValue: bestVal,
		Trials:  uiSweepTrialRows(trials, bestID),
		Polling: sw.Status == "running",
	}
}

// handleUISweepCreate is startSweep's UI twin: same shared core as
// handleCreateSweep, form POST instead of a JSON body, redirect back to the
// app page (?err= on failure) instead of a 202 body — same idiom as
// handleUIRunCreate.
func (s *server) handleUISweepCreate(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "job" {
		http.Error(w, "sweeps are only valid for job apps", http.StatusBadRequest)
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	req, err := parseUISweepRequest(r)
	if err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	var createdBy sql.NullInt64
	if u.ID != 0 {
		createdBy = sql.NullInt64{Int64: u.ID, Valid: true}
	}
	if _, _, _, err := s.startSweep(a, req, createdBy); err != nil {
		if errors.Is(err, errNotDeployed) || errors.Is(err, errBadSweepRequest) {
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		log.Printf("ui start sweep: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "sweep started")
	uiRedirect(w, r, p, a)
}

// parseUISweepRequest reads the start-sweep form into startSweep's request
// shape; malformed numeric fields fail here (before startSweep's own
// validation runs) with the same "invalid X" wording handleUIScale uses.
func parseUISweepRequest(r *http.Request) (sweepCreateRequest, error) {
	maxTrials, err := strconv.Atoi(r.PostFormValue("max_trials"))
	if err != nil {
		return sweepCreateRequest{}, errors.New("invalid max_trials")
	}
	parallel, err := strconv.Atoi(r.PostFormValue("parallel"))
	if err != nil {
		return sweepCreateRequest{}, errors.New("invalid parallel")
	}
	nodes := 0
	if v := r.PostFormValue("nodes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return sweepCreateRequest{}, errors.New("invalid nodes")
		}
		nodes = n
	}
	return sweepCreateRequest{
		ParamsYAML: r.PostFormValue("params_yaml"),
		Metric:     r.PostFormValue("metric"),
		Direction:  r.PostFormValue("direction"),
		MaxTrials:  maxTrials,
		Parallel:   parallel,
		EarlyStop:  r.PostFormValue("early_stop") != "",
		Nodes:      nodes,
		Framework:  r.PostFormValue("framework"),
	}, nil
}

// uiRequireSweep loads a sweep by id, 404ing (plain text, UI idiom) if it
// doesn't belong to app a.
func (s *server) uiRequireSweep(w http.ResponseWriter, a store.App, id string) (store.Sweep, bool) {
	sw, err := s.st.GetSweep(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sw.AppID != a.ID) {
		http.Error(w, "no such sweep", http.StatusNotFound)
		return store.Sweep{}, false
	}
	if err != nil {
		log.Printf("ui get sweep: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return store.Sweep{}, false
	}
	return sw, true
}

// handleUISweepStop is stopSweep's UI twin: same idempotent core as
// handleStopSweep, redirect back to the app page instead of a JSON body.
func (s *server) handleUISweepStop(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	sw, ok := s.uiRequireSweep(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	if sw.Status == "running" {
		if err := s.stopSweep(r.Context(), sw, a, p); err != nil {
			log.Printf("ui stop sweep %s: %v", sw.ID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	flash(w, "ok", "sweep stopped")
	uiRedirect(w, r, p, a)
}

// handleUISweepTrials is the Sweeps card's polling fragment: the
// "sweeptrials" template block re-fetches every 15s while the sweep is
// running (same self-terminating hx-trigger idiom as "statuschip") —
// rendering only the trial table, best-trial highlight, and stop control,
// not the full page.
func (s *server) handleUISweepTrials(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	sw, ok := s.uiRequireSweep(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	trials, err := s.st.ListTrials(sw.ID)
	if err != nil {
		log.Printf("ui list trials: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := uiSweepDataFrom(sw, trials)
	data.ProjectName, data.AppName, data.CSRF = p.Name, a.Name, s.csrf(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "sweeptrials", data); err != nil {
		log.Printf("render sweeptrials: %v", err)
	}
}
