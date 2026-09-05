-- Phase A schema: single-tenant projects, saved/versioned multi-file
-- workspaces, deployments, time-driven triggers, execution history, and a
-- best-effort key/value store. See the plan doc for the design rationale,
-- in particular why kv_store is write-side-only in this phase.

CREATE TABLE projects (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One file = one independently compiled/run `.fc` source = one `workflow:`
-- block. A project is an organizational folder of these, not a linked
-- multi-file program (fcc has no observed multi-file support).
CREATE TABLE files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL DEFAULT '',
  position   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(project_id, name)
);

CREATE TABLE versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  number     INTEGER NOT NULL,
  label      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(project_id, number)
);

-- Denormalized name+content (not a files.id FK) so a version snapshot stays
-- readable even after the source file is renamed or deleted.
CREATE TABLE version_files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  version_id INTEGER NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL
);
CREATE INDEX idx_version_files_version ON version_files(version_id);

CREATE TABLE deployments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  slug       TEXT NOT NULL UNIQUE,
  version_id INTEGER REFERENCES versions(id), -- NULL = always run the current draft
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE triggers (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id       INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  file_id          INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  version_id       INTEGER REFERENCES versions(id), -- NULL = always run the current draft
  schedule_type    TEXT NOT NULL, -- 'interval' | 'daily'
  interval_seconds INTEGER,
  daily_time_utc   TEXT,          -- 'HH:MM'
  enabled          INTEGER NOT NULL DEFAULT 1,
  next_run_at      TEXT NOT NULL,
  last_run_at      TEXT,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_triggers_due ON triggers(enabled, next_run_at);

CREATE TABLE executions (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  file_id           INTEGER REFERENCES files(id) ON DELETE SET NULL,
  file_name         TEXT NOT NULL,
  version_id        INTEGER REFERENCES versions(id),
  source            TEXT NOT NULL, -- 'project-run' | 'deployment' | 'trigger'
  deployment_id     INTEGER REFERENCES deployments(id) ON DELETE SET NULL,
  trigger_id        INTEGER REFERENCES triggers(id) ON DELETE SET NULL,
  started_at        TEXT NOT NULL,
  finished_at       TEXT,
  duration_ms       INTEGER,
  compile_exit_code INTEGER,
  compile_stderr    TEXT,
  run_exit_code     INTEGER,
  run_stderr        TEXT,
  timed_out         INTEGER NOT NULL DEFAULT 0,
  truncated         INTEGER NOT NULL DEFAULT 0,
  error             TEXT
);
CREATE INDEX idx_executions_project ON executions(project_id, started_at DESC);

-- Phase A: write-side-only, best-effort log parsed from `store set` calls in
-- the run trace. NOT readable by scripts (no confirmed store-get/read-back
-- mechanism exists in FlowCode's runtime) -- see the plan doc's Phase A/B
-- split. Treat this as a debug/audit log, not a working key-value API yet.
CREATE TABLE kv_store (
  project_id          INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key                 TEXT NOT NULL,
  value               TEXT NOT NULL,
  updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
  source_execution_id INTEGER REFERENCES executions(id) ON DELETE SET NULL,
  PRIMARY KEY (project_id, key)
);
