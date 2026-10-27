-- GAS-mapping additions: admin-managed system settings (one JSONB blob per
-- group), per-app call records, per-user per-app rate-limit overrides, and
-- the separate `administrators` allowlist for the administration area.
--
-- Logger-app entries are deliberately NOT stored here: Logger.log ships to
-- the admin-configured Loki instance and lives only there (plus the run's
-- in-memory result). app_calls holds Mail/HTTP call records.

CREATE TABLE system_settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,           -- one JSON blob per settings group;
                                      -- internal __ keys hold raw hex tokens
  updated_at TEXT NOT NULL DEFAULT now_utc(),
  updated_by BIGINT REFERENCES users(id) ON DELETE SET NULL
);

-- Seeder: baseline rows for every settings group, mirroring the defaults in
-- internal/settings so the database is fully populated from the first boot
-- and the admin area edits real rows rather than implicit code defaults.
-- Internal __ keys (session secret, service token, admin session secret)
-- cannot be seeded from SQL — they are random per install and generated at
-- binary boot via settings.Manager.EnsureInternal.
INSERT INTO system_settings (key, value) VALUES
  ('mail', '{"enabled":false,"smtpHost":"","smtpPort":587,"smtpUsername":"","smtpFrom":"","imapHost":"","imapPort":993,"imapTLS":true,"imapUsername":""}'),
  ('http', '{"enabled":true,"allowedDomains":[],"timeoutSeconds":10,"responseCapBytes":65536}'),
  ('logger', '{"enabled":false,"pushUrl":"","username":"","tenant":"","timeoutSeconds":5}'),
  ('quotas', '{"mailPerMinute":10,"httpPerMinute":30,"loggerPerMinute":60}'),
  ('run', '{"timeoutSeconds":5,"maxConcurrent":4,"ratePerMinute":30,"rateBurst":10,"deployRatePerMinute":60,"deployRateBurst":20,"outputCapBytes":65536}')
ON CONFLICT (key) DO NOTHING;

-- One row per app invocation that goes through the bridge (MailApp.*,
-- UrlFetchApp.fetch). Powers the execution transcript and the per-app admin
-- Logs pages.
CREATE TABLE app_calls (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  execution_id BIGINT NOT NULL REFERENCES ws_executions(id) ON DELETE CASCADE,
  user_id      BIGINT REFERENCES users(id) ON DELETE SET NULL,
  app          TEXT NOT NULL,        -- 'mail' | 'http' (logger goes to Loki)
  name         TEXT NOT NULL,        -- 'MailApp.sendEmail', 'UrlFetchApp.fetch', ...
  status       TEXT NOT NULL,        -- '250 OK', '200', 'quota-exceeded', 'error: ...'
  request      TEXT,                 -- params + token, capped
  response     TEXT,                 -- result body, capped
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL DEFAULT now_utc()
);
CREATE INDEX idx_app_calls_app ON app_calls(app, created_at DESC);
CREATE INDEX idx_app_calls_exec ON app_calls(execution_id);
CREATE INDEX idx_app_calls_user ON app_calls(user_id, created_at DESC);

-- Per-user overrides of the default per-app rate limit. No row = use the
-- app default from settings; enabled=false disables the app for this user.
CREATE TABLE app_quotas (
  user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  app              TEXT NOT NULL,    -- 'mail' | 'http' | 'logger'
  calls_per_minute INTEGER,          -- NULL = use the app default from settings
  enabled          BOOLEAN NOT NULL DEFAULT true,
  PRIMARY KEY (user_id, app)
);

-- The administration area's login allowlist. Identity comes from the admin
-- OIDC client; a row may exist before the person ever signed in (added by
-- email in the admin UI) — subject stays NULL until their first login binds
-- it. Users and administrators are entirely separate tables.
CREATE TABLE administrators (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subject       TEXT UNIQUE,         -- OIDC `sub` claim; NULL until first login
  email         TEXT NOT NULL,
  name          TEXT NOT NULL DEFAULT '',
  picture       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL DEFAULT now_utc(),
  last_login_at TEXT
);
CREATE UNIQUE INDEX idx_administrators_email ON administrators(lower(email));

-- Seeder: none from SQL. The initial allowlist comes from the admin
-- service's ADMIN_EMAILS environment variable at first boot (seeding only
-- while the table is empty) and is managed afterwards from the admin UI.
