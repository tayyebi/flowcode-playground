# FlowCode Playground

A platform for [FlowCode](https://github.com/tayyebi/flowcode): write a
workflow in a saved project, press Run, and see the compiler's diagnostics,
the bytecode it emitted, and a trace of the VM executing it — without
installing a C toolchain. Projects keep versioned snapshots, deploy a file as
a public HTTP endpoint, schedule time-driven triggers, and record a
best-effort log of what a workflow's `store set` calls wrote.

```
docker compose up -d
```

Then open <http://localhost:8033>.

## Using it

The [dashboard](http://localhost:8033/projects) lists your projects. A
project is a saved, named workspace: multiple independently-runnable `.fc`
files, "Save Version" snapshots, deployments, and time-driven triggers. See
[Projects, deployments, triggers, and the KV
log](#projects-deployments-triggers-and-the-kv-log) below for the important
limitations before relying on any of this for something real.

(The anonymous one-shot playground that used to live at `/` is gone, along
with its sample picker and shareable permalinks.)

---

## What you get

**Three views of the same program**, because compiling and running FlowCode
produces three genuinely different kinds of information:

| Section | Shows |
|---|---|
| **Trace** | The VM's execution log — instruction count, every plugin invocation in order, what each `store set` left behind, and whether the workflow completed |
| **Bytecode** | The decoded `.fcb` image: each instruction, its opcode, and its argument |
| **Diagnostics** | `fcc`'s errors and warnings, each with its line number |

Plus worked examples in the
[wiki](https://github.com/tayyebi/flowcode.wiki) to paste into your first
project.

### Why warnings matter here

FlowCode has **no comment syntax**. Both `# ...` and `// ...` compile to
`warning: unrecognized line`, and compilation still exits 0 — so a mistyped line
is silently dropped from a program that otherwise looks like it worked. The
Diagnostics section exists for exactly this reason: a warning is not a
non-event in this language.

### Why the playground ships its own runtime driver

Running a workflow with the stock `flowcode run` prints *nothing* on success.
The runtime's log level defaults to `FC_LOG_WARN`, and everything worth seeing —
`vm starting`, each builtin plugin call, `vm completed successfully` — is logged
at INFO or DEBUG.

So [`runner/fcplay.c`](runner/fcplay.c) is a small driver: flowcode's own
`src/cli.c` with the log level turned down to DEBUG, resource limits installed
on itself, and — because the VM's STORE opcode logs nothing, being an opcode
rather than a plugin call — a post-run dump of every key a `store set` wrote,
in the one-line format the server's KV log is parsed from. It links against
flowcode's sources using only public headers, and
[`scripts/docker-entrypoint.sh`](scripts/docker-entrypoint.sh) fails the build
if that trace ever stops appearing.

---

## Running it

There is no Dockerfile, no prebuilt image, and no CI. `docker compose up -d`
runs [`scripts/docker-entrypoint.sh`](scripts/docker-entrypoint.sh) inside a
plain `golang:1.25-alpine` base image: it installs the rest of the toolchain
(a C compiler, git) and builds FlowCode and the server, then execs the result.
The repo is bind-mounted into the container, so every
cache (apk, FlowCode's git clone, Go's module/build cache) and
every build artifact lives under `.buildcache/` **on the host** — a rebuild
after a `git pull` only redoes what actually changed, it never redownloads
dependencies from scratch.

**Deploying to a server**, in full:

```sh
git pull
docker compose up -d
docker compose logs -f
```

The first run compiles everything from a cold cache and takes a few minutes;
every run after that is fast, since `.buildcache/` persists between runs.

```sh
docker compose down       # stop the container; .buildcache/ and data/ are untouched
```

Pin FlowCode's revision for a reproducible build — `main` is the convenient
default, not the reproducible one:

```sh
FLOWCODE_REF=v0.1.0 docker compose up -d
```

`FLOWCODE_REF` takes any tag, branch, or commit SHA, and `FLOWCODE_REPO` points
the build at a fork.

Only port **8033** is ever published to the host; everything else the
container does is internal.

Configuration, all optional:

| Variable | Default | Meaning |
|---|---|---|
| `PLAYGROUND_TIMEOUT` | `5s` | Wall-clock limit for one compile or one run |
| `PLAYGROUND_MAX_CONCURRENT` | `4` | Simultaneous compile+run slots; beyond this, requests queue then 503 |
| `PLAYGROUND_DEPLOY_RATE_PER_MINUTE` | `60` | Per-IP budget for public `/deploy/{slug}` calls |
| `PLAYGROUND_DEPLOY_RATE_BURST` | `20` | Burst allowance for `/deploy/{slug}` |
| `TRUST_PROXY` | `0` | Set to `1` **only** behind a reverse proxy you control — see below |
| `PLAYGROUND_ADMIN_TOKEN` | *(unset)* | A single shared secret gating every `/api/projects...` route — with the anonymous playground gone, that is the whole API. Unset means those routes are open to anyone who can reach the server. Never gates `/healthz` or a deployment's public URL |

The port (`8033`) and the SQLite path (`/data/playground.db`, bind-mounted
from `./data` on the host) are fixed in `docker-compose.yml` rather than
configurable — see it directly to change either.

### Without Docker

Requires Go 1.25+ and a C compiler. The UI is server-rendered from templates
embedded in the binary — there is no frontend build and no JavaScript
anywhere.

```sh
# 1. Build FlowCode and the trace driver
git clone https://github.com/tayyebi/flowcode.git ../flowcode
make -C ../flowcode
cc -std=c11 -O2 -I../flowcode/include -o /tmp/fcplay \
   $(ls ../flowcode/src/*.c | grep -Ev '/(cli|compiler)\.c$') runner/fcplay.c -ldl

# 2. Build and run the server
cd server && go build -o /tmp/playground . && cd ..
FLOWCODE_FCC=../flowcode/fcc \
FLOWCODE_RUNNER=/tmp/fcplay \
PLAYGROUND_WORKDIR=/tmp/play \
PLAYGROUND_DB_PATH=/tmp/playground.db \
PORT=8033 \
/tmp/playground
```

---

## Security model

The engine compiles and executes whatever a project author saves — and
unless an admin token is set, anyone who can reach the server is a project
author, and a public `/deploy/{slug}` URL re-runs a stored workflow for
anyone holding it. Two things make that tractable, and one caveat qualifies
both.

**FlowCode's runtime does no I/O.** All 17 builtin plugins in `src/builtins.c`
are pass-throughs that log and forward their token — a shipped `http.post` that
actually posted would be a surprise, so upstream made sure it doesn't. The `os`
plugin that *does* perform shell execution and network calls is not built by
`make all`, is not copied into the image, and cannot be loaded anyway: the
`flowcode` CLI has no plugin flag.

**Defence in depth still applies, but at a different layer.** Because there's
no Dockerfile, the container itself runs as root and with a writable
filesystem — it has to, to install packages and compile on every deploy — so
it does *not* get the uid-10001 / read-only-root / `cap_drop: ALL` hardening
an earlier version of this repo baked into a purpose-built image. What's left
in place, all inside `fcplay` and the server itself, independent of how the
container is set up:

- each submission gets its own work directory, removed when the run ends
  including on timeout
- `fcplay` installs `RLIMIT_CPU` (2s), `RLIMIT_AS` (256 MB), `RLIMIT_FSIZE`,
  `RLIMIT_NOFILE`, and `RLIMIT_NPROC` on itself
- a wall-clock timeout kills the child's whole **process group** — rlimits alone
  are not enough, since `RLIMIT_CPU` counts CPU time and a blocked process burns
  none
- output is capped at 64 KB per stream, file content at 64 KB, plus per-IP
  rate limiting on public deploy URLs and a bounded number of concurrent runs
- children run with a bare environment, so nothing from the server's own env
  reaches them

The timeout is load-bearing rather than theoretical: `exec_loop` and
`exec_route` in flowcode's VM assign the jump target unconditionally and there
is no instruction budget, so a workflow that jumps backwards runs forever.

**The caveat:** this is defence in depth on a shared kernel, not a virtualisation
boundary — and with the container itself running as root, that kernel boundary
is weaker than it was. For an untrusted public deployment, put it behind a
proxy you control and strongly consider running it in a VM or under gVisor.

**On `TRUST_PROXY`:** leave it at `0` unless a reverse proxy you control sets
`X-Forwarded-For`. Honouring that header unconditionally lets any client forge
it and get a fresh rate-limit bucket per request — worse than having no limit,
because it looks like one is working.

Submitted programs are never written to the server's logs.

---

## API

### Run results

`POST /api/projects/{id}/files/{name}/run` returns the engine's result — the
shape every execution path produces (manual runs, triggers, deployments).

A program that fails to compile is a normal outcome and comes back as **200**
with the details in the body. Non-2xx means the *request* was refused: `400`
malformed, `413` over 64 KB, `503` too busy.

```jsonc
{
  "compile": {
    "exitCode": 0,
    "stderr": "",
    "timedOut": false,
    "durationMs": 3,
    "diagnostics": [
      { "level": "warning", "line": 8, "message": "unrecognized line: ..." }
    ]
  },
  "bytecode": {
    "version": 1, "instructionCount": 2, "argBlobSize": 20, "sizeBytes": 54,
    "instructions": [
      { "index": 0, "opcode": "EMIT", "opcodeHex": "0x01",
        "argOffset": 0, "argLength": 12, "arg": "\"hello, world\"" }
    ]
  },
  "run": {
    "exitCode": 0,
    "stderr": "[flowcode:INFO] vm starting, 2 instructions\n...",
    "timedOut": false, "durationMs": 2
  },
  "truncated": false
}
```

`run` is absent when compilation failed. `bytecode` is present whenever `fcc`
produced a readable image — including runs that only warned.

`GET /healthz` — `{"status": "ok"}`; also pings the database. Backs the
compose healthcheck.

### Projects API

Everything under `/api/projects...` is gated by `PLAYGROUND_ADMIN_TOKEN` when
one is set (`POST /api/admin/login {"token": "..."}` trades it for a cookie).
A project is a folder of independently-runnable `.fc` files — **not** a
linked multi-file program: `fcc` only ever compiles one file at a time, so
each file is its own complete workflow, the same way each bundled sample is.

```
GET/POST   /api/projects                                GET/PATCH/DELETE /api/projects/{id}
GET/PUT/DELETE /api/projects/{id}/files/{name}           POST .../files/{name}/run
GET/POST   /api/projects/{id}/versions                   POST .../versions/{n}/restore
GET/POST   /api/projects/{id}/deployments                PATCH/DELETE .../deployments/{id}
GET/POST   /api/projects/{id}/triggers                   PATCH/DELETE .../triggers/{id}
GET        /api/projects/{id}/executions[/{id}]
GET/DELETE /api/projects/{id}/kv[/{key}]
```

`ANY /deploy/{slug}` is public, with its own rate limit — see the
limitations below before using it for anything real.

---

## Projects, deployments, triggers, and the KV log

FlowCode's runtime does no I/O (see [Security model](#security-model) above)
and has no way to accept host-provided input — `fcplay` runs a `.fcb` file
start to finish with no request data, no stdin, and no read-back of anything
a `store set` call wrote. That shapes what these features can honestly do
today:

- **Saved, versioned files** work exactly as you'd expect: multiple files per
  project, immutable "Save Version" snapshots, restore.
- **Time-driven triggers** are fully real: the scheduler re-runs a file on
  its own schedule through the same sandboxed pipeline as everything else,
  independent of any request/response gap, and records an execution row
  each time.
- **Deployments are Phase A only.** Calling a deployment's `/deploy/{slug}`
  URL **ignores the request** — method, query string, and body are all
  discarded — and just re-runs the file's workflow with no parameters,
  returning the raw compile/run result (a note on this ships in the response
  body and an `X-FlowCode-Deploy-Note` header, so this isn't a silent
  surprise). It is not yet a real request/response web endpoint.
- **The KV log is write-side only.** After each run, `fcplay` dumps the final
  value of every key the workflow's `store set` calls wrote into the trace,
  and the server parses those lines into the KV panel (values sanitized to
  quotes-become-apostrophes and truncated at 512 bytes). There is no confirmed
  `store get`/read-back mechanism in FlowCode's runtime, so a workflow cannot
  read a previously stored value back mid-run. Treat the KV panel as a
  debug/audit log, not a working key-value API.

Lifting the last two limitations needs a real host-input/read-back bridge
into `fcplay` (which is a local file in this repo, so it's ours to extend)
and possibly upstream FlowCode changes — tracked as a later phase, not
attempted here.

## Layout

```
runner/fcplay.c    trace driver: cli.c + DEBUG logging + rlimits + store-set dump
server/            Go HTTP server
  main.go            routing, startup, admin token, health
  adminauth.go       shared-secret gate for /api/projects...
  projects.go, files.go, versions.go   project/file/version CRUD
  deployments.go     deployment CRUD + public /deploy/{slug}
  triggers.go        trigger CRUD
  executions.go      execution history
  kv.go              best-effort store-set log
  web.go             the server-rendered UI: HTML form handlers
  web/templates/     Go templates for every page (embedded)
  web/static/        the one stylesheet (embedded)
  ratelimit.go       per-IP token bucket (public deploy URLs)
  internal/engine/     the sandboxed compile+run pipeline (fcc/fcplay), shared
                        by every execution path — projects, deployments,
                        and triggers alike
  internal/db/          SQLite schema + embedded migrations
  internal/store/       typed CRUD over the schema, one file per entity
  internal/scheduler/   next-run-time math + the trigger-firing ticker
scripts/smoke.py             end-to-end check against a running instance
scripts/docker-entrypoint.sh what `docker compose up -d` actually runs: builds
                              FlowCode and the server, then execs the
                              playground binary
```

## Tests

```sh
cd server && go test ./...                    # unit tests
python3 scripts/smoke.py http://localhost:8033  # end-to-end, against a running instance
```

The Go end-to-end tests skip unless `FLOWCODE_FCC` and `FLOWCODE_RUNNER`
point at real binaries — see [Without Docker](#without-docker) for how to
build them locally. There is no CI: run both of the above yourself before
deploying, and run `scripts/smoke.py` against the real instance after
`docker compose up -d`.
