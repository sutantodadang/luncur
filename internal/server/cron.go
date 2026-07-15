package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// requireCronApp loads the app and answers kind_mismatch unless it is a
// kind=cron app. Shared by every cron-controls handler (pause/resume/
// trigger/run-history).
func (s *server) requireCronApp(w http.ResponseWriter, p store.Project, env store.Environment, name string) (store.App, bool) {
	a, ok := s.requireApp(w, p, env, name)
	if !ok {
		return store.App{}, false
	}
	if a.Kind != "cron" {
		writeError(w, http.StatusBadRequest, "kind_mismatch", "this action is only valid for cron apps")
		return store.App{}, false
	}
	return a, true
}

// pauseCron persists a cron app's suspended flag and re-applies its current
// state so a live CronJob's Spec.Suspend picks up the change immediately. If
// the app has no live deployment yet, syncApp is a no-op — the flag still
// persists and takes effect on the next deploy. Shared by the JSON API and
// the UI pause/resume buttons.
func (s *server) pauseCron(ctx context.Context, p store.Project, env store.Environment, a store.App, suspend bool) error {
	if err := s.st.SetAppSuspended(a.ID, suspend); err != nil {
		return err
	}
	a.Suspended = suspend
	return s.syncApp(ctx, p, env, a)
}

// handlePauseCron suspends a cron app's schedule.
func (s *server) handlePauseCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireCronApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if err := s.pauseCron(r.Context(), p, env, a, true); err != nil {
		log.Printf("pause cron %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suspended": true})
}

// handleResumeCron resumes a suspended cron app's schedule.
func (s *server) handleResumeCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireCronApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if err := s.pauseCron(r.Context(), p, env, a, false); err != nil {
		log.Printf("resume cron %s: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suspended": false})
}

// handleTriggerCron manually fires a cron app's CronJob ("run now"),
// building a one-off Job from its live JobTemplate.
func (s *server) handleTriggerCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnvWrite(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireCronApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if !s.requireKube(w) {
		return
	}

	d, err := s.st.LatestDeployment(a.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Status != "live") {
		writeError(w, http.StatusConflict, "not_deployed", errNotDeployed.Error())
		return
	}
	if err != nil {
		log.Printf("trigger cron %s: latest deployment: %v", a.Name, err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	job, err := s.kube.TriggerCronJob(r.Context(), env.Namespace, a.Name, time.Now().Unix())
	if err != nil {
		log.Printf("trigger cron %s: %v", a.Name, err)
		writeError(w, http.StatusBadGateway, "trigger_failed", "could not start run")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// handleCronRuns lists a cron app's recent Jobs (scheduled fires plus manual
// "run now" triggers), newest first.
func (s *server) handleCronRuns(w http.ResponseWriter, r *http.Request, u store.User) {
	p, env, ok := s.requireEnv(w, r, u, r.PathValue("project"), r.PathValue("env"))
	if !ok {
		return
	}
	a, ok := s.requireCronApp(w, p, env, r.PathValue("app"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}
	runs, err := s.kube.CronRuns(r.Context(), env.Namespace, a.Name)
	if err != nil {
		log.Printf("cron runs %s: %v", a.Name, err)
		writeError(w, http.StatusBadGateway, "kube_error", "could not list runs")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleUIPauseCron is handlePauseCron's UI twin: flash + redirect back to
// the app page instead of a JSON envelope.
func (s *server) handleUIPauseCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "cron" {
		http.Error(w, "this action is only valid for cron apps", http.StatusBadRequest)
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui pause cron %s: get environment: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.pauseCron(r.Context(), p, env, a, true); err != nil {
		log.Printf("ui pause cron %s: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "cron paused")
	uiRedirect(w, r, p, a)
}

// handleUIResumeCron is handleResumeCron's UI twin.
func (s *server) handleUIResumeCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "cron" {
		http.Error(w, "this action is only valid for cron apps", http.StatusBadRequest)
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui resume cron %s: get environment: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.pauseCron(r.Context(), p, env, a, false); err != nil {
		log.Printf("ui resume cron %s: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	flash(w, "ok", "cron resumed")
	uiRedirect(w, r, p, a)
}

// handleUITriggerCron is handleTriggerCron's UI twin.
func (s *server) handleUITriggerCron(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	if a.Kind != "cron" {
		http.Error(w, "this action is only valid for cron apps", http.StatusBadRequest)
		return
	}
	if a.Ejected {
		http.Error(w, errAppEjected.Error(), http.StatusConflict)
		return
	}
	if s.kube == nil {
		http.Error(w, "kubernetes is not configured", http.StatusServiceUnavailable)
		return
	}

	d, err := s.st.LatestDeployment(a.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Status != "live") {
		flash(w, "err", "app not deployed yet")
		uiRedirect(w, r, p, a)
		return
	}
	if err != nil {
		log.Printf("ui trigger cron %s: latest deployment: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui trigger cron %s: get environment: %v", a.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := s.kube.TriggerCronJob(r.Context(), env.Namespace, a.Name, time.Now().Unix()); err != nil {
		log.Printf("ui trigger cron %s: %v", a.Name, err)
		flash(w, "err", "could not start run")
		uiRedirect(w, r, p, a)
		return
	}
	flash(w, "ok", "run started")
	uiRedirect(w, r, p, a)
}
