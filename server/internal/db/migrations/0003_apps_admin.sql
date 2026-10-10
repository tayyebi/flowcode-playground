-- GAS-mapping additions: instance administrators, admin-managed system
-- settings (replacing environment variables), per-app call records, and
-- per-user per-app rate-limit overrides.
--
-- Logger-app entries are deliberately NOT stored here: Logger.log ships to
-- the admin-configured Loki instance and lives only there (plus the run's
-- in-memory result). app_calls holds Mail/HTTP call records.

-- System administrators are promoted manually from the database:
--   UPDATE users SET is_admin = 1 WHERE email = 'you@example.com';
-- There is no promote UI on purpose.
ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0;

CREATE TABLE system_settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,          -- JSON blob per settings group
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_by INTEGER REFERENCES users(id) ON DELETE SET NULL
);

-- One row per app invocation that goes through the bridge (MailApp.*,
-- UrlFetchApp.fetch). powers the execution transcript and the per-app admin
-- Logs pages.
CREATE TABLE app_calls (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  execution_id INTEGER NOT NULL REFERENCES ws_executions(id) ON DELETE CASCADE,
  user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
  app          TEXT NOT NULL,        -- 'mail' | 'http' (logger goes to Loki)
  name         TEXT NOT NULL,        -- 'MailApp.sendEmail', 'UrlFetchApp.fetch', ...
  status       TEXT NOT NULL,        -- '250 OK', '200', 'quota-exceeded', 'error: ...'
  request      TEXT,                 -- params + token, capped
  response     TEXT,                 -- result body, capped
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_app_calls_app ON app_calls(app, created_at DESC);
CREATE INDEX idx_app_calls_exec ON app_calls(execution_id);
CREATE INDEX idx_app_calls_user ON app_calls(user_id, created_at DESC);

-- Per-user overrides of the default per-app rate limit. No row (or enabled=0
-- disabling the override semantics is deliberate: enabled gates the APP for
-- this user) — defaults come from system_settings.
CREATE TABLE app_quotas (
  user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  app              TEXT NOT NULL,    -- 'mail' | 'http' | 'logger'
  calls_per_minute INTEGER,          -- NULL = use the app default from settings
  enabled          INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (user_id, app)
);
