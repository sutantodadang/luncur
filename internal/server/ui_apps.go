package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/sutantodadang/luncur/internal/kube"
	"github.com/sutantodadang/luncur/internal/render"
	"github.com/sutantodadang/luncur/internal/store"
)

// uiEnvChip is the environment selector's per-option view model — reused
// both for the current env (a single value, "Env" in the app/project page
// data) and the full list to switch between ("Envs"). Default envs get no
// special badge; Preview marks the chip a non-default env needs to look
// visually distinct (chip-warn vs chip-muted — see app.html/apps.html).
type uiEnvChip struct {
	Name    string
	Default bool
	Preview bool
}

// uiEnvChipFrom builds one uiEnvChip from a store.Environment.
func uiEnvChipFrom(e store.Environment) uiEnvChip {
	return uiEnvChip{Name: e.Name, Default: e.IsDefault, Preview: e.Kind == "preview"}
}

// uiEnvChips lists every environment on p, for the selector's option list.
func (s *server) uiEnvChips(p store.Project) ([]uiEnvChip, error) {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]uiEnvChip, 0, len(envs))
	for _, e := range envs {
		out = append(out, uiEnvChipFrom(e))
	}
	return out, nil
}

// uiPreviewApp is one preview environment's cloned app, as shown in the
// Previews card's app-URL list.
type uiPreviewApp struct {
	Name string
	URL  string
}

// uiPreviewRow is a preview environment's project-page view model — its
// source branch, idle-activity timestamp (the clock reapPreviews' TTL
// sweep reads), and its cloned apps' URLs. Mirrors previewJSON
// (preview.go), the REST API's equivalent shape.
type uiPreviewRow struct {
	Name         string
	SourceBranch string
	LastActiveAt string
	Apps         []uiPreviewApp
}

// uiPreviewRows lists every preview environment on p (kind=='preview' only
// — its standing environments are the env selector's concern, uiEnvChips),
// for the project page's Previews card.
func (s *server) uiPreviewRows(p store.Project) ([]uiPreviewRow, error) {
	envs, err := s.st.ListEnvironments(p.ID)
	if err != nil {
		return nil, err
	}
	rows := make([]uiPreviewRow, 0)
	for _, e := range envs {
		if e.Kind != "preview" {
			continue
		}
		apps, err := s.st.ListAppsInEnv(e.ID)
		if err != nil {
			return nil, err
		}
		appRows := make([]uiPreviewApp, 0, len(apps))
		for _, a := range apps {
			u := s.appURLForEnv(a, e.Name, p.DefaultEnv)
			if a.Internal {
				u = internalURLFor(a.Name, e.Namespace)
			}
			appRows = append(appRows, uiPreviewApp{Name: a.Name, URL: u})
		}
		rows = append(rows, uiPreviewRow{
			Name: e.Name, SourceBranch: e.SourceBranch, LastActiveAt: e.LastActiveAt, Apps: appRows,
		})
	}
	return rows, nil
}

// uiAppRow is apps.html's per-row view model: the store.App plus its
// derived public URL and latest-deploy status (empty when the app has never
// been deployed — the template renders a "no deploys" chip for that case).
type uiAppRow struct {
	Name        string
	Kind        string
	Schedule    string
	Replicas    int
	URL         string
	Internal    bool
	InternalURL string
	Ejected     bool
	Status      string
}

func (s *server) handleUIApps(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	env, ok := s.uiEnv(w, r, p)
	if !ok {
		return
	}
	list, err := s.st.ListAppsInEnv(env.ID)
	if err != nil {
		log.Printf("ui apps: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	envs, err := s.uiEnvChips(p)
	if err != nil {
		log.Printf("ui apps: list environments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rows := make([]uiAppRow, 0, len(list))
	for _, a := range list {
		// Every row here is already scoped to env (ListAppsInEnv), so its
		// public/internal URL is built directly off env rather than a
		// per-app re-lookup by EnvironmentID.
		url := s.appURLForEnv(a, env.Name, p.DefaultEnv)
		internalURL := ""
		if a.Kind != "web" {
			url = ""
		} else if a.Internal {
			url = ""
			internalURL = internalURLFor(a.Name, env.Namespace)
		}
		status := ""
		if d, err := s.st.LatestDeployment(a.ID); err == nil {
			status = d.Status
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Printf("ui apps latest deployment: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rows = append(rows, uiAppRow{
			Name: a.Name, Kind: a.Kind, Schedule: a.Schedule,
			Replicas: a.Replicas, URL: url, Internal: a.Internal, InternalURL: internalURL,
			Ejected: a.Ejected, Status: status,
		})
	}
	addons, err := s.addonRows(r.Context(), p)
	if err != nil {
		log.Printf("ui addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	members, err := s.st.ListMembers(p.ID)
	if err != nil {
		log.Printf("ui members: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	pipelines, err := s.uiPipelineCardRows(p)
	if err != nil {
		log.Printf("ui pipelines: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	previews, err := s.uiPreviewRows(p)
	if err != nil {
		log.Printf("ui previews: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var banner string
	if e := r.URL.Query().Get("err"); e != "" {
		banner = "error: " + e
	}
	// perr carries handleUIProjectRename/handleUIProjectDelete's outcome
	// back to this page — fixed strings only, same idiom as users.html's
	// "mail" notice, never the raw error or user input.
	var perrNote string
	switch r.URL.Query().Get("perr") {
	case "invalid":
		perrNote = "invalid project name"
	case "taken":
		perrNote = "name already in use"
	case "nokube":
		perrNote = "kubernetes unavailable — cannot destroy apps"
	}
	s.renderPage(w, r, "apps.html", map[string]any{
		"User": u, "Project": p, "Apps": rows, "Addons": addons, "Members": members, "Banner": banner,
		"CSRF": s.csrf(w, r), "IsAdmin": u.Role == "admin", "PErrNote": perrNote,
		"GPUQuota": p.GPUQuota, "Pipelines": pipelines, "Previews": previews,
		"CPUQuotaMilli": p.CPUQuotaMilli, "MemQuotaMB": p.MemQuotaMB,
		"Env": uiEnvChipFrom(env), "Envs": envs,
	})
}

// handleUICreateApp is handleCreateApp's UI twin: same store CreateApp/
// CreateGitApp core, plain-text 400 + redirect back to the create form
// instead of a JSON envelope.
func (s *server) handleUICreateApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProjectWrite(w, r, u)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	port := 0
	if v := r.PostFormValue("port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
		port = n
	}
	name := r.PostFormValue("name")
	kind := r.PostFormValue("kind")
	schedule := r.PostFormValue("schedule")
	gitURL := r.PostFormValue("git_url")
	image := strings.TrimSpace(r.PostFormValue("image"))

	buildPath, err := validBuildPath(r.PostFormValue("build_path"))
	if err != nil {
		http.Error(w, "build_path: "+err.Error(), http.StatusBadRequest)
		return
	}
	internal := r.PostFormValue("internal") != ""
	if err := validateInternalKind(internal, kind); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var gpu int64
	if v := r.PostFormValue("gpu"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			http.Error(w, "invalid gpu count", http.StatusBadRequest)
			return
		}
		gpu = n
	}
	modelSource := strings.TrimSpace(r.PostFormValue("model_source"))
	modelRuntime := r.PostFormValue("runtime")
	var modelRT render.ModelRuntimeInfo
	if kind == "model" {
		if gitURL != "" {
			http.Error(w, "model apps do not take a git url", http.StatusBadRequest)
			return
		}
		// Resolve now so a bad source/runtime combination fails before the
		// app row exists — same order as the JSON API's create.
		modelRT, err = render.ResolveModelRuntime(modelSource, modelRuntime, gpu)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	var a store.App
	switch {
	case kind == "model":
		a, err = s.st.CreateModelApp(p.ID, name, modelSource, modelRuntime)
	case gitURL != "":
		a, err = s.st.CreateGitApp(p.ID, name, port, gitURL, r.PostFormValue("git_branch"), kind, schedule)
	default:
		a, err = s.st.CreateApp(p.ID, name, port, kind, schedule)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// CreateApp/CreateGitApp/CreateModelApp only take a project_id; re-parent
	// to the project's default environment (see handleCreateApp) so every
	// env-scoped read (uiApp's GetApp lookup still works either way, but
	// syncIfLive/scaleApp/etc. below need a real environment) finds this app.
	env, err := s.st.GetEnvironment(p.ID, p.DefaultEnv)
	if err != nil {
		log.Printf("ui create app: get default environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.SetAppEnvironmentID(a.ID, env.ID); err != nil {
		log.Printf("ui create app: set app environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.EnvironmentID = env.ID
	if buildPath != "" {
		if err := s.st.SetBuildPath(a.ID, buildPath); err != nil {
			log.Printf("ui create app: set build path: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		a.BuildPath = buildPath
	}
	if internal {
		if err := s.st.SetInternal(a.ID, true); err != nil {
			log.Printf("ui create app: set internal: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		a.Internal = true
	}
	if gpu != 0 {
		if err := s.st.SetGPU(a.ID, gpu); err != nil {
			http.Error(w, "gpu: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.GPUCount = gpu
	}

	// Seal+store a private-repo clone token before any deploy, so the build
	// job can clone the repo. Only meaningful for git-source apps.
	if token := strings.TrimSpace(r.PostFormValue("git_token")); token != "" && a.SourceType == "git" {
		if err := s.setGitToken(r.Context(), a, token); err != nil {
			s.uiGitTokenError(w, err)
			return
		}
	}

	// Set env vars before any deploy so the container boots with them
	// present — e.g. postgres needs POSTGRES_PASSWORD on first start. The
	// app isn't live yet, so setAppEnvBulk just seals and stores; the deploy
	// below then renders the manifest with them.
	if envText := strings.TrimSpace(r.PostFormValue("env")); envText != "" {
		vars, err := parseDotenv(envText)
		if err != nil {
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("env: "+err.Error())+"&tab=ship", http.StatusSeeOther)
			return
		}
		if err := s.setAppEnvBulk(r.Context(), p, env, a, vars); err != nil {
			var ve *store.ValidationError
			msg := "env: internal error"
			switch {
			case errors.Is(err, errSealerUnavailable):
				msg = "env: sealer is not configured"
			case errors.As(err, &ve):
				msg = "env: " + ve.Error()
			default:
				log.Printf("ui create app: set env: %v", err)
			}
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(msg)+"&tab=ship", http.StatusSeeOther)
			return
		}
	}

	// Built-in runtime model apps deploy themselves at create: the runtime
	// image is known, so reuse the one-click image-deploy tail below.
	if a.Kind == "model" && modelRT.Name != "custom" {
		image = modelRT.Image
	}

	if image == "" {
		flash(w, "ok", "app created")
		http.Redirect(w, r, "/ui/projects/"+p.Name, http.StatusSeeOther)
		return
	}

	// One-click deploy from a prebuilt image: same applyImageDeploy core
	// deployImage (API) and rollback use. Any failure past this point leaves
	// the app created — only the deploy itself failed — so we redirect to
	// the app page with ?err= instead of erroring the whole create.
	if s.kube == nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: kubernetes is not configured")+"&tab=ship", http.StatusSeeOther)
		return
	}
	d, err := s.st.CreateDeployment(a.ID, "deploying", image, 0)
	if err != nil {
		log.Printf("ui create app: create deployment: %v", err)
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: internal error")+"&tab=ship", http.StatusSeeOther)
		return
	}
	if err := s.applyImageDeploy(r.Context(), p, env, a, d, image); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: "+err.Error())+"&tab=ship", http.StatusSeeOther)
		return
	}
	flash(w, "ok", "app created")
	uiRedirect(w, r, p, a, tabShip)
}

func (s *server) handleUIApp(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	s.renderAppDetail(w, r, u, p, a, uiResolveTab(r, a.Kind), nil)
}

// uiChipData is the "statuschip" fragment's view model: enough to render
// the chip itself plus, while the deploy is still in flight, the route the
// fragment polls to re-fetch its own next state.
type uiChipData struct {
	ProjectName string
	AppName     string
	Status      string
	Building    bool
}

// chipData classifies a latest-deploy status into the chip's view model.
// Shared by renderAppDetail (initial render) and handleUIChip (the polling
// fragment) so "what counts as still building" lives in exactly one place.
func chipData(projectName, appName, status string) uiChipData {
	return uiChipData{
		ProjectName: projectName, AppName: appName, Status: status,
		Building: status == "building" || status == "deploying",
	}
}

// handleUIChip is the polling fragment app.html's "statuschip" block
// re-fetches every 3s while a deploy is building/deploying. It renders only
// that one template block, not the full page.
func (s *server) handleUIChip(w http.ResponseWriter, r *http.Request, u store.User) {
	p, ok := s.uiProject(w, r, u)
	if !ok {
		return
	}
	a, ok := s.uiApp(w, r, p)
	if !ok {
		return
	}
	status := "never_deployed"
	if d, err := s.st.LatestDeployment(a.ID); err == nil {
		status = d.Status
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui chip: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "statuschip", chipData(p.Name, a.Name, status)); err != nil {
		log.Printf("render statuschip: %v", err)
	}
}

// uiDeployRow is app.html's Deploys-card view model: store.Deployment plus
// the image tag (the part of ImageRef after its last ":", full ref kept for
// the row's title attribute) and an actor placeholder. No store surface maps
// a user id to an email cheaply yet, so every row shows "-" for actor rather
// than adding one just for this column.
type uiDeployRow struct {
	ID                string
	Seq               int64
	Status            string
	ImageRef          string
	ImageTag          string
	CreatedAt         string
	RolledBackFromSeq int64 // 0 = not a rollback (or source fell out of history)
	Actor             string
}

// uiDeployRows builds the Deploys card's view model from ListDeployments'
// newest-first history, capped at limit rows. RolledBackFrom is an opaque
// id now, never shown to a user — the seq lookup map is built from the
// full (unclipped, up to 50-row) history so a rollback's source deploy
// resolves to its human-facing seq even when it falls outside the limit
// rows actually rendered.
// uiRunRow is app.html's Runs-card view model: store.JobRun with its
// nullable fields flattened to plain strings ("" when unset) for simple
// template rendering.
type uiRunRow struct {
	ID         int64
	Status     string
	Nodes      int
	ExitCode   string
	StartedAt  string
	FinishedAt string
}

// uiRunRows builds the Runs card's view model from ListJobRuns' newest-first
// history.
func uiRunRows(runs []store.JobRun) []uiRunRow {
	rows := make([]uiRunRow, 0, len(runs))
	for _, run := range runs {
		exit := ""
		if run.ExitCode.Valid {
			exit = strconv.FormatInt(run.ExitCode.Int64, 10)
		}
		finished := ""
		if run.FinishedAt.Valid {
			finished = run.FinishedAt.String
		}
		rows = append(rows, uiRunRow{
			ID: run.ID, Status: run.Status, Nodes: run.Nodes, ExitCode: exit,
			StartedAt: run.StartedAt, FinishedAt: finished,
		})
	}
	return rows
}

func uiDeployRows(history []store.Deployment, limit int) []uiDeployRow {
	seqByID := make(map[string]int64, len(history))
	for _, d := range history {
		seqByID[d.ID] = d.Seq
	}
	if len(history) > limit {
		history = history[:limit]
	}
	rows := make([]uiDeployRow, 0, len(history))
	for _, d := range history {
		tag := d.ImageRef
		if idx := strings.LastIndex(d.ImageRef, ":"); idx >= 0 {
			tag = d.ImageRef[idx+1:]
		}
		rows = append(rows, uiDeployRow{
			ID: d.ID, Seq: d.Seq, Status: d.Status, ImageRef: d.ImageRef, ImageTag: tag,
			CreatedAt: d.CreatedAt, RolledBackFromSeq: seqByID[d.RolledBackFrom], Actor: "-",
		})
	}
	return rows
}

// uiTab enumerates app.html's tab query values (UX Architecture v3, see
// DESIGN.md). Each is backed by its own "app_<tab>" define block — see
// internal/server/templates/app_overview.html etc. — rendered standalone as
// an htmx fragment or embedded in the full page shell.
type uiTab string

// uiResolveTab reads r's ?tab= query param, falling back to tabOverview for
// anything invalid: an absent/unknown value, or "jobs" on an app kind that
// has no Jobs tab (only job/cron apps show cron/runs/sweeps controls).
func uiResolveTab(r *http.Request, kind string) uiTab {
	switch tab := uiTab(r.URL.Query().Get("tab")); tab {
	case tabOverview, tabShip, tabObserve, tabWire, tabData:
		return tab
	case tabJobs:
		if kind == "job" || kind == "cron" {
			return tabJobs
		}
	}
	return tabOverview
}

// uiTabURL builds the app page URL for p/a on the given tab — the shared
// suffix every POST action handler redirects back to (see uiRedirect), so
// each control lands on the tab that owns it instead of always bouncing to
// Overview.
func uiTabURL(p store.Project, a store.App, tab uiTab) string {
	return "/ui/projects/" + p.Name + "/apps/" + a.Name + "?tab=" + string(tab)
}

// uiTabItem is one link in app.html's tab bar: hybrid naming (verb +
// mono noun-subtitle, DESIGN.md UX Architecture v3).
type uiTabItem struct {
	Key      string
	Verb     string
	Subtitle string
	Active   bool
}

// uiTabItems builds the six-tab bar, Jobs present only for kinds that carry
// cron/runs/sweeps controls (job, cron).
func uiTabItems(kind string, active uiTab) []uiTabItem {
	items := []uiTabItem{
		{Key: "overview", Verb: "Overview", Subtitle: "status · activity"},
		{Key: "ship", Verb: "Ship", Subtitle: "deploys · rollback · git"},
		{Key: "observe", Verb: "Observe", Subtitle: "logs · pods · metrics"},
		{Key: "wire", Verb: "Wire", Subtitle: "env · domains · scale"},
		{Key: "data", Verb: "Data", Subtitle: "volumes · addons"},
	}
	if kind == "job" || kind == "cron" {
		items = append(items, uiTabItem{Key: "jobs", Verb: "Jobs", Subtitle: "cron · runs · sweeps"})
	}
	for i := range items {
		items[i].Active = uiTab(items[i].Key) == active
	}
	return items
}

// uiPipelineStage is one box in Overview's literal deploy-state-machine
// render (DESIGN.md "Deploy state machine rendered literally").
type uiPipelineStage struct {
	Name  string
	Class string // "pipe-done" | "pipe-current" | "pipe-fail" | "pipe-pending"
}

// deployFailedStage names the pipeline stage a failed deploy died in:
// "deploying" unless no image was ever resolved (still empty at failure),
// in which case it died in "building" — the two points
// applyImageDeploy/deployGitApp can fail at. Shared by uiPipelineStages
// (which stage to mark red) and the Overview error card's "what" line
// (DESIGN.md "Error contract"), so the two surfaces never disagree about
// where a deploy died.
func deployFailedStage(latestImageRef string) string {
	if latestImageRef == "" {
		return "building"
	}
	return "deploying"
}

// uiPipelineStages classifies the latest deployment's status into the four
// pipeline boxes: queued -> building -> deploying -> live.
func uiPipelineStages(status, latestImageRef string) []uiPipelineStage {
	names := []string{"queued", "building", "deploying", "live"}
	order := map[string]int{"queued": 0, "building": 1, "deploying": 2, "live": 3}
	failedAt := deployFailedStage(latestImageRef)
	current := -1
	switch status {
	case "building", "deploying", "live":
		current = order[status]
	case "failed":
		current = order[failedAt]
	}
	stages := make([]uiPipelineStage, len(names))
	for i, n := range names {
		class := "pipe-pending"
		switch {
		case status == "failed" && i == current:
			class = "pipe-fail"
		case i < current, n == "live" && status == "live":
			class = "pipe-done"
		case i == current:
			class = "pipe-current"
		}
		stages[i] = uiPipelineStage{Name: n, Class: class}
	}
	return stages
}

// uiErrorCard is Overview's 3-line error contract (DESIGN.md "Error
// contract") for a failed latest deploy: what broke, a best-effort why
// (deployFailureHint against the build log's tail), and the next command —
// the template adds the copyable CLI-echo and a link into Ship's deploy
// history row for this deploy's raw log.
type uiErrorCard struct {
	Seq   int64
	Stage string
	Why   string
}

// lastLines returns at most n trailing lines of s, joined back with "\n".
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// deployFailureHint is the error card's "why" line: a best-effort guess at
// why a build failed, read from the tail of its build log. Markers are
// checked in this fixed order — an OOM during "npm install" must still
// report OOM, not a dependency failure — so order is part of the contract.
// Pure function (no log I/O) so it's cheaply table-tested; the caller
// passes lastLines(actual log, deployLogTailLines).
func deployFailureHint(logTail string) string {
	lower := strings.ToLower(logTail)
	switch {
	case logTail == "":
		return "See the build log for the failing step."
	case strings.Contains(lower, "oom") || strings.Contains(lower, "killed"):
		return "builder pod ran out of memory (OOM-killed)"
	case strings.Contains(lower, "no space left"):
		return "builder disk is full (no space left on device)"
	case strings.Contains(lower, "copy failed") || strings.Contains(lower, "not found"):
		return "a file or path referenced in the build was not found"
	case strings.Contains(lower, "npm err!") || strings.Contains(lower, "pip install") || strings.Contains(lower, "go build"):
		for _, line := range strings.Split(logTail, "\n") {
			l := strings.ToLower(line)
			if strings.Contains(l, "npm err!") || strings.Contains(l, "pip install") || strings.Contains(l, "go build") {
				line = strings.TrimSpace(line)
				if len(line) > 120 {
					line = line[:120]
				}
				return fmt.Sprintf("dependency install or compile step failed: %q", line)
			}
		}
		return "dependency install or compile step failed"
	case strings.Contains(lower, "failedcreate") || strings.Contains(lower, "forbidden"):
		return "cluster policy or RBAC blocked the builder pod"
	default:
		return "See the build log for the failing step."
	}
}

// uiPodHistoryRow is one row of the Pods card's collapsed history
// disclosure (DESIGN.md "Pods presentation"): an exited pod with its
// plain-language reason, so an operator doesn't have to decode
// "CrashLoopBackOff" vs a raw Evicted message themselves.
type uiPodHistoryRow struct {
	Name   string
	Reason string
	Age    string
}

// podHistoryReason maps a history pod's raw kube.PodInfo exit detail to the
// plain-language sentence DESIGN.md's Pods presentation calls for. Falls
// back to the pod's raw waiting/exit reason (or "-") for anything the
// mapping doesn't special-case, so an unexpected reason still shows
// something rather than going blank.
func podHistoryReason(p kube.PodInfo) string {
	switch p.ExitReason {
	case "Evicted":
		return "Evicted — node memory/disk pressure"
	case "OOMKilled":
		return "OOM-killed — memory limit hit"
	case "Error":
		if p.ExitCode != 0 {
			return fmt.Sprintf("exited with code %d", p.ExitCode)
		}
		return "exited with an error"
	case "Completed":
		return "completed"
	case "":
		if p.Reason != "" {
			return p.Reason
		}
		return "-"
	default:
		return p.ExitReason
	}
}

// uiPodsView is app_observe's Pods card view model and Overview's summary
// line (DESIGN.md "Pods presentation"): live pods (Running/Pending) keep
// the full table; Failed/Succeeded/Terminating pods move into History with
// a plain-language reason instead, since they're only around until the
// hourly failed-pod GC prunes them.
type uiPodsView struct {
	Wanted, Running, Restarts int
	Live                      []kube.PodInfo
	History                   []uiPodHistoryRow
}

// uiPods builds the Pods view model. wanted is the autoscale floor when
// autoscale is on, else the app's stored replica count — the same
// AutoMin>0?AutoMin:Replicas floor logic scaleApp/autoscaleApp use.
// Restarts sums only live pods' restart counts — a pod already moved to
// History no longer contributes to "current" restart pressure.
func uiPods(pods []kube.PodInfo, autoMin, replicas int) uiPodsView {
	wanted := replicas
	if autoMin > 0 {
		wanted = autoMin
	}
	v := uiPodsView{Wanted: wanted}
	for _, p := range pods {
		switch p.Phase {
		case "Failed", "Succeeded", "Terminating":
			v.History = append(v.History, uiPodHistoryRow{
				Name: p.Name, Reason: podHistoryReason(p), Age: p.StartedAt,
			})
		default:
			v.Live = append(v.Live, p)
			v.Restarts += int(p.Restarts)
			if p.Phase == "Running" && p.Ready {
				v.Running++
			}
		}
	}
	return v
}

// uiLaunchStep is one row of Overview's Launch Sequence checklist
// (DESIGN.md "Launch Sequence"): State is "done" (green ●), "current"
// (orange ◌ — its form expands inline), "todo" (muted ○), or "skipped"
// (muted checkmark, env vars skipped because a deploy already shipped
// without them).
type uiLaunchStep struct {
	Label string
	State string
	Hint  string
}

// uiLaunchSequence is the checklist Overview shows in place of the status
// board + pipeline for an app that has never deployed. Exactly one step is
// ever "current"; EnvCurrent/DeployCurrent tell the template which inline
// form (if any — a tarball/image-source app has no UI deploy control yet,
// see DESIGN.md's Parity Contract) to expand in that row.
type uiLaunchSequence struct {
	Steps         []uiLaunchStep
	EnvCurrent    bool
	DeployCurrent bool
}

// uiLaunchSequenceFor builds the checklist. Every step observes real
// store state, never click history, so a step completed via the CLI shows
// done in the UI too. hasLiveDeploy/hasInFlightDeploy are always false on
// Overview's actual call site (it only builds this checklist when the app
// has zero deployments ever — DESIGN.md: the checklist disappears for
// good the moment a first deploy exists), but the branches that key off
// them are kept correct rather than assumed unreachable, since this is a
// pure function worth testing on its own terms.
func uiLaunchSequenceFor(a store.App, envKeyCount int, hasLiveDeploy, hasInFlightDeploy bool, domainCount int) uiLaunchSequence {
	step1Label := "Connect repository"
	if a.SourceType != "git" {
		step1Label = "Choose image source"
	}
	step1 := uiLaunchStep{Label: step1Label, State: "done", Hint: "connected"}

	step2 := uiLaunchStep{Label: "Set environment variables"}
	switch {
	case envKeyCount > 0:
		step2.State, step2.Hint = "done", fmt.Sprintf("%d set", envKeyCount)
	case hasLiveDeploy || hasInFlightDeploy:
		step2.State, step2.Hint = "skipped", "skipped"
	default:
		step2.State, step2.Hint = "current", "no env vars yet"
	}

	step3 := uiLaunchStep{Label: "First deploy"}
	switch {
	case hasLiveDeploy:
		step3.State, step3.Hint = "done", "live"
	case hasInFlightDeploy:
		step3.State, step3.Hint = "current", "in progress"
	case step2.State == "current":
		step3.State, step3.Hint = "todo", "not started"
	default:
		step3.State, step3.Hint = "current", "not started"
	}

	step4 := uiLaunchStep{Label: "Attach domain (optional)"}
	if domainCount > 0 {
		step4.State, step4.Hint = "done", fmt.Sprintf("%d attached", domainCount)
	} else {
		step4.State, step4.Hint = "todo", "optional"
	}

	return uiLaunchSequence{
		Steps:         []uiLaunchStep{step1, step2, step3, step4},
		EnvCurrent:    step2.State == "current",
		DeployCurrent: step3.State == "current" && a.SourceType == "git",
	}
}

// renderAppDetail assembles app.html's view model for tab and renders it —
// either the full page shell (nav/breadcrumb/tab bar/content) or, for an
// htmx tab-switch request (HX-Request set, HX-Boosted not — a boosted nav
// still needs the full page), just that tab's own "app_<tab>" fragment. Only
// the data the active tab needs is assembled: kube pod listing + metrics
// history for overview/observe, runs/sweeps store queries + the cronRuns
// kube call for jobs — domains/volumes/addons/env/history stay cheap SQLite
// loaded on every tab. extra is merged in last (overriding nothing app.html
// itself sets) — its only current use is handleUIWebhookEnable riding the
// freshly generated secret along on the same response, instead of a
// redirect (a redirect would have to carry the secret in the URL, which
// must never happen).
func (s *server) renderAppDetail(w http.ResponseWriter, r *http.Request, u store.User, p store.Project, a store.App, tab uiTab, extra map[string]any) {
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui app detail: get environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	status := "never_deployed"
	latestID := ""
	var latestSeq int64
	var latestImageRef string
	if d, err := s.st.LatestDeployment(a.ID); err == nil {
		status = d.Status
		latestID = d.ID
		latestSeq = d.Seq
		latestImageRef = d.ImageRef
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("ui app latest deployment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	history, err := s.st.ListDeployments(a.ID)
	if err != nil {
		log.Printf("ui app history: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Env values stay sealed — the UI only ever shows keys, never plaintext.
	sealed, err := s.st.ListEnv(a.ID)
	if err != nil {
		log.Printf("ui app env: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	envKeys := make([]string, 0, len(sealed))
	for k := range sealed {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	domains, err := s.st.ListDomains(a.ID)
	if err != nil {
		log.Printf("ui app domains: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	volumes, err := s.st.ListVolumes(a.ID)
	if err != nil {
		log.Printf("ui app volumes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	attached, err := s.st.AddonsForApp(a.ID)
	if err != nil {
		log.Printf("ui app addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	projectAddons, err := s.st.ListAddons(p.ID)
	if err != nil {
		log.Printf("ui project addons: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Kube pod listing + metrics history are Overview/Observe-only: they're
	// the expensive calls this tab split exists to gate (see renderAppDetail's
	// doc comment).
	var metrics appMetricsView
	var pods []kube.PodInfo
	if tab == tabOverview || tab == tabObserve {
		metrics, err = s.appMetricsData(r.Context(), p, env, a)
		if err != nil {
			log.Printf("ui app metrics: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if s.kube != nil {
			if list, err := s.kube.AppPodInfos(r.Context(), env.Namespace, a.Name); err == nil {
				pods = list
			}
		}
	}
	podsView := uiPods(pods, a.AutoMin, a.Replicas)

	// Error card (Overview-only, DESIGN.md "Error contract"): best-effort
	// "why" read from the failed deploy's own build log tail. Gated to the
	// one tab that renders it so a failed deploy sitting on, say, the Data
	// tab doesn't pay for a log read it won't show.
	var errorCard *uiErrorCard
	if tab == tabOverview && status == "failed" {
		why := ""
		if s.src != nil {
			if logBytes, err := s.src.ReadLog(latestID); err == nil {
				why = deployFailureHint(lastLines(string(logBytes), deployLogTailLines))
			}
		}
		if why == "" {
			why = deployFailureHint("")
		}
		errorCard = &uiErrorCard{Seq: latestSeq, Stage: deployFailedStage(latestImageRef), Why: why}
	}

	// Launch Sequence (Overview-only, DESIGN.md "Launch Sequence"): shown
	// in place of the status board + pipeline while the app has never
	// deployed — status stays the "never_deployed" sentinel exactly until
	// that first deployment row exists, so this and the status board can
	// never both apply.
	var launch *uiLaunchSequence
	if tab == tabOverview && status == "never_deployed" {
		seq := uiLaunchSequenceFor(a, len(envKeys), false, false, len(domains))
		launch = &seq
	}

	// Runs/CronRuns/Sweeps cards are Jobs-tab-only (also kind-gated, same as
	// before) — the store queries and the cronRuns kube call are the other
	// expensive calls this split gates.
	var runRows []uiRunRow
	var cronRuns []uiCronRunRow
	var sweepRows []uiSweepRow
	var sweep *uiSweepData
	if tab == tabJobs {
		if a.Kind == "job" {
			runs, err := s.st.ListJobRuns(a.ID)
			if err != nil {
				log.Printf("ui app runs: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			runRows = uiRunRows(runs)
		}
		if a.Kind == "cron" && s.kube != nil {
			if list, err := s.kube.CronRuns(r.Context(), env.Namespace, a.Name); err == nil {
				cronRuns = list
			}
		}
		// Sweeps card, likewise job-only: sweepRows is the history table
		// (newest first); sweep is the most recent sweep's live detail (nil
		// when the app has none yet) — the card only ever shows one sweep's
		// trial table, not every past sweep's.
		if a.Kind == "job" {
			sweeps, err := s.st.ListSweeps(a.ID)
			if err != nil {
				log.Printf("ui app sweeps: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			sweepRows = make([]uiSweepRow, 0, len(sweeps))
			for _, sw := range sweeps {
				trials, err := s.st.ListTrials(sw.ID)
				if err != nil {
					log.Printf("ui app sweep %s trials: %v", sw.ID, err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				sweepRows = append(sweepRows, uiSweepRowFrom(sw, trials))
				if sweep == nil {
					d := uiSweepDataFrom(sw, trials)
					sweep = &d
				}
			}
		}
	}

	url := s.appURLForEnv(a, env.Name, p.DefaultEnv)
	internalURL := ""
	if a.Internal {
		internalURL = internalURLFor(a.Name, env.Namespace)
	}

	chip := chipData(p.Name, a.Name, status)
	csrf := s.csrf(w, r)
	if sweep != nil {
		sweep.ProjectName, sweep.AppName, sweep.CSRF = p.Name, a.Name, csrf
	}
	envs, err := s.uiEnvChips(p)
	if err != nil {
		log.Printf("ui app detail: list environments: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"User": u, "Project": p, "App": a,
		"Status": status, "LatestID": latestID, "LatestSeq": latestSeq, "URL": url, "InternalURL": internalURL,
		"Chip": chip, "Building": chip.Building,
		"Deploys": uiDeployRows(history, 10), "EnvKeys": envKeys,
		"IsGit":          a.SourceType == "git",
		"WebhookEnabled": a.WebhookSecret != nil,
		"WebhookURL":     "http://" + r.Host + webhookPath(p.Name, a.Name),
		"Domains":        domains, "Volumes": volumes, "Warning": firstNonEmpty(r.URL.Query().Get("warn"), r.URL.Query().Get("err")),
		"Addons": attached, "ProjectAddons": projectAddons, "Metrics": metrics, "PodsView": podsView,
		"Runs": runRows, "TrainFrameworks": render.TrainFrameworks,
		"CronRuns": cronRuns,
		"Sweeps":   sweepRows, "Sweep": sweep,
		"CSRF": csrf, "IsAdmin": u.Role == "admin",
		"Env": uiEnvChipFrom(env), "Envs": envs,
		"Tab": string(tab), "TabItems": uiTabItems(a.Kind, tab),
		"PipelineStages": uiPipelineStages(status, latestImageRef),
		"ErrorCard":      errorCard,
		"LaunchSequence": launch,
	}
	for k, v := range extra {
		data[k] = v
	}

	// A tab-switch click (hx-get on the tab bar, see app.html's "app_shell")
	// only needs the swapped-in tab's own fragment — HX-Request is set, and
	// HX-Boosted is NOT (the tab links carry their own explicit hx-get/
	// hx-target, which htmx honors over the body's hx-boost). A boosted nav
	// (e.g. clicking a sidebar link) also sets HX-Request, but needs the
	// full page, so it's excluded here.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.tmpl.ExecuteTemplate(w, "app_"+string(tab), data); err != nil {
			log.Printf("render app_%s: %v", tab, err)
		}
		return
	}
	s.renderPage(w, r, "app.html", data)
}
