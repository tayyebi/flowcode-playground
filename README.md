# FlowCode Playground

A Google-Apps-Script-style platform for
[FlowCode](https://github.com/tayyebi/flowcode): sign in with your
organization account, write a workflow in a saved project, press Run, and see
the compiler's diagnostics, the bytecode it emitted, a trace of the VM
executing it, and a transcript of every app call it made. Projects keep
versioned snapshots, deploy a file as a public HTTP endpoint (the webhook
trigger), schedule time-driven triggers, and can call the built-in apps —
**MailApp**, **UrlFetchApp**, and **Logger** — whose credentials your system
administrators configure, not the environment.

```
docker compose up -d
```

Then open <http://localhost:8033> and sign in with SSO.

## The user journey (the Apps Script mapping)

| Google Apps Script | FlowCode Playground |
|---|---|
| Google account sign-in | OIDC single sign-on (any discovery-based provider) |
| script.google.com home / My Projects | `/projects` — your personal workspace's projects |
| Editor + Run + execution log | project page: editor, Run, Trace / Bytecode / Diagnostics / **App calls** |
| `MailApp.sendEmail()` | `MailApp.sendEmail` (SMTP, admin-configured) |
| `UrlFetchApp.fetch()` | `UrlFetchApp.fetch` (HTTP, admin-configured allowlist) |
| `Logger.log()` | `Logger.log` (pushed to Loki, admin-configured) |
| Web-app deployment / webhooks | public `/deploy/{slug}` URLs — **the playground listens, the core never does** |
| Time-driven triggers | interval / daily-at-UTC triggers per project file |
| Admin console | `/admin` — per-app Configuration, Logs, and Rate limits pages |

## The three apps

Workflow source reads exactly like Apps Script — call parameters compile
into the bytecode and cross the plugin ABI:

```
workflow: OrderPipeline

step notify:
    MailApp.sendEmail
        to = "alice@example.com"
        subject = "Order shipped"
        body = "It is on the way."
end

step fetchTracking:
    UrlFetchApp.fetch
        url = "https://carrier.example.com/track/123"
        Accept = "application/json"
end

step logged:
    Logger.log
        message = "notification sent"
end
```

- **MailApp.sendEmail** — SMTP delivery through the account the
  administrators configured (`/admin/mail`). Params `to`/`subject`/`body`
  are optional with the current token as fallback.
- **MailApp.read** — IMAP inbox read (`folder`, `limit`); the token becomes
  a JSON array of `{from, subject, date, snippet}`.
- **UrlFetchApp.fetch** — outbound HTTP (`url`, `method`, `body`; every
  other param is a request header). The response body becomes the token,
  capped by the admin-configured size limit.
- **Logger.log** — one log entry pushed to Loki. Nothing is stored locally:
  the fresh-run transcript is in-memory, history lives in Loki.

All four are **strict**: a disabled app, missing credentials, an exhausted
per-user rate limit, or an unreachable backend halts the workflow with a
clear trace line. A logger that silently drops entries is worse than one
that fails loudly.

## Configuration: env is for booting, /admin is for running

The **only application configuration in the environment** is the OIDC
credentials — the server needs them before it can authenticate anything.
Everything else lives in the database (`system_settings`) and is managed by
system administrators in the `/admin` area.

| Env var | Purpose |
|---|---|
| `OIDC_ISSUER` | Provider issuer URL (discovery-based, any provider) — **required** |
| `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | OAuth2 client — **required** |
| `OIDC_REDIRECT_URL` | Defaults to `http://<request-host>/api/auth/callback` |
| `PORT`, `PLAYGROUND_DB_PATH`, `PLAYGROUND_WORKDIR`, `FLOWCODE_FCC`, `FLOWCODE_RUNNER` | Boot paths (sane defaults in the container) |
| `TRUST_PROXY` | `1` only behind a proxy you control |
| `FLOWCODE_REPO` / `FLOWCODE_REF` | Compose build pins |

Admin-managed (`/admin`, stored in SQLite): SMTP/IMAP credentials, the Loki
push URL (optional basic-auth and tenant), the UrlFetchApp allowlist and
caps, per-app default and per-user rate limits, app enable toggles, and
sandbox limits (timeout, concurrency, site rate limits — applied at restart).

### Promoting the first system administrator

There is deliberately no promote UI. Sign in once (so your user row exists),
then:

```sh
sqlite3 data/playground.db "UPDATE users SET is_admin = 1 WHERE email = 'you@example.com';"
```

Reload `/admin` and the full administration sidebar appears. A
`__service_token` (internal setting, rotatable in /admin → General) lets
scripts call the JSON API with `Authorization: Bearer`.

## Logger → Loki (optional)

Logger.log is third-party-backed, like auth (OIDC) and mail (SMTP). Add the
optional override file and a single-tenant Loki comes up next to the
playground:

```
docker compose -f docker-compose.override.yml up -d
```

Then set Logger → push URL to `http://loki:3100` in `/admin/logger`. The
admin Logs page for the Logger app is a viewer over Loki's query API; the
`user`/`project`/`exec` labels make entries filterable. Retention is Loki's
concern (14 days in the bundled config).

## Security model

- **The core listens to nothing.** `fcc` and `fcplay` are pure compute; the
  playground web server owns every socket: HTTP for users, a per-run unix
  socket for app calls, public deploy URLs as the webhook listener.
- Runs execute in a sandboxed subprocess: bare environment, own process
  group, wall-clock timeout, rlimits (CPU, address space, file size,
  nproc), capped output.
- OIDC ID tokens are verified against the provider's JWKS; sessions are
  HMAC-signed cookies with a database-stored secret.
- Every page and API route requires a signed-in user; project access goes
  through workspace membership (`EffectiveProjectRole` — viewers are
  read-only). Foreign project ids return 404, not 403.
- Public `/deploy/{slug}` traffic is rate-limited and attributed to the
  project's owner for quotas and app-call records.
- Secrets (SMTP/IMAP/Loki passwords) are write-only from the admin UI's
  point of view: forms never echo them, an empty field means "keep".

## Projects, deployments, triggers, and the KV log

A project is a set of independently runnable `.fc` files. Versions snapshot
all files; restoring one rewrites them. A deployment publishes one file at a
public URL — any request runs the workflow (the request itself is ignored;
the URL is the webhook trigger). Triggers run a file on an interval or daily
at a UTC time. The KV log is a write-side record of what `store set` calls
left behind — it is not a readable key-value API for scripts.

## Development

```
cd server && go test ./...      # unit + store + scheduler + engine tests
make -C ../flowcode test        # core language tests
```

The server embeds its templates (`server/web/`); there is no frontend build.
`fcc` and `fcplay` are located via `FLOWCODE_FCC` / `FLOWCODE_RUNNER`.
Architecture diagram: [`docs/architecture.md`](docs/architecture.md).
