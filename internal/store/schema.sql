CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin','member')),
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  last_used_at TEXT,
  expires_at   TEXT,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS projects (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  k8s_namespace TEXT NOT NULL UNIQUE,
  gpu_quota     INTEGER NOT NULL DEFAULT 0,
  cpu_quota_milli INTEGER NOT NULL DEFAULT 0,
  mem_quota_mb  INTEGER NOT NULL DEFAULT 0,
  default_env      TEXT NOT NULL DEFAULT 'production',
  preview_base_env TEXT NOT NULL DEFAULT 'develop',
  webhook_secret BLOB,
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('admin','member','viewer')),
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE IF NOT EXISTS environments (
  id              INTEGER PRIMARY KEY,
  project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  k8s_namespace   TEXT NOT NULL,
  kind            TEXT NOT NULL DEFAULT 'standing' CHECK (kind IN ('standing','preview')),
  is_default      INTEGER NOT NULL DEFAULT 0,
  base_branch     TEXT NOT NULL DEFAULT '',
  source_branch   TEXT NOT NULL DEFAULT '',
  last_active_at  TEXT NOT NULL DEFAULT (datetime('now')),
  created_at      TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS apps (
  id            INTEGER PRIMARY KEY,
  project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  environment_id INTEGER NOT NULL DEFAULT 0,
  name          TEXT NOT NULL,
  source_type   TEXT NOT NULL CHECK (source_type IN ('tarball','git')),
  git_url       TEXT,
  git_branch    TEXT,
  git_token_enc BLOB,
  port          INTEGER NOT NULL DEFAULT 8080,
  replicas      INTEGER NOT NULL DEFAULT 1,
  cpu_milli     INTEGER NOT NULL DEFAULT 0,
  memory_mb     INTEGER NOT NULL DEFAULT 0,
  health_path   TEXT NOT NULL DEFAULT '',
  kind          TEXT NOT NULL DEFAULT 'web',
  schedule      TEXT NOT NULL DEFAULT '',
  webhook_secret BLOB,
  build_path    TEXT NOT NULL DEFAULT '',
  internal      INTEGER NOT NULL DEFAULT 0,
  gpu_count     INTEGER NOT NULL DEFAULT 0,
  inject_s3     INTEGER NOT NULL DEFAULT 0,
  model_source  TEXT NOT NULL DEFAULT '',
  runtime       TEXT NOT NULL DEFAULT '',
  nodes         INTEGER NOT NULL DEFAULT 1,
  framework     TEXT NOT NULL DEFAULT '',
  autoscale_min INTEGER NOT NULL DEFAULT 0,
  autoscale_max INTEGER NOT NULL DEFAULT 0,
  autoscale_cpu INTEGER NOT NULL DEFAULT 0,
  suspended     INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, environment_id, name)
);

-- id is an opaque 12-char lowercase base-36 nanoid (see store.NewID), not an
-- autoincrementing integer — it flows unescaped into k8s Job names
-- ("build-<id>") and log/tarball filenames, so it must never leak
-- information about creation order. The table keeps SQLite's implicit
-- rowid for that purpose: ORDER BY rowid gives insertion order the same
-- way ORDER BY id used to, without exposing it. seq (below) remains the
-- only ordering humans ever see.
CREATE TABLE IF NOT EXISTS deployments (
  id               TEXT PRIMARY KEY,
  app_id           INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  seq              INTEGER NOT NULL DEFAULT 0,
  status           TEXT NOT NULL CHECK (status IN ('building','deploying','live','failed')),
  image_ref        TEXT,
  log_path         TEXT,
  created_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  rolled_back_from TEXT
);

CREATE TABLE IF NOT EXISTS env_vars (
  app_id    INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  value_enc BLOB NOT NULL,
  PRIMARY KEY (app_id, key)
);

CREATE TABLE IF NOT EXISTS domains (
  id       INTEGER PRIMARY KEY,
  app_id   INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  hostname TEXT NOT NULL UNIQUE,
  tls      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS overrides (
  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  patch_json TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (app_id, kind)
);

CREATE TABLE IF NOT EXISTS invites (
  token      TEXT PRIMARY KEY,
  role       TEXT NOT NULL CHECK (role IN ('admin','member')),
  expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ssh_keys (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  public_key  TEXT NOT NULL,
  fingerprint TEXT NOT NULL UNIQUE,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS addons (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  environment_id INTEGER NOT NULL DEFAULT 0,
  type       TEXT NOT NULL CHECK (type IN ('postgres','redis','minio','mlflow')),
  name       TEXT NOT NULL,
  version    TEXT NOT NULL,
  size_gb    INTEGER NOT NULL DEFAULT 1,
  creds_enc  BLOB,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS addon_attachments (
  addon_id INTEGER NOT NULL REFERENCES addons(id) ON DELETE CASCADE,
  app_id   INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  PRIMARY KEY (addon_id, app_id)
);

CREATE TABLE IF NOT EXISTS backups (
  id         INTEGER PRIMARY KEY,
  path       TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  uploaded   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS volumes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  path       TEXT NOT NULL,
  size_gb    INTEGER NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(app_id, name),
  UNIQUE(app_id, path)
);

-- Per-project external S3 credentials (endpoint + sealed keys). An
-- in-cluster MinIO addon is the alternative; this table only holds
-- user-supplied external object storage.
CREATE TABLE IF NOT EXISTS project_s3 (
  project_id     INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  endpoint       TEXT NOT NULL,
  region         TEXT NOT NULL DEFAULT '',
  bucket         TEXT NOT NULL,
  access_key_enc BLOB NOT NULL,
  secret_key_enc BLOB NOT NULL
);

-- One row per triggered run of a kind=job app. exit_code is NULL until the
-- run finishes (and stays NULL when no pod exit code could be determined).
CREATE TABLE IF NOT EXISTS job_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  status      TEXT NOT NULL CHECK (status IN ('running','succeeded','failed')),
  nodes       INTEGER NOT NULL DEFAULT 1,
  framework   TEXT NOT NULL DEFAULT '',
  exit_code   INTEGER,
  started_at  TEXT NOT NULL DEFAULT (datetime('now')),
  finished_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_job_runs_app ON job_runs(app_id);

CREATE TABLE IF NOT EXISTS audit_log (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  user_email TEXT NOT NULL,
  action     TEXT NOT NULL,
  target     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at);

CREATE TABLE IF NOT EXISTS sweeps (
  id          TEXT PRIMARY KEY,
  app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  metric      TEXT NOT NULL,
  direction   TEXT NOT NULL CHECK (direction IN ('min','max')),
  max_trials  INTEGER NOT NULL,
  parallel    INTEGER NOT NULL,
  early_stop  INTEGER NOT NULL DEFAULT 0,
  nodes       INTEGER NOT NULL DEFAULT 1,
  framework   TEXT NOT NULL DEFAULT '',
  seed        INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL CHECK (status IN ('running','done','stopped','failed')),
  warning     TEXT NOT NULL DEFAULT '',
  created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_sweeps_app ON sweeps(app_id);

CREATE TABLE IF NOT EXISTS sweep_trials (
  id           TEXT PRIMARY KEY,
  sweep_id     TEXT NOT NULL REFERENCES sweeps(id) ON DELETE CASCADE,
  run_id       INTEGER REFERENCES job_runs(id) ON DELETE SET NULL,
  params_json  TEXT NOT NULL,
  metric_value REAL,
  metric_step  INTEGER,
  state        TEXT NOT NULL CHECK (state IN ('pending','running','done','failed','pruned')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_sweep_trials_sweep ON sweep_trials(sweep_id);

CREATE TABLE IF NOT EXISTS gpu_instances (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  provider    TEXT NOT NULL,
  external_id INTEGER NOT NULL DEFAULT 0,
  external_ref TEXT NOT NULL DEFAULT '',
  label       TEXT NOT NULL,
  gpu_name    TEXT NOT NULL DEFAULT '',
  num_gpus    INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL CHECK (status IN ('renting','active','destroyed')),
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS pipelines (
  id             TEXT PRIMARY KEY,          -- nanoid
  project_id     INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,             -- DNS-1123, unique per project
  yaml           TEXT NOT NULL,
  cron           TEXT NOT NULL DEFAULT '',  -- 5-field, '' = no schedule
  webhook_secret BLOB,                      -- sealed, NULL until generated
  engine         TEXT NOT NULL DEFAULT '',  -- ''=follow global setting
  created_by     INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at     TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(project_id, name)
);

CREATE TABLE IF NOT EXISTS pipeline_runs (
  id          TEXT PRIMARY KEY,
  pipeline_id TEXT NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
  status      TEXT NOT NULL CHECK (status IN ('running','done','failed','stopped')),
  spec_json   TEXT NOT NULL,                -- compiled snapshot; immutable
  trigger     TEXT NOT NULL CHECK (trigger IN ('manual','cron','webhook')),
  warning     TEXT NOT NULL DEFAULT '',     -- sticky operator note
  started_at  TEXT NOT NULL DEFAULT (datetime('now')),
  finished_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_pipeline_runs_pipeline ON pipeline_runs(pipeline_id);

CREATE TABLE IF NOT EXISTS pipeline_run_steps (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('app','image','deploy','scale','notify')),
  state       TEXT NOT NULL CHECK (state IN ('pending','running','done','failed','skipped')),
  job_run_id  INTEGER REFERENCES job_runs(id) ON DELETE SET NULL,  -- kind=app
  attempt     INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT '',     -- error text / action result
  started_at  TEXT,
  finished_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_pipeline_run_steps_run ON pipeline_run_steps(run_id);

-- SQLite does not index foreign keys automatically. The columns below are
-- queried on hot paths and are not the leading column of any PRIMARY KEY or
-- UNIQUE constraint, so without these each lookup — and each ON DELETE
-- CASCADE — is a full table scan.
--
-- Only genuinely uncovered columns are listed. apps(project_id),
-- addons(project_id), environments(project_id), pipelines(project_id) and
-- volumes(app_id) already lead a UNIQUE constraint; env_vars(app_id) and
-- overrides(app_id) already lead their PRIMARY KEY; api_tokens(hash) is
-- UNIQUE (the auth path); project_s3(project_id) is the PRIMARY KEY.
-- ponytail: ssh_keys(user_id) and api_tokens(user_id) are left unindexed —
-- both hold a handful of rows per user; add them if that stops being true.

-- LatestDeployment ("WHERE app_id = ? ORDER BY rowid DESC LIMIT 1") and the
-- MAX(seq) subquery on every deploy create both hit this. deployments is the
-- one table here that grows without bound.
CREATE INDEX IF NOT EXISTS idx_deployments_app ON deployments(app_id);

-- Read per app page render and walked by the cert sweep.
CREATE INDEX IF NOT EXISTS idx_domains_app ON domains(app_id);

-- PRIMARY KEY (project_id, user_id) covers project_id only; "list the
-- projects this user belongs to" filters on user_id.
CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members(user_id);

-- PRIMARY KEY (addon_id, app_id) covers addon_id only; attachments are read
-- per app when building an app's env.
CREATE INDEX IF NOT EXISTS idx_addon_attachments_app ON addon_attachments(app_id);

-- Token usage of the built-in AI assistant, one row per (UTC day, user,
-- workflow), upserted after every model call. Feeds the ai_daily_token_budget
-- check and `luncur ai usage`. user_id 0 = system (notification summaries).
CREATE TABLE IF NOT EXISTS ai_usage (
  day           TEXT NOT NULL,
  user_id       INTEGER NOT NULL,
  workflow      TEXT NOT NULL,
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  requests      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, user_id, workflow)
);

-- Per-app reliability/rollout policy. A side table rather than apps columns:
-- migrateAppsUniqueScope's legacy rebuild copies a fixed apps column list, so
-- new apps columns would be dropped on old installs. A missing row means
-- every default below (see store.DefaultAppPolicy).
CREATE TABLE IF NOT EXISTS app_policies (
  app_id             INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
  auto_rollback      INTEGER NOT NULL DEFAULT 1,
  rollout_timeout    INTEGER NOT NULL DEFAULT 300,
  probe              TEXT NOT NULL DEFAULT 'auto',
  security           TEXT NOT NULL DEFAULT 'baseline',
  strategy           TEXT NOT NULL DEFAULT 'rolling',
  canary_steps       TEXT NOT NULL DEFAULT '10,50',
  canary_interval    INTEGER NOT NULL DEFAULT 120,
  canary_min_success INTEGER NOT NULL DEFAULT 99,
  bluegreen_keep     INTEGER NOT NULL DEFAULT 600
);

-- What the rollout gate learned about a deploy: why it failed (one line,
-- the 3-line error's "what broke") and when its new pods became ready.
-- A side table for the same reason as app_policies (deployments has a
-- legacy rebuild too).
CREATE TABLE IF NOT EXISTS deployment_outcomes (
  deploy_id   TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
  fail_reason TEXT NOT NULL DEFAULT '',
  ready_at    TEXT NOT NULL DEFAULT '',
  ready       INTEGER NOT NULL DEFAULT 0,
  want        INTEGER NOT NULL DEFAULT 0
);

-- One row per canary/blue-green deploy: the step machine's state, so the UI
-- can show progress and a restart resumes from the recorded step.
CREATE TABLE IF NOT EXISTS rollouts (
  deploy_id   TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
  app_id      INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  strategy    TEXT NOT NULL,
  step        INTEGER NOT NULL DEFAULT 0,
  weight      INTEGER NOT NULL DEFAULT 0,
  phase       TEXT NOT NULL DEFAULT 'starting',
  step_started_at TEXT NOT NULL DEFAULT (datetime('now')),
  probes_ok   INTEGER NOT NULL DEFAULT 0,
  probes_total INTEGER NOT NULL DEFAULT 0,
  note        TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_rollouts_app ON rollouts(app_id);

-- Webhook delivery ids already handled (GitHub/Gitea/GitLab/luncur), so a
-- replayed signed request is a no-op. Pruned after 72h.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id      TEXT PRIMARY KEY,
  seen_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Uptime checks: one per web app that has one enabled.
CREATE TABLE IF NOT EXISTS uptime_checks (
  app_id     INTEGER PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
  enabled    INTEGER NOT NULL DEFAULT 1,
  external   INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'unknown',
  fail_streak INTEGER NOT NULL DEFAULT 0,
  ok_streak  INTEGER NOT NULL DEFAULT 0,
  last_at    TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT ''
);

-- Raw probe results, kept 48h.
CREATE TABLE IF NOT EXISTS uptime_samples (
  app_id     INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  at         TEXT NOT NULL,
  ok         INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  code       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_uptime_samples_app_at ON uptime_samples(app_id, at);

-- Daily roll-ups of uptime_samples, kept 400 days (status page bars).
CREATE TABLE IF NOT EXISTS uptime_daily (
  app_id   INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  day      TEXT NOT NULL,
  ok_count INTEGER NOT NULL DEFAULT 0,
  total    INTEGER NOT NULL DEFAULT 0,
  p95_ms   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (app_id, day)
);

-- Incidents: opened automatically by the uptime prober (app_id set) or by a
-- member for planned maintenance (app_id may be 0).
CREATE TABLE IF NOT EXISTS incidents (
  id          INTEGER PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  app_id      INTEGER NOT NULL DEFAULT 0,
  title       TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
  auto        INTEGER NOT NULL DEFAULT 0,
  opened_at   TEXT NOT NULL DEFAULT (datetime('now')),
  resolved_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_incidents_project ON incidents(project_id, status);

CREATE TABLE IF NOT EXISTS incident_updates (
  id          INTEGER PRIMARY KEY,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  body        TEXT NOT NULL,
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Public status pages, at most one per project.
CREATE TABLE IF NOT EXISTS status_pages (
  project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  slug       TEXT NOT NULL UNIQUE,
  title      TEXT NOT NULL DEFAULT '',
  apps       TEXT NOT NULL DEFAULT '',
  enabled    INTEGER NOT NULL DEFAULT 1
);

-- Hourly per-app resource usage aggregates from metrics-server samples,
-- kept 30 days. Feeds right-sizing and cost insights.
CREATE TABLE IF NOT EXISTS app_usage_hourly (
  app_id        INTEGER NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
  hour          TEXT NOT NULL,
  cpu_p95_milli INTEGER NOT NULL DEFAULT 0,
  cpu_max_milli INTEGER NOT NULL DEFAULT 0,
  mem_max_mb    INTEGER NOT NULL DEFAULT 0,
  samples       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (app_id, hour)
);
