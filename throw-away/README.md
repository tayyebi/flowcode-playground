# throw-away — the local test rig

Local scaffolding for testing the playground without any real third-party
provider. Nothing here ships: the base compose file knows nothing about it,
and the override file that wires it in is purely local. Mail and Loki used
to be mocks too — both are now real and bundled in the base file (the
send-only `mailer` relay and the no-auth `loki`), leaving exactly one piece
of scaffolding behind:

| Piece | Replaces | Where | State |
|---|---|---|---|
| `idaas` (ghcr.io/tayyebi/idaas) | your OIDC provider | https://localhost:9443 — behind nginx, real password logins | `data/idaas/` (signing keys, managed users) |

## With docker compose (everything wired)

```sh
cd ..                       # repo root, next to docker-compose.yml
cat > .env <<'EOF'
OIDC_ISSUER=https://localhost:9443
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
vars to idaas and mounts the rig's certificate into the backends.)

1. **Create accounts** (once — they persist in `data/idaas/managed-users`).
   The admin API answers only from the internal private networks **and**
   only with the bearer token (both configured in the override file):

   ```sh
   curl -k -H "Authorization: Bearer <IDAAS_ADMIN_TOKEN from the override>" \
     -d 'username=admin' -d 'password=…' -d 'name=Admin' \
     -d 'email=admin@example.com' -d 'groups=' \
     https://localhost:9443/api/users
   ```

   The `email` claim of a managed user is the address given here — so
   `admin@example.com` satisfies the `ADMIN_EMAILS` allowlist, and the first
   /admin login binds the account's subject to the allowlist row. A second
   non-allowlisted account (e.g. `dev@example.com`) exercises the user area
   and the "not an administrator" rejection.
2. **Sign in**: open https://localhost → *Continue with SSO* → idaas's real
   username + password form (accept the self-signed certificate on :9443
   like on the main site). The administration area lives at
   https://localhost/admin, gated on the allowlist (seeded from
   `ADMIN_EMAILS` on first boot only; manage it under /admin/administrators).
3. **Mail**: in `/admin/mail`, set SMTP host `mailer`, port `25`, from
   `playground@example.com`, and enable. This is the bundled send-only
   Postfix relay: test sends are REAL mail from this machine, and
   residential ISPs commonly block outbound :25 — deferrals are visible in
   `docker compose logs mailer`. Use a throwaway recipient. `MailApp.read`
   has no local backend (the relay is send-only); it needs an
   admin-configured external IMAP server.
4. **Logger**: in `/admin/logger`, just enable it; the Logger app is
   hard-wired to the bundled `loki`. The admin Logger → Logs page does NOT
   read Loki — it tails the app's own log file (`data/logs/logger.log`),
   the operational record of every dispatched call.

### Scripted login (curl, no browser)

The old mock auto-approved `/auth?sub=&email=` — idaas does real password
logins, so a scripted login parses the CSRF token out of the form:

```sh
JAR=$(mktemp)
AUTH=$(curl -sk -c "$JAR" -o /dev/null -w '%{redirect_url}' https://localhost/login/go)
curl -sk -b "$JAR" -c "$JAR" -o /tmp/login.html "$AUTH"
CSRF=$(grep -o 'name="csrf" value="[^"]*"' /tmp/login.html | cut -d'"' -f4)
RESP=$(curl -sk -b "$JAR" -c "$JAR" -o /dev/null -w '%{redirect_url}' \
  -d "csrf=$CSRF" -d 'username=admin' -d 'password=…' \
  https://localhost:9443/login)
curl -sk -b "$JAR" -c "$JAR" -L -o /dev/null "$RESP"   # lands on /projects
```

Mind the lockout: 5 failed passwords per username start an exponential
backoff (15 s doubling, up to 15 min) — a mistyped scripted password then
blocks even the correct one for a while.

### Why nginx + the socat bridges?

idaas has **no published port**; nginx fronts it at
`https://localhost:9443` with the same self-signed certificate as the main
site, so the browser reaches it like any other page. The issuer is that
same URL — but "localhost" inside each Go container is the container
itself, so one socat sidecar per binary shares its network namespace and
forwards its `localhost:9443` to nginx. go-oidc enforces issuer/URL
equality, so this is the one string that works everywhere. The backends
trust the certificate via `SSL_CERT_FILE` (Go's root-pool override) — no
code, no cert imports.

## Notes

- **Restarting a backend** must include its bridge: the socat sidecar
  shares the backend's network namespace, which vanishes when the backend
  container alone is restarted. Use
  `docker compose restart playground admin oidc-localhost-bridge admin-oidc-localhost-bridge`,
  not single-service restarts.
- The binaries now start listening **before** OIDC discovery succeeds
  (background retry, forever): a cold `docker compose up` used to deadlock
  — the backends' issuer is reachable only through nginx/the bridges, which
  wait on the backends' health. `/login` answers "sign-in provider is
  still starting" until discovery lands (usually seconds).
- OIDC clients are registered in `data/idaas/clients.conf` (the two
  `mock-secret` registrations matching the override file); idaas matches
  redirect URIs exactly and reloads the file on container restart.
- The admin API is unreachable from outside the compose network (idaas is
  unpublished), answers only to the internal private ranges, and requires
  the bearer token — still, treat the token as local-dev-only.
- idaas requires a `nonce` on the authorize request; both binaries send
  one and verify the returned ID token carries it (cookie-bound, like the
  state).
