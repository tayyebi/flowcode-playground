-- Phase B schema: multi-tenant SaaS identity. OIDC subjects become `users`
-- rows on first login; every project belongs to a `workspaces` row (a
-- Cloudflare-style account boundary, addressed in URLs as /#/w/<slug>/...);
-- membership is the sharing primitive, with per-project roles layered on top
-- for granular sharing inside a workspace.
--
-- The existing single-tenant `projects` table is NOT dropped: its rows remain
-- readable via the break-glass admin token (workspace_id stays NULL =
-- "legacy, admin-only") so an instance upgrades without data loss. All new
-- API surface creates workspace-scoped projects in `ws_projects`.

CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  subject       TEXT NOT NULL UNIQUE, -- OIDC `sub` claim
  email         TEXT NOT NULL DEFAULT '',
  name          TEXT NOT NULL DEFAULT '',
  picture       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL DEFAULT (datetime('now')),
  last_login_at TEXT
);

CREATE TABLE workspaces (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  slug       TEXT NOT NULL UNIQUE,     -- URL segment: /#/w/<slug>/...
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- role: 'owner' | 'admin' | 'member'. Only owners/admins manage members and
-- delete the workspace; members create projects and share them internally.
CREATE TABLE workspace_members (
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role         TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX idx_wsm_user ON workspace_members(user_id);

-- Workspace-scoped replacement for `projects`. Same shape plus workspace_id
-- and created_by; slug is unique only within a workspace (Cloudflare-style:
-- two accounts may both have a project called "api").
CREATE TABLE ws_projects (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  created_by   INTEGER REFERENCES users(id) ON DELETE SET NULL,
  slug         TEXT NOT NULL,
  name         TEXT NOT NULL,
  description  TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(workspace_id, slug)
);
CREATE INDEX idx_wsp_workspace ON ws_projects(workspace_id, updated_at DESC);

-- Content tables mirror phase A but hang off ws_projects. ON DELETE CASCADE
-- chains give us full cleanup from one workspace/project delete.
CREATE TABLE ws_files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL DEFAULT '',
  position   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(project_id, name)
);

CREATE TABLE ws_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  number     INTEGER NOT NULL,
  label      TEXT NOT NULL DEFAULT '',
  author_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(project_id, number)
);

CREATE TABLE ws_version_files (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  version_id INTEGER NOT NULL REFERENCES ws_versions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL
);
CREATE INDEX idx_wsvf_version ON ws_version_files(version_id);

CREATE TABLE ws_deployments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id    INTEGER NOT NULL REFERENCES ws_files(id) ON DELETE CASCADE,
  slug       TEXT NOT NULL UNIQUE,
  version_id INTEGER REFERENCES ws_versions(id), -- NULL = always run the current draft
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE ws_triggers (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id       INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id          INTEGER NOT NULL REFERENCES ws_files(id) ON DELETE CASCADE,
  version_id       INTEGER REFERENCES ws_versions(id),
  schedule_type    TEXT NOT NULL, -- 'interval' | 'daily'
  interval_seconds INTEGER,
  daily_time_utc   TEXT,          -- 'HH:MM'
  enabled          INTEGER NOT NULL DEFAULT 1,
  next_run_at      TEXT NOT NULL,
  last_run_at      TEXT,
  created_at       TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_wst_due ON ws_triggers(enabled, next_run_at);

CREATE TABLE ws_executions (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id        INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id           INTEGER REFERENCES ws_files(id) ON DELETE SET NULL,
  file_name         TEXT NOT NULL,
  version_id        INTEGER REFERENCES ws_versions(id),
  source            TEXT NOT NULL, -- 'project-run' | 'deployment' | 'trigger'
  deployment_id     INTEGER REFERENCES ws_deployments(id) ON DELETE SET NULL,
  trigger_id        INTEGER REFERENCES ws_triggers(id) ON DELETE SET NULL,
  actor_id          INTEGER REFERENCES users(id) ON DELETE SET NULL,
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
CREATE INDEX idx_wse_project ON ws_executions(project_id, started_at DESC);

CREATE TABLE ws_kv_store (
  project_id          INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  key                 TEXT NOT NULL,
  value               TEXT NOT NULL,
  updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
  source_execution_id INTEGER REFERENCES ws_executions(id) ON DELETE SET NULL,
  PRIMARY KEY (project_id, key)
);

-- Per-project sharing inside a workspace: grants a specific role on one
-- project to a specific member. No row = the viewer's effective role falls
-- back to their workspace role. owner|editor|viewer.
CREATE TABLE project_shares (
  project_id INTEGER NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('owner', 'editor', 'viewer')),
  granted_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_psh_user ON project_shares(user_id);
