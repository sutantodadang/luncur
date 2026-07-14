package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type App struct {
	ID        int64
	ProjectID int64
	// EnvironmentID is the owning environment's id. 0 on rows written before
	// environments existed (backfillEnvironments re-parents those to the
	// project's production environment).
	EnvironmentID int64
	Name          string
	Port          int
	Replicas      int
	SourceType    string
	GitURL        string
	GitBranch     string
	Ejected       bool
	CPUMilli      int64
	MemoryMB      int64
	HealthPath    string
	// Kind is one of web|worker|cron; "" normalizes to "web" on create.
	Kind string
	// Schedule is a 5-field cron expression; only set (and required) for
	// cron apps.
	Schedule string
	// WebhookSecret is the sealed (AES-256-GCM) webhook secret; nil means
	// the webhook is disabled. Only meaningful for git-source apps.
	WebhookSecret []byte
	// BuildPath is an optional repo-relative subdirectory used as the build
	// context/detection dir, letting one git repo back several apps
	// (monorepo support). "" (the default) builds the repo root — the
	// pre-existing behavior. Set at create time; immutable thereafter
	// (recreate the app to change it).
	BuildPath string
	// Internal marks a web app as cluster-only: render emits a ClusterIP
	// Service but no Ingress, so the app has no public URL — only reachable
	// from other apps in the same cluster. Only meaningful for kind "web";
	// worker/cron already have no Service to make internal.
	Internal bool
	// GPUCount is the number of nvidia.com/gpu devices the app requests
	// (requests==limits). 0 means none. Every kind may request GPUs —
	// cron included (scheduled retraining on GPU nodes).
	GPUCount int64
	// InjectS3 opts the app into LUNCUR_S3_* env injection from the
	// project's external S3 settings (an attached MinIO addon injects the
	// same keys via the addon-attachment mechanism instead).
	InjectS3 bool
	// ModelSource locates a kind=model app's weights: hf:<org>/<name>[/<file>]
	// or s3:<key>. Empty for other kinds.
	ModelSource string
	// Runtime is the model serving runtime: ""/auto (resolved at render),
	// llamacpp, vllm, or custom. Only meaningful for kind "model".
	Runtime string
	// Nodes is the default number of pods a kind=job run spans. 1 (the
	// default) renders the classic single-pod Job; a run may override it.
	Nodes int
	// Framework optionally names a rendezvous env preset applied on top of
	// the LUNCUR_* contract for multi-node runs: "torchrun" or "torch".
	// "" means the raw contract only. Keep in sync with
	// render.TrainFrameworks / render's framework validation.
	Framework string
	// AutoMin, AutoMax, AutoCPU configure autoscaling/v2 HorizontalPodAutoscaler
	// for web/worker apps. AutoMin 0 means autoscale is off; when on, the HPA
	// owns replica count and Replicas is only the value restored on disable.
	AutoMin, AutoMax, AutoCPU int
	// Suspended pauses a cron app's schedule: render maps it to
	// CronJob.Spec.Suspend, so a suspended cron stops firing new runs without
	// losing its stored schedule/history. Only meaningful for kind "cron".
	Suspended bool
}

// TrainFrameworks are the valid values for apps.framework / job_runs.framework.
// "" = raw LUNCUR_* env contract only. Order matters for the error message.
// Keep in sync with render.TrainFrameworks.
var TrainFrameworks = []string{"torchrun", "torch"}

func validFramework(f string) bool {
	if f == "" {
		return true
	}
	for _, v := range TrainFrameworks {
		if f == v {
			return true
		}
	}
	return false
}

// SetAppTraining sets a job app's default multi-node run shape. The server
// layer enforces kind=job; the store only validates values.
func (s *Store) SetAppTraining(appID int64, nodes int, framework string) error {
	if nodes < 1 {
		return errors.New("nodes must be >= 1")
	}
	if !validFramework(framework) {
		return fmt.Errorf("unknown framework %q (valid: %s)", framework, strings.Join(TrainFrameworks, ", "))
	}
	res, err := s.db.Exec(`UPDATE apps SET nodes = ?, framework = ? WHERE id = ?`, nodes, framework, appID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// normalizeAppKind defaults an empty kind to "web" for back-compat with
// callers written before app kinds existed.
func normalizeAppKind(kind string) string {
	if kind == "" {
		return "web"
	}
	return kind
}

// validateAppKind enforces the port/schedule matrix per app kind: web needs
// a port and no schedule; worker and job need neither; cron needs a
// validated schedule and no port.
func validateAppKind(kind string, port int, schedule string) error {
	switch kind {
	case "web":
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %d", port)
		}
		if schedule != "" {
			return fmt.Errorf("schedule is only valid for cron apps")
		}
	case "worker":
		if port != 0 {
			return fmt.Errorf("worker apps do not take a port")
		}
		if schedule != "" {
			return fmt.Errorf("schedule is only valid for cron apps")
		}
	case "job":
		if port != 0 {
			return fmt.Errorf("job apps do not take a port")
		}
		if schedule != "" {
			return fmt.Errorf("job apps run on demand; use kind cron for a schedule")
		}
	case "model":
		if port != 0 {
			return fmt.Errorf("model apps do not take a port (the runtime picks one)")
		}
		if schedule != "" {
			return fmt.Errorf("schedule is only valid for cron apps")
		}
	case "cron":
		if port != 0 {
			return fmt.Errorf("cron apps do not take a port")
		}
		if schedule == "" {
			return fmt.Errorf("cron apps require a schedule")
		}
		if err := validCronSpec(schedule); err != nil {
			return fmt.Errorf("invalid schedule: %w", err)
		}
	default:
		return fmt.Errorf("invalid kind %q (want web, worker, cron, job, or model)", kind)
	}
	return nil
}

func (s *Store) CreateApp(projectID int64, name string, port int, kind, schedule string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	kind = normalizeAppKind(kind)
	if err := validateAppKind(kind, port, schedule); err != nil {
		return App{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, port, kind, schedule) VALUES (?, ?, 'tarball', ?, ?, ?)`,
		projectID, name, port, kind, schedule,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{
		ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1,
		SourceType: "tarball", Kind: kind, Schedule: schedule, Nodes: 1,
	}, nil
}

// validModelRuntime is the runtime enum a model app may carry; "" and
// "auto" both mean render-time resolution.
func validModelRuntime(r string) bool {
	switch r {
	case "", "auto", "llamacpp", "vllm", "custom":
		return true
	}
	return false
}

// CreateModelApp registers a kind=model app. source must be
// hf:<org>/<name>[/<file>] or s3:<key>; deeper shape validation happens at
// render time.
func (s *Store) CreateModelApp(projectID int64, name, source, runtime string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	if !strings.HasPrefix(source, "hf:") && !strings.HasPrefix(source, "s3:") {
		return App{}, fmt.Errorf("model source must start with hf: or s3:, got %q", source)
	}
	if !validModelRuntime(runtime) {
		return App{}, fmt.Errorf("invalid runtime %q (auto|llamacpp|vllm|custom)", runtime)
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, port, kind, model_source, runtime) VALUES (?, ?, 'tarball', 0, 'model', ?, ?)`,
		projectID, name, source, runtime,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{
		ID: id, ProjectID: projectID, Name: name, Replicas: 1,
		SourceType: "tarball", Kind: "model", ModelSource: source, Runtime: runtime, Nodes: 1,
	}, nil
}

// CreateGitApp registers an app whose source is a git repo cloned at deploy
// time. Phase 1 supports public repos / token-in-URL only; the git_token_enc
// column stays unused until private-repo token sealing lands.
func (s *Store) CreateGitApp(projectID int64, name string, port int, gitURL, gitBranch, kind, schedule string) (App, error) {
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	kind = normalizeAppKind(kind)
	if err := validateAppKind(kind, port, schedule); err != nil {
		return App{}, err
	}
	if gitURL == "" {
		return App{}, fmt.Errorf("git url is required")
	}
	if gitBranch == "" {
		gitBranch = "main"
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, name, source_type, git_url, git_branch, port, kind, schedule) VALUES (?, ?, 'git', ?, ?, ?, ?, ?)`,
		projectID, name, gitURL, gitBranch, port, kind, schedule,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{
		ID: id, ProjectID: projectID, Name: name, Port: port, Replicas: 1,
		SourceType: "git", GitURL: gitURL, GitBranch: gitBranch, Kind: kind, Schedule: schedule, Nodes: 1,
	}, nil
}

// SetGitToken stores the sealed git access token for a private-repo clone.
// The value arrives already sealed — the store never sees plaintext. A nil or
// empty slice clears it. Returns ErrNotFound if the app does not exist.
func (s *Store) SetGitToken(appID int64, sealed []byte) error {
	var enc any // nil → SQL NULL (clears the token)
	if len(sealed) > 0 {
		enc = sealed
	}
	res, err := s.db.Exec(`UPDATE apps SET git_token_enc = ? WHERE id = ?`, enc, appID)
	if err != nil {
		return fmt.Errorf("set git token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GitToken returns the sealed git token bytes for an app, or nil if none is
// set. Callers unseal via the server's sealer.
func (s *Store) GitToken(appID int64) ([]byte, error) {
	var enc []byte
	err := s.db.QueryRow(`SELECT git_token_enc FROM apps WHERE id = ?`, appID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return enc, nil
}

func (s *Store) GetApp(projectID int64, name string) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal, gpu_count, inject_s3, model_source, runtime, nodes, framework, autoscale_min, autoscale_max, autoscale_cpu, suspended, environment_id FROM apps WHERE project_id = ? AND name = ?`,
		projectID, name,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal, &a.GPUCount, &a.InjectS3, &a.ModelSource, &a.Runtime, &a.Nodes, &a.Framework, &a.AutoMin, &a.AutoMax, &a.AutoCPU, &a.Suspended, &a.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.GitURL = gitURL.String
	a.GitBranch = gitBranch.String
	return a, nil
}

// GetAppByID looks up an app by its primary key, for code that only has a
// foreign key reference (e.g. a Domain row) and not the owning project's
// name.
func (s *Store) GetAppByID(id int64) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := s.db.QueryRow(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal, gpu_count, inject_s3, model_source, runtime, nodes, framework, autoscale_min, autoscale_max, autoscale_cpu, suspended, environment_id FROM apps WHERE id = ?`,
		id,
	).Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal, &a.GPUCount, &a.InjectS3, &a.ModelSource, &a.Runtime, &a.Nodes, &a.Framework, &a.AutoMin, &a.AutoMax, &a.AutoCPU, &a.Suspended, &a.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.GitURL = gitURL.String
	a.GitBranch = gitBranch.String
	return a, nil
}

func (s *Store) ListApps(projectID int64) ([]App, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal, gpu_count, inject_s3, model_source, runtime, nodes, framework, autoscale_min, autoscale_max, autoscale_cpu, suspended, environment_id FROM apps WHERE project_id = ? ORDER BY name`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var gitURL, gitBranch sql.NullString
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal, &a.GPUCount, &a.InjectS3, &a.ModelSource, &a.Runtime, &a.Nodes, &a.Framework, &a.AutoMin, &a.AutoMax, &a.AutoCPU, &a.Suspended, &a.EnvironmentID); err != nil {
			return nil, err
		}
		a.GitURL = gitURL.String
		a.GitBranch = gitBranch.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteApp(id int64) error {
	res, err := s.db.Exec(`DELETE FROM apps WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppEjected marks an app as ejected from luncur's management.
// SetAppAdopted reverses it.
func (s *Store) SetAppEjected(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppAdopted clears the ejected flag: luncur manages the app again.
func (s *Store) SetAppAdopted(id int64) error {
	res, err := s.db.Exec(`UPDATE apps SET ejected = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetReplicas(id int64, n int) error {
	if n < 0 || n > 20 {
		return fmt.Errorf("replicas must be 0-20, got %d", n)
	}
	res, err := s.db.Exec(`UPDATE apps SET replicas = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetResources sets the app's CPU (milli) and memory (MiB) request+limit.
// 0 clears (no resources rendered).
func (s *Store) SetResources(id int64, cpuMilli, memoryMB int64) error {
	if cpuMilli < 0 || memoryMB < 0 {
		return fmt.Errorf("cpu/memory must be >= 0, got cpu=%d memory=%d", cpuMilli, memoryMB)
	}
	res, err := s.db.Exec(`UPDATE apps SET cpu_milli = ?, memory_mb = ? WHERE id = ?`, cpuMilli, memoryMB, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetWebhookSecret sets (or, with nil/empty, clears) the app's sealed
// webhook secret. nil/empty stores NULL (webhook disabled).
func (s *Store) SetWebhookSecret(id int64, sealed []byte) error {
	var v any
	if len(sealed) > 0 {
		v = sealed
	}
	res, err := s.db.Exec(`UPDATE apps SET webhook_secret = ? WHERE id = ?`, v, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHealthPath sets the HTTP path probed for readiness/liveness.
// "" clears (no probes rendered).
func (s *Store) SetHealthPath(id int64, path string) error {
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("health path must start with '/', got %q", path)
		}
		if len(path) > 256 {
			return fmt.Errorf("health path must be at most 256 characters, got %d", len(path))
		}
		for _, r := range path {
			if r <= ' ' || r == 0x7f {
				return fmt.Errorf("health path must not contain whitespace or control characters")
			}
		}
	}
	res, err := s.db.Exec(`UPDATE apps SET health_path = ? WHERE id = ?`, path, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetBuildPath sets the app's build_path (repo-relative subdirectory used as
// the build context/detection dir). Format validation happens at the
// API/UI layer (server.validBuildPath) before this is called; this just
// persists the already-validated value.
func (s *Store) SetBuildPath(id int64, path string) error {
	res, err := s.db.Exec(`UPDATE apps SET build_path = ? WHERE id = ?`, path, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInjectS3 opts an app in or out of LUNCUR_S3_* env injection from the
// project's external S3 settings.
func (s *Store) SetInjectS3(id int64, on bool) error {
	res, err := s.db.Exec(`UPDATE apps SET inject_s3 = ? WHERE id = ?`, on, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetGPU sets the number of nvidia.com/gpu devices the app requests
// (requests==limits). 0 clears. Allowed for every kind — cron included, so
// scheduled retraining can use GPU nodes.
func (s *Store) SetGPU(id int64, n int64) error {
	if n < 0 || n > 16 {
		return fmt.Errorf("gpu must be 0-16, got %d", n)
	}
	res, err := s.db.Exec(`UPDATE apps SET gpu_count = ? WHERE id = ?`, n, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInternal sets the app's internal flag (ClusterIP Service, no Ingress,
// no public URL). Kind/internal validation happens at the API/UI layer
// (server.validateInternalKind) before this is called; this just persists
// the already-validated value.
func (s *Store) SetInternal(id int64, internal bool) error {
	res, err := s.db.Exec(`UPDATE apps SET internal = ? WHERE id = ?`, internal, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAutoscale sets the app's HPA parameters. Passing 0/0/0 disables
// autoscale; otherwise min must be >= 1, max must be >= min and <= 20, and
// cpu (target average utilization percent) must be 1-100.
func (s *Store) SetAutoscale(id int64, min, max, cpu int) error {
	off := min == 0 && max == 0 && cpu == 0
	valid := off || (min >= 1 && max >= min && max <= 20 && cpu >= 1 && cpu <= 100)
	if !valid {
		return fmt.Errorf("invalid autoscale params min=%d max=%d cpu=%d (want min>=1, max>=min<=20, cpu 1-100, or all zero to disable)", min, max, cpu)
	}
	res, err := s.db.Exec(`UPDATE apps SET autoscale_min = ?, autoscale_max = ?, autoscale_cpu = ? WHERE id = ?`, min, max, cpu, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppSuspended toggles a cron app's suspended flag; render maps it to
// CronJob.Spec.Suspend so the schedule stops/resumes firing. The server
// layer enforces kind=cron; the store only persists the flag.
func (s *Store) SetAppSuspended(id int64, suspended bool) error {
	res, err := s.db.Exec(`UPDATE apps SET suspended = ? WHERE id = ?`, suspended, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAppEnvironmentID re-parents an app to a different environment. Used
// right after CreateApp/CreateGitApp/CreateModelApp (which only take a
// project_id, not an environment_id) so a newly created app lands in the
// caller's resolved environment instead of environment_id=0.
func (s *Store) SetAppEnvironmentID(id, envID int64) error {
	res, err := s.db.Exec(`UPDATE apps SET environment_id = ? WHERE id = ?`, envID, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// appEnvCols is the full apps column list (matching GetApp/GetAppByID/
// ListApps above) used by the environment-scoped app methods below.
const appEnvCols = `id, project_id, name, port, replicas, source_type, git_url, git_branch, ejected, cpu_milli, memory_mb, health_path, kind, schedule, webhook_secret, build_path, internal, gpu_count, inject_s3, model_source, runtime, nodes, framework, autoscale_min, autoscale_max, autoscale_cpu, suspended, environment_id`

func scanAppEnvRow(row *sql.Row) (App, error) {
	var a App
	var gitURL, gitBranch sql.NullString
	err := row.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal, &a.GPUCount, &a.InjectS3, &a.ModelSource, &a.Runtime, &a.Nodes, &a.Framework, &a.AutoMin, &a.AutoMax, &a.AutoCPU, &a.Suspended, &a.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	a.GitURL = gitURL.String
	a.GitBranch = gitBranch.String
	return a, nil
}

// CreateAppInEnv registers an app under an environment's namespace. It also
// sets project_id to the environment's owning project so legacy
// project_id-scoped queries (GetApp, ListApps, ...) keep working unchanged.
func (s *Store) CreateAppInEnv(envID int64, name string, port int, kind, schedule string) (App, error) {
	env, err := s.GetEnvironmentByID(envID)
	if err != nil {
		return App{}, err
	}
	if !validName(name) {
		return App{}, fmt.Errorf("invalid app name %q (lowercase letters, digits, dashes; max 40 chars)", name)
	}
	kind = normalizeAppKind(kind)
	if err := validateAppKind(kind, port, schedule); err != nil {
		return App{}, err
	}
	res, err := s.db.Exec(
		`INSERT INTO apps (project_id, environment_id, name, source_type, port, kind, schedule)
		 VALUES (?, ?, ?, 'tarball', ?, ?, ?)`,
		env.ProjectID, envID, name, port, kind, schedule,
	)
	if err != nil {
		return App{}, fmt.Errorf("insert app: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return App{}, err
	}
	return App{
		ID: id, ProjectID: env.ProjectID, EnvironmentID: envID, Name: name, Port: port, Replicas: 1,
		SourceType: "tarball", Kind: kind, Schedule: schedule, Nodes: 1,
	}, nil
}

// ListAppsInEnv returns every app in an environment, ordered by name.
func (s *Store) ListAppsInEnv(envID int64) ([]App, error) {
	rows, err := s.db.Query(`SELECT `+appEnvCols+` FROM apps WHERE environment_id = ? ORDER BY name`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var gitURL, gitBranch sql.NullString
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Port, &a.Replicas, &a.SourceType, &gitURL, &gitBranch, &a.Ejected, &a.CPUMilli, &a.MemoryMB, &a.HealthPath, &a.Kind, &a.Schedule, &a.WebhookSecret, &a.BuildPath, &a.Internal, &a.GPUCount, &a.InjectS3, &a.ModelSource, &a.Runtime, &a.Nodes, &a.Framework, &a.AutoMin, &a.AutoMax, &a.AutoCPU, &a.Suspended, &a.EnvironmentID); err != nil {
			return nil, err
		}
		a.GitURL = gitURL.String
		a.GitBranch = gitBranch.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAppInEnv looks up an app by name within a specific environment.
func (s *Store) GetAppInEnv(envID int64, name string) (App, error) {
	return scanAppEnvRow(s.db.QueryRow(`SELECT `+appEnvCols+` FROM apps WHERE environment_id = ? AND name = ?`, envID, name))
}
