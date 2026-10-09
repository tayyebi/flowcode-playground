-- Bookkeeping for the example seeder: one row per project example that has
-- been seeded into this database. Its presence — not the absence of a project
-- with the example's slug — is what makes seeding "exactly once", so a project
-- the admin deletes through the UI stays deleted across restarts.
CREATE TABLE seeded_examples (
    name      TEXT PRIMARY KEY,   -- example directory name (e.g. "watchtower")
    slug      TEXT NOT NULL,      -- slug of the project that was seeded
    project_id INTEGER NOT NULL,
    hash      TEXT NOT NULL,      -- sha256 over the example's files + manifest
    seeded_at TEXT NOT NULL DEFAULT (datetime('now'))
);
