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
  created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS project_members (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('admin','member')),
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE IF NOT EXISTS apps (
  id            INTEGER PRIMARY KEY,
  project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
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
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE (project_id, name)
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

CREATE TABLE IF NOT EXISTS gpu_instances (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  provider    TEXT NOT NULL,
  external_id INTEGER NOT NULL,
  label       TEXT NOT NULL,
  gpu_name    TEXT NOT NULL DEFAULT '',
  num_gpus    INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL CHECK (status IN ('renting','active','destroyed')),
  created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
