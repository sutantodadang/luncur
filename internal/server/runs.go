package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/store"
)

// jobRunName is the per-run Job's cluster name. Run ids are SQLite
// autoincrement integers and app names are validated DNS labels, so the
// result is always a valid object name.
func jobRunName(app string, runID int64) string {
	return fmt.Sprintf("%s-run-%d", app, runID)
}

// runTimeout bounds how long a run's watcher waits before marking it
// failed; training jobs can be long, so this is generous. A package-level
// var so tests can lower it.
var runTimeout = 24 * time.Hour

// runWatchPoll is the run watcher's Job poll interval; a package-level var
// so tests can lower it.
var runWatchPoll = 5 * time.Second

func runJSON(app store.App, r store.JobRun) map[string]any {
	out := map[string]any{
		"id":         r.ID,
		"status":     r.Status,
		"job":        jobRunName(app.Name, r.ID),
		"started_at": r.StartedAt,
	}
	if r.ExitCode.Valid {
		out["exit_code"] = r.ExitCode.Int64
	}
	if r.FinishedAt.Valid {
		out["finished_at"] = r.FinishedAt.String
	}
	return out
}

// requireJobApp loads the app and answers kind_mismatch unless it is a
// kind=job app. Shared by every runs handler.
func (s *server) requireJobApp(w http.ResponseWriter, p store.Project, name string) (store.App, bool) {
	a, ok := s.requireApp(w, p, name)
	if !ok {
		return store.App{}, false
	}
	if a.Kind != "job" {
		writeError(w, http.StatusBadRequest, "kind_mismatch", "runs are only valid for job apps")
		return store.App{}, false
	}
	return a, true
}

// errNotDeployed is the sentinel returned by startRun when the app has no
// live deployment to run against — callers map it to their own "not
// deployed" response (409 JSON for the API, a redirect banner for the UI).
var errNotDeployed = errors.New("app has no live deployment; deploy an image first")

// errRunStartFailed is the sentinel wrapped by startRun when the run row was
// created but rendering/applying its Job to the cluster failed — callers map
// this to a gateway-style error (502 for the JSON API), distinct from an
// internal error before the run row existed (500).
var errRunStartFailed = errors.New("could not start run")

// startRun triggers one run of a kind=job app: a batch/v1 Job named
// <app>-run-<n> rendered against the latest live deployment's image. Shared
// by the JSON API (handleCreateRun) and the UI run-now button.
func (s *server) startRun(ctx context.Context, p store.Project, a store.App) (store.JobRun, error) {
	d, err := s.st.LatestDeployment(a.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.Status != "live") {
		return store.JobRun{}, errNotDeployed
	}
	if err != nil {
		return store.JobRun{}, fmt.Errorf("latest deployment: %w", err)
	}

	run, err := s.st.CreateJobRun(a.ID)
	if err != nil {
		return store.JobRun{}, fmt.Errorf("create job run: %w", err)
	}

	rendered, err := s.renderRun(p, a, d.ImageRef, run.ID)
	if err == nil {
		if err = s.kube.EnsureNamespace(ctx, p.Namespace); err == nil {
			err = s.kube.Apply(ctx, p.Namespace, rendered.Objects)
		}
	}
	if err != nil {
		log.Printf("start run %d: %v", run.ID, err)
		if e := s.st.FinishJobRun(run.ID, "failed", nil); e != nil {
			log.Printf("mark run %d failed: %v", run.ID, e)
		}
		return store.JobRun{}, fmt.Errorf("%w: %v", errRunStartFailed, err)
	}

	go s.watchRun(p, a, run)

	return run, nil
}

// handleCreateRun triggers one run of a kind=job app via the JSON API.
func (s *server) handleCreateRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if !s.requireKube(w) {
		return
	}

	run, err := s.startRun(r.Context(), p, a)
	switch {
	case errors.Is(err, errNotDeployed):
		writeError(w, http.StatusConflict, "not_deployed", err.Error())
		return
	case errors.Is(err, errRunStartFailed):
		writeError(w, http.StatusBadGateway, "run_failed", "could not start run")
		return
	case err != nil:
		log.Printf("create run: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	writeJSON(w, http.StatusAccepted, runJSON(a, run))
}

// watchRun waits for a run's Job to finish and records the outcome plus the
// pod's exit code.
func (s *server) watchRun(p store.Project, a store.App, run store.JobRun) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	name := jobRunName(a.Name, run.ID)
	ok, err := s.kube.WaitJob(ctx, p.Namespace, name, runWatchPoll)
	status := "succeeded"
	if err != nil || !ok {
		status = "failed"
	}
	var exitCode *int64
	if code, found, err := s.kube.JobExitCode(ctx, p.Namespace, name); err == nil && found {
		exitCode = &code
	}
	if err := s.st.FinishJobRun(run.ID, status, exitCode); err != nil {
		log.Printf("finish run %d: %v", run.ID, err)
	}
}

func (s *server) handleListRuns(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	runs, err := s.st.ListJobRuns(a.ID)
	if err != nil {
		log.Printf("list runs: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, runJSON(a, run))
	}
	writeJSON(w, http.StatusOK, out)
}

// requireRun parses and loads a run by id, verifying it belongs to app a.
func (s *server) requireRun(w http.ResponseWriter, a store.App, idStr string) (store.JobRun, bool) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid run id")
		return store.JobRun{}, false
	}
	run, err := s.st.GetJobRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such run")
		return store.JobRun{}, false
	}
	if err != nil {
		log.Printf("get run: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.JobRun{}, false
	}
	return run, true
}

func (s *server) handleGetRun(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	run, ok := s.requireRun(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, runJSON(a, run))
}

// handleRunLogs streams a run's pod logs as SSE (follow=1 keeps the stream
// open while the run is still producing output). Same shape as
// handleRuntimeLogs, but scoped to the run's Job pods.
func (s *server) handleRunLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	run, ok := s.requireRun(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.requireKube(w) {
		return
	}

	follow := r.URL.Query().Get("follow") == "1"
	pods, err := s.kube.JobPods(r.Context(), p.Namespace, jobRunName(a.Name, run.ID))
	if err != nil {
		log.Printf("list run pods: %v", err)
		writeError(w, http.StatusBadGateway, "kube_error", "could not list pods")
		return
	}
	if len(pods) == 0 {
		writeError(w, http.StatusNotFound, "no_pods", "run has no pods (not scheduled yet, or cleaned up)")
		return
	}

	fl, ok := sseStart(w)
	if !ok {
		return
	}
	lines := make(chan string, 64)
	send := func(line string) bool {
		select {
		case lines <- line:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			rc, err := s.kube.PodLogStream(r.Context(), p.Namespace, pod, follow)
			if err != nil {
				send("[" + pod + "] error: " + err.Error())
				return
			}
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				if !send("[" + pod + "] " + sc.Text()) {
					return
				}
			}
		}(pod)
	}
	go func() { wg.Wait(); close(lines) }()

	for {
		select {
		case line, more := <-lines:
			if !more {
				sseEnd(w, fl, "eof")
				return
			}
			sseData(w, fl, line)
		case <-r.Context().Done():
			return
		}
	}
}
