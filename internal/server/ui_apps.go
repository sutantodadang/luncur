package server

import (
	"errors"
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
	s.renderPage(w, "apps.html", map[string]any{
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
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("env: "+err.Error()), http.StatusSeeOther)
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
			http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
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
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: kubernetes is not configured"), http.StatusSeeOther)
		return
	}
	d, err := s.st.CreateDeployment(a.ID, "deploying", image, 0)
	if err != nil {
		log.Printf("ui create app: create deployment: %v", err)
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: internal error"), http.StatusSeeOther)
		return
	}
	if err := s.applyImageDeploy(r.Context(), p, env, a, d, image); err != nil {
		http.Redirect(w, r, "/ui/projects/"+p.Name+"/apps/"+a.Name+"?err="+url.QueryEscape("deploy failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	flash(w, "ok", "app created")
	uiRedirect(w, r, p, a)
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
	s.renderAppDetail(w, r, u, p, a, nil)
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

// renderAppDetail assembles app.html's full view model and renders it.
// extra is merged in last (overriding nothing app.html itself sets) — its
// only current use is handleUIWebhookEnable riding the freshly generated
// secret along on the same response, instead of a redirect (a redirect
// would have to carry the secret in the URL, which must never happen).
func (s *server) renderAppDetail(w http.ResponseWriter, r *http.Request, u store.User, p store.Project, a store.App, extra map[string]any) {
	env, err := s.st.GetEnvironmentByID(a.EnvironmentID)
	if err != nil {
		log.Printf("ui app detail: get environment: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	status := "never_deployed"
	latestID := ""
	if d, err := s.st.LatestDeployment(a.ID); err == nil {
		status = d.Status
		latestID = d.ID
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

	metrics, err := s.appMetricsData(r.Context(), p, env, a)
	if err != nil {
		log.Printf("ui app metrics: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var pods []kube.PodInfo
	if s.kube != nil {
		if list, err := s.kube.AppPodInfos(r.Context(), env.Namespace, a.Name); err == nil {
			pods = list
		}
	}

	// Runs card is only meaningful for kind=job apps; nil for every other
	// kind (app.html gates the whole card on .App.Kind).
	var runRows []uiRunRow
	if a.Kind == "job" {
		runs, err := s.st.ListJobRuns(a.ID)
		if err != nil {
			log.Printf("ui app runs: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		runRows = uiRunRows(runs)
	}

	// Cron runs card is only meaningful for kind=cron apps; nil (empty table)
	// for every other kind, and also when the cluster listing errs — same
	// tolerance as the pods block above.
	var cronRuns []uiCronRunRow
	if a.Kind == "cron" && s.kube != nil {
		if list, err := s.kube.CronRuns(r.Context(), env.Namespace, a.Name); err == nil {
			cronRuns = list
		}
	}

	// Sweeps card, likewise job-only: sweepRows is the history table (newest
	// first); sweep is the most recent sweep's live detail (nil when the app
	// has none yet) — the card only ever shows one sweep's trial table, not
	// every past sweep's.
	var sweepRows []uiSweepRow
	var sweep *uiSweepData
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
		"Status": status, "LatestID": latestID, "URL": url, "InternalURL": internalURL,
		"Chip": chip, "Building": chip.Building,
		"Deploys": uiDeployRows(history, 10), "EnvKeys": envKeys,
		"IsGit":          a.SourceType == "git",
		"WebhookEnabled": a.WebhookSecret != nil,
		"WebhookURL":     "http://" + r.Host + webhookPath(p.Name, a.Name),
		"Domains":        domains, "Volumes": volumes, "Warning": firstNonEmpty(r.URL.Query().Get("warn"), r.URL.Query().Get("err")),
		"Addons": attached, "ProjectAddons": projectAddons, "Metrics": metrics, "Pods": pods,
		"Runs": runRows, "TrainFrameworks": render.TrainFrameworks,
		"CronRuns": cronRuns,
		"Sweeps":   sweepRows, "Sweep": sweep,
		"CSRF": csrf, "IsAdmin": u.Role == "admin",
		"Env": uiEnvChipFrom(env), "Envs": envs,
	}
	for k, v := range extra {
		data[k] = v
	}
	s.renderPage(w, "app.html", data)
}
