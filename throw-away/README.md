# throw-away — the local test rig

Two self-built mocks for testing the playground's third-party integrations
without any real provider. Each is a single Go file (stdlib only) with a
simple web GUI and **one JSON file as its database** — the name of the
directory is the retirement plan. Mail is no longer mocked: the bundled
send-only relay in the base compose file (the `mailer` service) serves the
local rig too.

| Mock | Replaces | GUI | Database |
|---|---|---|---|
| `oidc-mock/` | your OIDC provider | http://localhost:9400 — identity picker on login, identity manager, login log | `data/oidc-mock.json` (identities, signing key, login events) |
| `loki-mock/` | Loki | http://localhost:3100 — log viewer with filter | `data/loki-mock.json` (last 2000 entries) |

## With docker compose (everything wired)

```sh
cd ..                       # repo root, next to docker-compose.yml
cat > .env <<'EOF'
OIDC_ISSUER=http://localhost:9400
OIDC_CLIENT_ID=playground
OIDC_CLIENT_SECRET=mock-secret
OIDC_ADMIN_CLIENT_ID=playground-admin
OIDC_ADMIN_CLIENT_SECRET=mock-secret
ADMIN_EMAILS=admin@example.com
MAIL_NAME=mail.localhost
EOF
docker compose up -d
```

(The `.env` exists because the base compose file hard-requires the OIDC
variables and MAIL_NAME at parse time; the override file then pins the OIDC
vars to the mock.)

1. **Sign in**: open https://localhost → *Continue with SSO* → the mock's
   identity picker appears — one click, no passwords. Manage identities
   (add/remove) at http://localhost:9400. The administration area at
   https://localhost/admin is gated on the `ADMIN_EMAILS` allowlist (seeded
   on first boot only; manage it under /admin/administrators afterwards).
2. **Mail**: in `/admin/mail`, set SMTP host `mailer`, port `25`, from
   `playground@example.com`, and enable. This is the bundled send-only
   Postfix relay: test sends are REAL mail from this machine, and
   residential ISPs commonly block outbound :25 — deferrals are visible in
   `docker compose logs mailer`. Use a throwaway recipient. `MailApp.read`
   has no local backend (the relay is send-only); it needs an
   admin-configured external IMAP server.
3. **Logger**: in `/admin/logger`, set the push URL to `http://loki-mock:3100`
   and enable; `Logger.log` entries appear at http://localhost:3100. The
   admin Logger → Logs page does NOT read Loki — it tails the app's own
   log file (`data/logs/logger.log`), the operational record of every dispatched
   call.

To sign in as a *specific* user in scripted flows, append identity params to
the `/auth` URL — they bypass the picker and auto-approve:

```
http://localhost:9400/auth?...&sub=mock-user-2&email=second@example.com&name=Second
```

Each distinct `sub` is a distinct playground user with their own
auto-created workspace.

### Why the socat bridge?

The issuer is `http://localhost:9400` because your browser must reach the
`/auth` endpoint — but "localhost" inside the playground container is the
container itself. The `oidc-localhost-bridge` service shares the
playground's network namespace and forwards its `localhost:9400` to the
mock's published port, so one issuer string serves both. go-oidc enforces
issuer/URL equality, so this is the one string that works everywhere.

## Standalone (no docker)

Each mock is `go run .` with env overrides:

```sh
cd oidc-mock && ISSUER=http://127.0.0.1:9400 go run .
```

Then run the server against them (`OIDC_ISSUER=http://127.0.0.1:9400`) and
drive the code flow with curl:

```sh
JAR=$(mktemp)
AUTH=$(curl -s -o /dev/null -w '%{redirect_url}' -c "$JAR" http://localhost:8033/login/go)
curl -s -o /dev/null -L -b "$JAR" -c "$JAR" \
  "${AUTH}&email=me@example.com&name=Me"          # lands on /projects
curl -s -b "$JAR" http://localhost:8033/projects   # authenticated
```

## Notes

- **Restarting the playground** must include the bridge: the socat sidecar
  shares the playground's network namespace, which vanishes when the
  playground container alone is restarted. Use
  `docker compose restart playground oidc-localhost-bridge`
  (or `docker compose up -d`), not `docker compose restart playground`.
- The mocks have **no authentication at all** — never expose them beyond
  localhost / the compose network.
- `x/oauth2` sends client credentials via Basic auth on its first token
  request; oidc-mock reads `client_id` from either place (the `aud` claim
  depends on it).
- oidc-mock persists its RSA signing key, so ID tokens verify across
  restarts; issued one-time codes live in memory.
