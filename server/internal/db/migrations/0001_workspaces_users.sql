-- Multi-tenant SaaS identity: OIDC subjects become `users` rows on first
-- login; every project belongs to a `workspaces` row (a Cloudflare-style
-- account boundary); membership is the sharing primitive, with per-project
-- roles layered on top for granular sharing inside a workspace.
--
-- Instance administrators are NOT users-with-a-flag: they live in their own
-- `administrators` table (see 0002), so platform users and platform admins
-- sit in separate tables by design.
--
-- Timestamps are TEXT holding fixed-format UTC strings (RFC 3339 for values
-- computed in Go, 'YYYY-MM-DD HH:MM:SS' for SQL-side defaults via now_utc()).
-- The scheduler compares next_run_at lexicographically, which both layouts
-- support within themselves; keep the two formats off the same column.

-- now_utc() is the SQL-side default clock, byte-compatible with SQLite's
-- datetime('now') output this schema inherits its conventions from.
CREATE FUNCTION now_utc() RETURNS text
  LANGUAGE sql STABLE
  AS $$ SELECT to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH:MM:SS') $$;

CREATE TABLE users (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subject       TEXT NOT NULL UNIQUE, -- OIDC `sub` claim
  email         TEXT NOT NULL DEFAULT '',
  name          TEXT NOT NULL DEFAULT '',
  picture       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL DEFAULT now_utc(),
  last_login_at TEXT
);

CREATE TABLE workspaces (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,     -- URL segment
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT now_utc()
);

-- role: 'owner' | 'admin' | 'member'. Only owners/admins manage members and
-- delete the workspace; members create projects and share them internally.
CREATE TABLE workspace_members (
  workspace_id BIGINT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role         TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
  created_at   TEXT NOT NULL DEFAULT now_utc(),
  PRIMARY KEY (workspace_id, user_id)
);
CREATE INDEX idx_wsm_user ON workspace_members(user_id);

-- Slug is unique only within a workspace (Cloudflare-style: two accounts may
-- both have a project called "api").
CREATE TABLE ws_projects (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  workspace_id BIGINT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
  slug         TEXT NOT NULL,
  name         TEXT NOT NULL,
  description  TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL DEFAULT now_utc(),
  updated_at   TEXT NOT NULL DEFAULT now_utc(),
  UNIQUE(workspace_id, slug)
);
CREATE INDEX idx_wsp_workspace ON ws_projects(workspace_id, updated_at DESC);

-- ON DELETE CASCADE chains give full cleanup from one workspace/project
-- delete.
CREATE TABLE ws_files (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL DEFAULT '',
  position   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT now_utc(),
  updated_at TEXT NOT NULL DEFAULT now_utc(),
  UNIQUE(project_id, name)
);

CREATE TABLE ws_versions (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  number     INTEGER NOT NULL,
  label      TEXT NOT NULL DEFAULT '',
  author_id  BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT now_utc(),
  UNIQUE(project_id, number)
);

-- Denormalized name+content (not a ws_files.id FK) so a version snapshot
-- stays readable even after the source file is renamed or deleted.
CREATE TABLE ws_version_files (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  version_id BIGINT NOT NULL REFERENCES ws_versions(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  content    TEXT NOT NULL
);
CREATE INDEX idx_wsvf_version ON ws_version_files(version_id);

CREATE TABLE ws_deployments (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id    BIGINT NOT NULL REFERENCES ws_files(id) ON DELETE CASCADE,
  slug       TEXT NOT NULL UNIQUE,
  version_id BIGINT REFERENCES ws_versions(id), -- NULL = always run the current draft
  enabled    BOOLEAN NOT NULL DEFAULT true,
  created_at TEXT NOT NULL DEFAULT now_utc(),
  updated_at TEXT NOT NULL DEFAULT now_utc()
);

CREATE TABLE ws_triggers (
  id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id       BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id          BIGINT NOT NULL REFERENCES ws_files(id) ON DELETE CASCADE,
  version_id       BIGINT REFERENCES ws_versions(id), -- NULL = always run the current draft
  schedule_type    TEXT NOT NULL, -- 'interval' | 'daily'
  interval_seconds INTEGER,
  daily_time_utc   TEXT,          -- 'HH:MM'
  enabled          BOOLEAN NOT NULL DEFAULT true,
  next_run_at      TEXT NOT NULL,
  last_run_at      TEXT,
  created_at       TEXT NOT NULL DEFAULT now_utc(),
  updated_at       TEXT NOT NULL DEFAULT now_utc()
);
CREATE INDEX idx_wst_due ON ws_triggers(enabled, next_run_at);

CREATE TABLE ws_executions (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  project_id        BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  file_id           BIGINT REFERENCES ws_files(id) ON DELETE SET NULL,
  file_name         TEXT NOT NULL,
  version_id        BIGINT REFERENCES ws_versions(id),
  source            TEXT NOT NULL, -- 'project-run' | 'deployment' | 'trigger'
  deployment_id     BIGINT REFERENCES ws_deployments(id) ON DELETE SET NULL,
  trigger_id        BIGINT REFERENCES ws_triggers(id) ON DELETE SET NULL,
  actor_id          BIGINT REFERENCES users(id) ON DELETE SET NULL,
  started_at        TEXT NOT NULL,
  finished_at       TEXT,
  duration_ms       INTEGER,
  compile_exit_code INTEGER,
  compile_stderr    TEXT,
  run_exit_code     INTEGER,
  run_stderr        TEXT,
  timed_out         BOOLEAN NOT NULL DEFAULT false,
  truncated         BOOLEAN NOT NULL DEFAULT false,
  error             TEXT
);
CREATE INDEX idx_wse_project ON ws_executions(project_id, started_at DESC);

CREATE TABLE ws_kv_store (
  project_id          BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  key                 TEXT NOT NULL,
  value               TEXT NOT NULL,
  updated_at          TEXT NOT NULL DEFAULT now_utc(),
  source_execution_id BIGINT REFERENCES ws_executions(id) ON DELETE SET NULL,
  PRIMARY KEY (project_id, key)
);

-- Per-project sharing inside a workspace: grants a specific role on one
-- project to a specific member. No row = the viewer's effective role falls
-- back to their workspace role. owner|editor|viewer.
CREATE TABLE project_shares (
  project_id BIGINT NOT NULL REFERENCES ws_projects(id) ON DELETE CASCADE,
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('owner', 'editor', 'viewer')),
  granted_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at TEXT NOT NULL DEFAULT now_utc(),
  PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_psh_user ON project_shares(user_id);

-- Seeders: none for this migration. Users and workspaces are runtime-created
-- by OIDC logins; there is no demo content to pre-populate.
