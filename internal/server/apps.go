package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// errKubeUnavailable is the shared cores' sentinel for "this action can't
// be applied because no kube client is configured". Callers (the JSON API
// and the UI) each translate it into their own response shape.
var errKubeUnavailable = errors.New("kubernetes is not configured")

// deployIDPattern matches store.NewID's shape: a 12-char lowercase base-36
// nanoid. Anywhere a deployment id arrives from a client (a path segment or
// a form/JSON field) it's checked against this before ever reaching the DB
// — the same guard an int64 ParseInt used to provide for free.
var deployIDPattern = regexp.MustCompile(`^[a-z0-9]{12}$`)

func validDeployID(id string) bool { return deployIDPattern.MatchString(id) }

// errBuildUnavailable is deployGitApp's sentinel for "no build source is
// configured" (the server was started without a data directory).
var errBuildUnavailable = errors.New("server has no data directory configured")

// scaleReplicasError wraps a SetReplicas validation failure so callers can
// tell it apart from scaleApp's other (internal) failure modes and answer
// with a caller-fault status code.
type scaleReplicasError struct{ err error }

func (e *scaleReplicasError) Error() string { return e.err.Error() }
func (e *scaleReplicasError) Unwrap() error { return e.err }

// kindMismatchError wraps a validation failure caused by an operation not
// being supported for the app's kind (e.g. scaling replicas on a cron app,
// adding a domain to a worker). Callers map it to 400 kind_mismatch.
type kindMismatchError struct{ err error }

func (e *kindMismatchError) Error() string { return e.err.Error() }
func (e *kindMismatchError) Unwrap() error { return e.err }

// appJSON builds the JSON API's app representation. p is needed for internal
// apps' cluster-DNS URL (which is namespace-qualified); non-internal apps
// ignore it beyond that. An internal app gets "internal_url" instead of
// "url" — its public sslip.io hostname resolves nowhere useful, since no
// Ingress was ever rendered for it.
func (s *server) appJSON(p store.Project, a store.App) map[string]any {
	out := map[string]any{
		"id":              a.ID,
		"name":            a.Name,
		"port":            a.Port,
		"replicas":        a.Replicas,
		"health_path":     a.HealthPath,
		"kind":            a.Kind,
		"schedule":        a.Schedule,
		"webhook_enabled": a.WebhookSecret != nil,
		"internal":        a.Internal,
		"gpu":             a.GPUCount,
		"s3_env":          a.InjectS3,
	}
	if a.Kind == "model" {
		out["model_source"] = a.ModelSource
		out["runtime"] = a.Runtime
	}
	if a.Internal {
		out["internal_url"] = internalURLFor(a.Name, p.Namespace)
	} else {
		out["url"] = s.appURL(a)
	}
	if a.AutoMin > 0 {
		out["autoscale"] = map[string]any{"min": a.AutoMin, "max": a.AutoMax, "cpu": a.AutoCPU}
	}
	return out
}

// validateInternalKind enforces that internal=true only applies to web apps:
// worker/cron kinds already render no Service, so there is nothing for
// "internal" to mean for them. kind is the raw request field ("" defaults to
// web, matching store.normalizeAppKind). Shared by the JSON API
// (handleCreateApp) and the UI (handleUICreateApp) so both reject before the
// app row is ever created.
func validateInternalKind(internal bool, kind string) error {
	if internal && kind != "" && kind != "web" {
		return fmt.Errorf("internal only applies to web apps")
	}
	return nil
}

// requireApp loads an app within a project by name. Writes the error
// response and returns ok=false on failure.
func (s *server) requireApp(w http.ResponseWriter, p store.Project, name string) (store.App, bool) {
	a, err := s.st.GetApp(p.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such app")
		return store.App{}, false
	}
	if err != nil {
		log.Printf("get app: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.App{}, false
	}
	return a, true
}

func (s *server) handleCreateApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	var req struct {
		Name      string `json:"name"`
		Port      int    `json:"port"`
		GitURL    string `json:"git_url"`
		GitBranch string `json:"git_branch"`
		Kind      string `json:"kind"`
		Schedule  string `json:"schedule"`
		BuildPath string `json:"build_path"`
		Internal  bool   `json:"internal"`
		GPU       int64  `json:"gpu"`
		// Model apps only.
		ModelSource string `json:"model_source"`
		Runtime     string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	buildPath, err := validBuildPath(req.BuildPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "build_path: "+err.Error())
		return
	}
	if err := validateInternalKind(req.Internal, req.Kind); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// New apps start at 1 replica, so the delta is just req.GPU; scale-ups
	// are validated separately in scaleApp.
	if req.GPU > 0 {
		if err := s.validateGPUBudget(p, req.GPU); err != nil {
			writeError(w, http.StatusBadRequest, "gpu_budget", err.Error())
			return
		}
	}
	var modelRT render.ModelRuntimeInfo
	if req.Kind == "model" {
		if req.GitURL != "" {
			writeError(w, http.StatusBadRequest, "bad_request", "model apps do not take a git url")
			return
		}
		// Resolve now so a bad source/runtime combination fails before the
		// app row exists.
		modelRT, err = render.ResolveModelRuntime(req.ModelSource, req.Runtime, req.GPU)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	var a store.App
	switch {
	case req.Kind == "model":
		a, err = s.st.CreateModelApp(p.ID, req.Name, req.ModelSource, req.Runtime)
	case req.GitURL != "":
		a, err = s.st.CreateGitApp(p.ID, req.Name, req.Port, req.GitURL, req.GitBranch, req.Kind, req.Schedule)
	default:
		a, err = s.st.CreateApp(p.ID, req.Name, req.Port, req.Kind, req.Schedule)
	}
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			writeError(w, http.StatusConflict, "app_exists", "app already exists")
			return
		}
		if strings.HasPrefix(err.Error(), "insert app:") {
			log.Printf("create app: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if buildPath != "" {
		if err := s.st.SetBuildPath(a.ID, buildPath); err != nil {
			log.Printf("set build path: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		a.BuildPath = buildPath
	}
	if req.Internal {
		if err := s.st.SetInternal(a.ID, true); err != nil {
			log.Printf("set internal: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		a.Internal = true
	}
	if req.GPU != 0 {
		if err := s.st.SetGPU(a.ID, req.GPU); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "gpu: "+err.Error())
			return
		}
		a.GPUCount = req.GPU
	}
	out := s.appJSON(p, a)
	// Built-in runtime model apps deploy themselves at create: the runtime
	// image is known, so within one apply the endpoint is on its way up.
	if a.Kind == "model" && modelRT.Name != "custom" && s.kube != nil {
		d, err := s.st.CreateDeployment(a.ID, "deploying", modelRT.Image, u.ID)
		if err != nil {
			log.Printf("create model deployment: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := s.applyImageDeploy(ctx, p, a, d, modelRT.Image); err != nil {
				log.Printf("deploy model %s: %v", a.Name, err)
			}
		}()
		out["status"] = "deploying"
		out["deployment_id"] = d.ID
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) handleListApps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	list, err := s.st.ListApps(p.ID)
	if err != nil {
		log.Printf("list apps: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, s.appJSON(p, a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleGetApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	out := s.appJSON(p, a)
	d, err := s.st.LatestDeployment(a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		out["status"] = "never_deployed"
		out["image"] = ""
	case err != nil:
		log.Printf("latest deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	default:
		out["status"] = d.Status
		out["image"] = d.ImageRef
		out["seq"] = d.Seq
	}
	writeJSON(w, http.StatusOK, out)
}

// destroyApp is handleDeleteApp's and handleUIAppDestroy's shared core: an
// ejected app's kube objects are no longer luncur's to touch — only the DB
// row comes out. A non-ejected app deletes its kube objects first; the
// caller must have already confirmed kube is configured.
func (s *server) destroyApp(ctx context.Context, p store.Project, a store.App) error {
	if !a.Ejected {
		if err := s.kube.DeleteAppObjects(ctx, p.Namespace, a.Name); err != nil {
			return err
		}
	}
	return s.st.DeleteApp(a.ID)
}

func (s *server) handleDeleteApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	// Non-ejected apps keep the original behavior (kube required, objects
	// deleted before the row); an ejected app skips straight to destroyApp.
	if !a.Ejected && !s.requireKube(w) {
		return
	}
	if err := s.destroyApp(r.Context(), p, a); err != nil {
		log.Printf("delete app: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeployApp dispatches on the request shape: a multipart body carries
// a source tarball for an async build (Job -> wait -> apply); a JSON body
// with a non-empty "image" is the synchronous prebuilt-image path (kept
// byte-for-byte compatible with the pre-build-pipeline behavior); a git-source
// app (App.SourceType == "git") with neither triggers an async build cloning
// from its configured repo; anything else is a bad request.
func (s *server) handleDeployApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	if s.refuseEjected(w, a) {
		return
	}
	if !s.requireKube(w) {
		return
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if a.Kind == "model" {
			writeError(w, http.StatusBadRequest, "kind_mismatch", "model apps do not build from source; runtime custom deploys a prebuilt image")
			return
		}
		if s.src == nil {
			writeError(w, http.StatusServiceUnavailable, "build_unavailable", "server has no data directory configured")
			return
		}

		// 256 MiB: a source tarball larger than that is a mistake, not an app.
		r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid multipart body")
			return
		}
		part, _, err := r.FormFile("source")
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "missing source file")
			return
		}
		defer part.Close()

		d, err := s.st.CreateDeployment(a.ID, "building", "", u.ID)
		if err != nil {
			log.Printf("create deployment: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if _, err := s.src.Save(d.ID, part); err != nil {
			log.Printf("save source tarball: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}

		s.startBuild(p, a, d)

		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_id": d.ID,
			"seq":           d.Seq,
			"status":        "building",
		})
		return
	}

	var req struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if a.Kind == "model" {
		rt, err := render.ResolveModelRuntime(a.ModelSource, a.Runtime, a.GPUCount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		image := req.Image
		switch {
		case rt.Name == "custom" && image == "":
			writeError(w, http.StatusBadRequest, "bad_request", "runtime custom deploys with an image; pass one")
			return
		case rt.Name != "custom" && image != "":
			writeError(w, http.StatusBadRequest, "bad_request", "built-in runtime model apps do not take an image (use runtime custom)")
			return
		case image == "":
			image = rt.Image
		}
		s.deployImage(w, r, p, a, image)
		return
	}

	if req.Image != "" {
		s.deployImage(w, r, p, a, req.Image)
		return
	}

	if a.SourceType == "git" {
		d, err := s.deployGitApp(p, a, u.ID)
		if err != nil {
			switch {
			case errors.Is(err, errKubeUnavailable):
				// Unreachable today (requireKube already answered above),
				// kept so the shared core stays complete for any caller.
				writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
			case errors.Is(err, errBuildUnavailable):
				writeError(w, http.StatusServiceUnavailable, "build_unavailable", "server has no data directory configured")
			default:
				log.Printf("git deploy: %v", err)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"deployment_id": d.ID, "seq": d.Seq, "status": "building"})
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", "provide a source tarball or an image")
}

// deployGitApp is the shared core behind API (handleDeployApp's git branch)
// and UI (handleUIDeploy) git-triggered deploys: verify kube and the build
// source are configured, record a "building" deployment, and kick off the
// async build. Returns errKubeUnavailable / errBuildUnavailable when the
// respective dependency is missing — checked BEFORE creating the deployment
// row, so a row is never left stuck in "building" for a build that can't
// start (and startBuild's goroutine never sees a nil kube/src).
func (s *server) deployGitApp(p store.Project, a store.App, userID int64) (store.Deployment, error) {
	if s.kube == nil {
		return store.Deployment{}, errKubeUnavailable
	}
	if s.src == nil {
		return store.Deployment{}, errBuildUnavailable
	}
	d, err := s.st.CreateDeployment(a.ID, "building", "", userID)
	if err != nil {
		return store.Deployment{}, err
	}
	s.startBuild(p, a, d)
	return d, nil
}

// applyImageDeploy is the synchronous render+apply core shared by prebuilt
// image deploys and rollbacks: apply the app at `image`, then mark the
// deployment live — or failed, returning the error.
func (s *server) applyImageDeploy(ctx context.Context, p store.Project, a store.App, d store.Deployment, image string) error {
	rendered, err := s.renderApp(p, a, image, true)
	if err == nil {
		if err = s.ensureProjectNamespace(ctx, p.Namespace); err == nil {
			err = s.kube.Apply(ctx, p.Namespace, rendered.Objects)
		}
	}
	if err != nil {
		if e := s.st.SetDeploymentStatus(d.ID, "failed"); e != nil {
			log.Printf("mark deploy %s failed: %v", d.ID, e)
		}
		s.notify(notifyEvent{Event: "deploy_failed", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, Err: err.Error()})
		return err
	}
	if err := s.st.SetDeploymentStatus(d.ID, "live"); err != nil {
		log.Printf("mark deploy %s live (apply already succeeded): %v", d.ID, err)
	}
	s.notify(notifyEvent{Event: "deploy_success", Project: p.Name, App: a.Name, DeployID: d.ID, Seq: d.Seq, URL: s.appURL(a)})
	return nil
}

// deployImage is the synchronous prebuilt-image deploy path: render, apply,
// mark live. Unchanged from the pre-build-pipeline behavior.
func (s *server) deployImage(w http.ResponseWriter, r *http.Request, p store.Project, a store.App, image string) {
	d, err := s.st.CreateDeployment(a.ID, "deploying", image, 0)
	if err != nil {
		log.Printf("create deployment: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	if err := s.applyImageDeploy(r.Context(), p, a, d, image); err != nil {
		log.Printf("deploy image %s: %v", image, err)
		writeError(w, http.StatusBadGateway, "deploy_failed", "deploy failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID,
		"seq":           d.Seq,
		"status":        "live",
		"url":           s.appURL(a),
	})
}

// requireDeploy loads a deployment by id and verifies it belongs to app a.
// Writes the error response and returns ok=false on failure. idStr comes
// straight from the client (a path segment) — it must look like one of our
// nanoids before it's worth a DB round trip; a malformed id gets the same
// 400 an unparseable integer id used to get back before ids were opaque.
func (s *server) requireDeploy(w http.ResponseWriter, a store.App, idStr string) (store.Deployment, bool) {
	if !validDeployID(idStr) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid deploy id")
		return store.Deployment{}, false
	}
	d, err := s.st.GetDeployment(idStr)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.AppID != a.ID) {
		writeError(w, http.StatusNotFound, "not_found", "no such deploy")
		return store.Deployment{}, false
	}
	if err != nil {
		log.Printf("get deploy: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return store.Deployment{}, false
	}
	return d, true
}

// handleListDeploys returns an app's deploy history (newest first, capped at
// 50 by ListDeployments) — the CLI's only way to learn a deploy's internal
// id from its human-facing seq before calling the rollback API (which still
// takes the internal id).
func (s *server) handleListDeploys(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		log.Printf("list deploys: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]map[string]any, 0, len(history))
	for _, d := range history {
		out = append(out, map[string]any{
			"id":               d.ID,
			"seq":              d.Seq,
			"status":           d.Status,
			"image":            d.ImageRef,
			"created_at":       d.CreatedAt,
			"rolled_back_from": d.RolledBackFrom,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleGetDeploy(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	d, ok := s.requireDeploy(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": d.ID,
		"seq":           d.Seq,
		"status":        d.Status,
		"image":         d.ImageRef,
		"url":           s.appURL(a),
	})
}

func (s *server) handleDeployLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}
	d, ok := s.requireDeploy(w, a, r.PathValue("id"))
	if !ok {
		return
	}
	if s.src == nil {
		writeError(w, http.StatusServiceUnavailable, "build_unavailable", "no build logs available")
		return
	}
	if r.URL.Query().Get("follow") == "1" {
		fl, ok := sseStart(w)
		if !ok {
			return
		}
		s.followFile(w, fl, r, s.src.LogPath(d.ID), func() (bool, string) {
			cur, err := s.st.GetDeployment(d.ID)
			if err != nil {
				return true, "unknown"
			}
			return cur.Status == "live" || cur.Status == "failed", cur.Status
		})
		return
	}
	logBytes, err := s.src.ReadLog(d.ID)
	if err != nil {
		log.Printf("read log: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(logBytes)
}

// parseCPUMilli parses a CPU quantity ("250m", "1", "1.5") into millicores.
// "" clears (returns 0). Rejects negative or unparseable values.
func parseCPUMilli(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu quantity %q: %w", s, err)
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("cpu must be >= 0, got %q", s)
	}
	return q.MilliValue(), nil
}

// parseMemoryMB parses a memory quantity ("256Mi", "1Gi", or a plain integer
// meaning MiB, e.g. "512") into MiB. "" clears (returns 0). Rejects negative,
// unparseable, or nonzero-but-under-1Mi values.
func parseMemoryMB(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	qs := s
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		qs = s + "Mi" // plain integer means MiB
	}
	q, err := resource.ParseQuantity(qs)
	if err != nil {
		return 0, fmt.Errorf("invalid memory quantity %q: %w", s, err)
	}
	if q.Sign() < 0 {
		return 0, fmt.Errorf("memory must be >= 0, got %q", s)
	}
	mb := q.Value() / (1024 * 1024)
	if q.Value() > 0 && mb == 0 {
		return 0, fmt.Errorf("memory %q is less than 1Mi", s)
	}
	return mb, nil
}

// scaleChange is a partial update: nil fields are left unchanged. All-nil is
// rejected by scaleApp ("nothing to change").
type scaleChange struct {
	Replicas *int
	CPUMilli *int64
	MemoryMB *int64
	GPU      *int64
}

// scaleApp is the shared core of handleScaleApp and its UI twin
// (handleUIScale): check whether the app is live BEFORE persisting any
// change (a live app with no kube client can't apply a scale, so it must not
// record a DB state it can't honor), then persist and, if live, sync.
// Returns errKubeUnavailable when a live app's scale can't be applied, or a
// *scaleReplicasError when the requested replica count is invalid; any other
// error is an internal failure.
func (s *server) scaleApp(ctx context.Context, p store.Project, a store.App, req scaleChange) (store.App, error) {
	if a.Ejected {
		return store.App{}, errAppEjected
	}
	if req.Replicas == nil && req.CPUMilli == nil && req.MemoryMB == nil && req.GPU == nil {
		return store.App{}, &scaleReplicasError{fmt.Errorf("nothing to change")}
	}
	if a.Kind == "cron" && req.Replicas != nil {
		return store.App{}, &kindMismatchError{fmt.Errorf("cron apps do not scale")}
	}
	if req.Replicas != nil && a.AutoMin > 0 {
		return store.App{}, &scaleReplicasError{fmt.Errorf("autoscale active (%d-%d @%d%% cpu); disable first: luncur autoscale %s --off", a.AutoMin, a.AutoMax, a.AutoCPU, a.Name)}
	}
	if req.Replicas != nil && *req.Replicas > 1 {
		vols, err := s.st.ListVolumes(a.ID)
		if err != nil {
			return store.App{}, err
		}
		if len(vols) > 0 {
			return store.App{}, &volumeReplicaConflictError{fmt.Errorf("app has a volume (RWO node-local storage); max 1 replica")}
		}
	}
	// GPU budget: a scale request may change GPU count, replicas, or both
	// in one call, so validate the net delta between the app's current
	// planned GPU usage and what it would be after this change.
	if req.GPU != nil || req.Replicas != nil {
		newGPU := a.GPUCount
		if req.GPU != nil {
			newGPU = *req.GPU
		}
		newReplicas := a.Replicas
		if req.Replicas != nil {
			newReplicas = *req.Replicas
		}
		before := a.GPUCount * gpuEffReplicas(a.Kind, a.Replicas)
		after := newGPU * gpuEffReplicas(a.Kind, newReplicas)
		if delta := after - before; delta > 0 {
			if err := s.validateGPUBudget(p, delta); err != nil {
				return store.App{}, &scaleReplicasError{err}
			}
		}
	}
	d, err := s.st.LatestDeployment(a.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.App{}, err
	}
	live := err == nil && d.Status == "live"
	if live && s.kube == nil {
		return store.App{}, errKubeUnavailable
	}

	if req.Replicas != nil {
		if err := s.st.SetReplicas(a.ID, *req.Replicas); err != nil {
			return store.App{}, &scaleReplicasError{err}
		}
		a.Replicas = *req.Replicas
	}
	if req.CPUMilli != nil || req.MemoryMB != nil {
		cpu, mem := a.CPUMilli, a.MemoryMB
		if req.CPUMilli != nil {
			cpu = *req.CPUMilli
		}
		if req.MemoryMB != nil {
			mem = *req.MemoryMB
		}
		if err := s.st.SetResources(a.ID, cpu, mem); err != nil {
			return store.App{}, &scaleReplicasError{err}
		}
		a.CPUMilli, a.MemoryMB = cpu, mem
	}
	if req.GPU != nil {
		if err := s.st.SetGPU(a.ID, *req.GPU); err != nil {
			return store.App{}, &scaleReplicasError{err}
		}
		a.GPUCount = *req.GPU
	}

	if live {
		if err := s.syncApp(ctx, p, a); err != nil {
			return store.App{}, err
		}
		// Sync only upserts; when the replica floor drops below 2 the stale
		// PDB must be deleted or it blocks node drains.
		floor := a.Replicas
		if a.AutoMin > 0 {
			floor = a.AutoMin
		}
		if floor < 2 {
			if err := s.kube.DeleteObject(ctx, p.Namespace, "PodDisruptionBudget", a.Name); err != nil {
				log.Printf("delete pdb %s/%s: %v", p.Namespace, a.Name, err)
			}
		}
	}

	return a, nil
}

func (s *server) handleScaleApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var body struct {
		Replicas *int    `json:"replicas"`
		CPU      *string `json:"cpu"`
		Memory   *string `json:"memory"`
		GPU      *int64  `json:"gpu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	var req scaleChange
	req.Replicas = body.Replicas
	req.GPU = body.GPU
	if body.CPU != nil {
		cpu, err := parseCPUMilli(*body.CPU)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "cpu: "+err.Error())
			return
		}
		req.CPUMilli = &cpu
	}
	if body.Memory != nil {
		mem, err := parseMemoryMB(*body.Memory)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "memory: "+err.Error())
			return
		}
		req.MemoryMB = &mem
	}

	updated, err := s.scaleApp(r.Context(), p, a, req)
	if err != nil {
		var re *scaleReplicasError
		var ke *kindMismatchError
		var rc *volumeReplicaConflictError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, errKubeUnavailable):
			writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
		case errors.As(err, &ke):
			writeError(w, http.StatusBadRequest, "kind_mismatch", ke.Error())
		case errors.As(err, &rc):
			writeError(w, http.StatusConflict, "volume_replica_conflict", rc.Error())
		case errors.As(err, &re):
			writeError(w, http.StatusBadRequest, "bad_request", re.Error())
		default:
			log.Printf("scale app: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"replicas":  updated.Replicas,
		"cpu_milli": updated.CPUMilli,
		"memory_mb": updated.MemoryMB,
		"gpu":       updated.GPUCount,
	})
}

// autoscaleApp is the shared core of handleAutoscaleApp and its UI twin
// (handleUIAutoscale): min 0 disables autoscale, else min/max/cpu configure
// an autoscaling/v2 HPA. Guards mirror scaleApp's live-check-before-persist
// pattern; enabling additionally requires a CPU request, no volumes (RWO
// storage can't be autoscaled the same way it can't run >1 replica), and no
// GPUs (scale those manually).
func (s *server) autoscaleApp(ctx context.Context, p store.Project, a store.App, min, max, cpu int) (store.App, error) {
	if a.Ejected {
		return store.App{}, errAppEjected
	}
	enabling := min > 0
	if enabling {
		if a.Kind != "web" && a.Kind != "worker" {
			return store.App{}, &kindMismatchError{fmt.Errorf("only web and worker apps autoscale")}
		}
		if a.CPUMilli <= 0 {
			return store.App{}, &scaleReplicasError{fmt.Errorf("set CPU resources first (luncur scale <app> --cpu 500m); HPA needs a CPU request")}
		}
		vols, err := s.st.ListVolumes(a.ID)
		if err != nil {
			return store.App{}, err
		}
		if len(vols) > 0 {
			return store.App{}, &volumeReplicaConflictError{fmt.Errorf("app has a volume (RWO node-local storage); max 1 replica")}
		}
		if a.GPUCount > 0 {
			return store.App{}, &scaleReplicasError{fmt.Errorf("GPU apps can't autoscale; scale manually")}
		}
	}

	d, err := s.st.LatestDeployment(a.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.App{}, err
	}
	live := err == nil && d.Status == "live"
	if live && s.kube == nil {
		return store.App{}, errKubeUnavailable
	}

	if err := s.st.SetAutoscale(a.ID, min, max, cpu); err != nil {
		return store.App{}, &scaleReplicasError{err}
	}
	a.AutoMin, a.AutoMax, a.AutoCPU = min, max, cpu

	if live {
		if !enabling {
			if err := s.kube.DeleteObject(ctx, p.Namespace, "HorizontalPodAutoscaler", a.Name); err != nil {
				return store.App{}, err
			}
		}
		if err := s.syncApp(ctx, p, a); err != nil {
			return store.App{}, err
		}
		// Sync only upserts; when the replica floor drops below 2 the stale
		// PDB must be deleted or it blocks node drains.
		floor := a.Replicas
		if a.AutoMin > 0 {
			floor = a.AutoMin
		}
		if floor < 2 {
			if err := s.kube.DeleteObject(ctx, p.Namespace, "PodDisruptionBudget", a.Name); err != nil {
				log.Printf("delete pdb %s/%s: %v", p.Namespace, a.Name, err)
			}
		}
	}

	return a, nil
}

func (s *server) handleAutoscaleApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var body struct {
		Min *int `json:"min"`
		Max *int `json:"max"`
		CPU *int `json:"cpu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	min, max, cpu := 0, 0, 0
	if body.Min != nil {
		min = *body.Min
	}
	if body.Max != nil {
		max = *body.Max
	}
	if body.CPU != nil {
		cpu = *body.CPU
	}

	updated, err := s.autoscaleApp(r.Context(), p, a, min, max, cpu)
	if err != nil {
		var re *scaleReplicasError
		var ke *kindMismatchError
		var rc *volumeReplicaConflictError
		switch {
		case errors.Is(err, errAppEjected):
			writeError(w, http.StatusConflict, "app_ejected", errAppEjected.Error())
		case errors.Is(err, errKubeUnavailable):
			writeError(w, http.StatusServiceUnavailable, "kubernetes_unavailable", "kubernetes is not configured")
		case errors.As(err, &ke):
			writeError(w, http.StatusBadRequest, "kind_mismatch", ke.Error())
		case errors.As(err, &rc):
			writeError(w, http.StatusConflict, "volume_replica_conflict", rc.Error())
		case errors.As(err, &re):
			writeError(w, http.StatusBadRequest, "bad_request", re.Error())
		default:
			log.Printf("autoscale app: %v", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
		}
		return
	}

	writeJSON(w, http.StatusOK, s.appJSON(p, updated))
}

// errTrainingOverBudget wraps a validateGPUBudget failure from setAppTraining
// so handleSetTraining can tell it apart from a plain store validation error
// (bad nodes/framework) and answer with the same over_budget code startRun
// uses for the run-level version of this check.
var errTrainingOverBudget = errors.New("over gpu budget")

// setAppTraining is handleSetTraining's shared core: raising a job app's
// nodes raises its planned GPU footprint (SumProjectGPURequests counts job
// apps at gpu × nodes), so the delta is budget-checked before persisting.
func (s *server) setAppTraining(p store.Project, a store.App, nodes int, framework string) error {
	if extra := a.GPUCount * int64(nodes-a.Nodes); extra > 0 {
		if err := s.validateGPUBudget(p, extra); err != nil {
			return fmt.Errorf("%w: %v", errTrainingOverBudget, err)
		}
	}
	return s.st.SetAppTraining(a.ID, nodes, framework)
}

// handleSetTraining sets a kind=job app's default multi-node run shape
// (nodes/framework) — the values startRun falls back to when a run request
// doesn't override them.
func (s *server) handleSetTraining(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.requireProject(w, u, r.PathValue("project"))
	if !ok {
		return
	}
	a, ok := s.requireJobApp(w, p, r.PathValue("app"))
	if !ok {
		return
	}

	var req struct {
		Nodes     int    `json:"nodes"`
		Framework string `json:"framework"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	if err := s.setAppTraining(p, a, req.Nodes, req.Framework); err != nil {
		if errors.Is(err, errTrainingOverBudget) {
			writeError(w, http.StatusBadRequest, "over_budget", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"app": a.Name, "nodes": req.Nodes, "framework": req.Framework})
}
