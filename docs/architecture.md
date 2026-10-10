# Flowcode Playground → Google Apps Script — Target Architecture

Architecture principle: **the core listens to nothing and owns no sockets — the
playground owns every socket.** The playground web server is the only listener
(HTTP :8033 for users, per-run unix bridge for app calls); public
`/deploy/{slug}` URLs are the webhook listener, which calls the core when a
request arrives. Playground ⇄ core communicate only over the run pipeline
(subprocess + per-run unix socket).

```
┌─────────────────────────────────────────────────────────────────────────────────────┐
│                                BROWSER (server-rendered, no JS)                     │
│                                                                                     │
│  user:   /login → OIDC · /projects (dashboard, my workspaces) ·                     │
│          /projects/{id} (editor + Run + versions/deployments/triggers/KV) ·         │
│          /projects/{id}/executions/{execId} (trace + bytecode + APP CALLS)          │
│  admin:  /admin/** — sidebar: [Mail: Config·Logs·RateLimits]                        │
│                          [HTTP: Config·Logs·RateLimits] [Logger: Config·Logs]       │
│                          [System: Users·Executions·General]                         │
└──────────────┬──────────────────────────────────────────────┬───────────────────────┘
               │ ① HTML pages + form POSTs                    │ ⑥ admin forms
               │                                              │
               ▼                                              ▼
┌───────────────────────┐   ┌─────────────────────────────────────────────────────────┐
│   OIDC PROVIDER       │◄─►│              GO PLAYGROUND SERVER  :8033                 │
│   (discovery)         │ ① │                                                         │
│   env: OIDC_ISSUER,   │   │  auth.go      OIDC code exchange · UpsertUserBySubject  │
│   OIDC_CLIENT_ID/SEC  │   │               session cookie (HMAC uid|expiry,           │
└───────────────────────┘   │               secret = system_settings.__session_secret)│
                            │               requireUser/requireUserPage on ALL routes  │
   env: boot paths only ───►│                                                         │
   (PORT, DB path, fcc/     │  web.go       embedded templates (pages)                 │
   fcplay paths, workdir,   │  JSON API     same handlers, Bearer __service_token      │
   TRUST_PROXY)             │               (for smoke.py / scripts)                   │
                            │                                                         │
                            │  WS content layer   WS* store methods ·                   │
                            │                     EffectiveProjectRole gate             │
                            │                     (viewer = read-only)                 │
                            │         ▲            legacy projects = admin break-glass │
                            │         │                                               │
                            │  scheduler (15s tick) ── WSDueTriggers ──┐               │
                            │                                         ▼               │
                            │  ENGINE  ────────────────────────────────────────────    │
                            │  per-run tmpdir:  main.fc → [fcc] → main.fcb → [fcplay]  │
                            │  slots/timeouts/output caps (unchanged sandbox)          │
                            │        │2 compile            │3 execute                   │
                            │        │                     │  unix socket bridge.sock  │
                            │        │                     ▼  (length-prefixed JSON:   │
                            │  APPS SERVICE (internal/apps)     name+params+token)     │
                            │  quota check (user,app) → execute → app_calls row        │
                            │                                    (mail/http only)       │
                            │   ├─ MailApp.sendEmail ── net/smtp ──────────► SMTP      │
                            │   ├─ MailApp.read ─────── go-imap ────────► IMAP         │
                            │   ├─ UrlFetchApp.fetch ─ net/http ────► any HTTP         │
                            │   └─ Logger.log ──── Loki push API ──► LOKI              │
                            │       (labels: app·user·project·exec; strict: halts     │
                            │       on unconfigured/unreachable; docker-compose.       │
                            │       override.yml service, http://loki:3100)            │
                            │  SETTINGS SERVICE (cached system_settings,              │
                            │  reloaded on admin save: SMTP/IMAP creds, allowlist,    │
                            │  app toggles, quotas, run limits)                        │
                            │         ▲                                               │
                            │         ▼                                               │
                            │  STORE ── SQLite (modernc, WAL)                          │
                            │  users(+is_admin) · workspaces · workspace_members ·     │
                            │  project_shares · ws_projects/files/versions/deploy-     │
                            │  ments/triggers/executions(actor_id)/kv ·                │
                            │  system_settings · app_calls · app_quotas ·              │
                            │  legacy projects/files/... (admin-only)                  │
                            └───────────────────────────────────────────────────────────┘
                            ▲              ▲                ▲       rlimits: CPU/AS/
                            │              │                │       FSIZE/NOFILE/NPROC
                    ┌───────┴──────┐ ┌─────┴──────────┐ ┌────┴─────────────┐  bare env
                    │ fcc (core)   │ │ fcplay (runner)│ │ ⑤ /deploy/{slug} │
                    │ compiler:    │ │ ABI-v2 bridge: │ │ public webhook    │
                    │ params into  │ │ registers ONLY │ │ listener; actor = │
                    │ CALL blob;   │ │ the 4 app fns; │ │ project owner     │
                    │ webhook=no-op│ │ forwards calls │ └──────────────────┘
                    └──────────────┘ │ over the unix  │
                                     │ socket ④       │   result → token + trace line
                                     └────────────────┘   "app call <name> -> <status>"

┌─────────────────────────────────────────────────────────────────────────────────────┐
│ FLOWCODE CORE (pure compute — listens to nothing, owns no sockets)                  │
│   include/flowcode.h  Plugin ABI v2: fc_call_t {name, token, params, host}          │
│                       fc_vm_set_host() hands plugins a host context pointer         │
│   src/compiler.c      key=value lines under CALL/TRANSFORM compile into the arg     │
│                       blob (name\0k\0v\0…; old name-only images still load);        │
│                       `webhook` = recognized no-op (trigger blocks compile to       │
│                       nothing; --strict trigger runs fixed); bare-name exception    │
│                       removed from is_plugin_call                                   │
│   src/builtins.c      stub list = MailApp.sendEmail · MailApp.read ·                │
│                       UrlFetchApp.fetch · Logger.log (placeholders dropped)         │
│   VM / bytecode layout / registry — unchanged semantics                             │
└─────────────────────────────────────────────────────────────────────────────────────┘

Flows: ① OIDC login → session (first login auto-creates personal workspace + owner role)
       ② Run POST → engine → fcc → fcplay; result page shows trace/bytecode/diagnostics/App calls
       ③ fcplay executes → hits MailApp.*/UrlFetchApp.fetch/Logger.log
       ④ bridge: fcplay ⇄ bridge.sock ⇄ apps service (quota → execute → app_calls)
       ⑤ public deploy URL / scheduler trigger → same engine pipeline (actor = owner)
       ⑥ admin edits settings/quotas → cached reload → next runs use new config
```
