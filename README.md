# FlowCode Playground

A platform for [FlowCode](https://github.com/tayyebi/flowcode): write a
workflow, press Run, and see the compiler's diagnostics, the bytecode it
emitted, and a trace of the VM executing it — without installing a C
toolchain. Beyond the anonymous one-shot playground, it also lets you save
projects, keep versioned snapshots, deploy a file as a public HTTP endpoint,
schedule time-driven triggers, and inspect a best-effort log of what a
workflow's `store set` calls wrote.

```
docker compose up -d
```

Then open <http://localhost:8080>.

## Two ways to use this

**The anonymous playground** (`/`, or the "Playground" nav link) is unchanged
from before: paste a program, press Run, nothing is saved, nothing is logged.

**Projects** (`/#/dashboard`) are saved, named workspaces: multiple
independently-runnable `.fc` files, "Save Version" snapshots, deployments,
and time-driven triggers. See [Projects, deployments, triggers, and the KV
log](#projects-deployments-triggers-and-the-kv-log) below for the important
limitations before relying on any of this for something real.

---

## What you get

**Three views of the same program**, because compiling and running FlowCode
produces three genuinely different kinds of information:

| Tab | Shows |
|---|---|
| **Trace** | The VM's execution log — instruction count, every plugin invocation in order, and whether the workflow completed |
| **Bytecode** | The decoded `.fcb` image: each instruction, its opcode, and its argument, with `ROUTE`/`LOOP` jump targets rendered as clickable links |
| **Diagnostics** | `fcc`'s errors and warnings, each one clickable to its line and mirrored as an inline marker in the editor |

Plus a picker for all eight bundled sample workflows, syntax highlighting built
from the compiler's own token rules, and shareable permalinks.

### Why warnings matter here

FlowCode has **no comment syntax**. Both `# ...` and `// ...` compile to
`warning: unrecognized line`, and compilation still exits 0 — so a mistyped line
is silently dropped from a program that otherwise looks like it worked. The
Diagnostics tab carries a badge and the editor gets inline markers for exactly
this reason: a warning is not a non-event in this language.

### Why the playground ships its own runtime driver

Running a workflow with the stock `flowcode run` prints *nothing* on success.
The runtime's log level defaults to `FC_LOG_WARN`, and everything worth seeing —
`vm starting`, each builtin plugin call, `vm completed successfully` — is logged
at INFO or DEBUG.

So [`runner/fcplay.c`](runner/fcplay.c) is a ~60-line driver: flowcode's own
`src/cli.c` with the log level turned down to DEBUG and resource limits
installed on itself. It links against flowcode's sources using only public
headers, and the Docker build fails if its trace ever stops appearing.

---

## Running it

### From GHCR (the default)

[`docker-compose.yml`](docker-compose.yml) pulls
`ghcr.io/tayyebi/flowcode-playground:latest` and runs it with the containment
settings described below.

```sh
docker compose up -d
docker compose logs -f
docker compose down
```

Configuration, all optional:

| Variable | Default | Meaning |
|---|---|---|
| `PLAYGROUND_PORT` | `8080` | Host port to publish |
| `PLAYGROUND_TIMEOUT` | `5s` | Wall-clock limit for one compile or one run |
| `PLAYGROUND_MAX_CONCURRENT` | `4` | Simultaneous compile+run slots; beyond this, requests queue then 503 |
| `PLAYGROUND_RATE_PER_MINUTE` | `30` | Per-IP run budget for the anonymous playground |
| `PLAYGROUND_RATE_BURST` | `10` | Per-IP burst allowance for the anonymous playground |
| `PLAYGROUND_DEPLOY_RATE_PER_MINUTE` | `60` | Per-IP budget for public `/deploy/{slug}` calls, tracked separately so deploy traffic can't starve (or be starved by) the playground's own budget |
| `PLAYGROUND_DEPLOY_RATE_BURST` | `20` | Burst allowance for `/deploy/{slug}` |
| `TRUST_PROXY` | `0` | Set to `1` **only** behind a reverse proxy you control — see below |
| `PLAYGROUND_DB_PATH` | `/data/playground.db` | Where the SQLite database (projects, versions, deployments, triggers, executions, kv log) lives. Needs a writable, **persistent** path — see the compose file's `playground-data` volume |
| `PLAYGROUND_ADMIN_TOKEN` | *(unset)* | A single shared secret gating `/api/projects...`. Unset means those routes are open to anyone who can reach the server — same posture as before Projects existed. Never gates `/api/run`, `/api/samples`, `/healthz`, or a deployment's public URL |

### Building from source

```sh
docker compose -f docker-compose.yml -f docker-compose.build.yml up --build
```

The image builds FlowCode from source. Pin the revision for a reproducible
build — `main` is the convenient default, not the reproducible one:

```sh
FLOWCODE_REF=v0.1.0 docker compose \
  -f docker-compose.yml -f docker-compose.build.yml up --build
```

`FLOWCODE_REF` takes any tag, branch, or commit SHA, and `FLOWCODE_REPO` points
the build at a fork.

### Without Docker

Requires Go 1.25+, Node 22+, and a C compiler.

```sh
# 1. Build FlowCode and the trace driver
git clone https://github.com/tayyebi/flowcode.git ../flowcode
make -C ../flowcode
cc -std=c11 -O2 -I../flowcode/include -o /tmp/fcplay \
   $(ls ../flowcode/src/*.c | grep -Ev '/(cli|compiler)\.c$') runner/fcplay.c -ldl

# 2. Build the frontend
cd web && npm ci && npm run build && cd ..

# 3. Run the server
cd server && go build -o /tmp/playground . && cd ..
FLOWCODE_FCC=../flowcode/fcc \
FLOWCODE_RUNNER=/tmp/fcplay \
FLOWCODE_SAMPLES_DIR=../flowcode/samples \
PLAYGROUND_WORKDIR=/tmp/play \
PLAYGROUND_WEB_ROOT=web/dist \
PLAYGROUND_DB_PATH=/tmp/playground.db \
/tmp/playground
```

For frontend work, `cd web && npm run dev` serves on :5173 and proxies `/api` to
:8080, so the editor reloads without rebuilding anything else.

---

## Security model

The playground compiles and executes code submitted by strangers. Two things
make that tractable, and one caveat qualifies both.

**FlowCode's runtime does no I/O.** All 17 builtin plugins in `src/builtins.c`
are pass-throughs that log and forward their token — a shipped `http.post` that
actually posted would be a surprise, so upstream made sure it doesn't. The `os`
plugin that *does* perform shell execution and network calls is not built by
`make all`, is not copied into the image, and cannot be loaded anyway: the
`flowcode` CLI has no plugin flag.

**The container is locked down regardless.** Because "the interpreter is safe"
should never be the only thing between the internet and a host:

- runs as an unprivileged user (uid 10001), `cap_drop: ALL`, `no-new-privileges`
- read-only root filesystem; the only writable paths are `noexec,nosuid,nodev`
  tmpfs mounts
- each submission gets its own work directory, removed when the run ends
  including on timeout
- `fcplay` installs `RLIMIT_CPU` (2s), `RLIMIT_AS` (256 MB), `RLIMIT_FSIZE`,
  `RLIMIT_NOFILE`, and `RLIMIT_NPROC` on itself
- a wall-clock timeout kills the child's whole **process group** — rlimits alone
  are not enough, since `RLIMIT_CPU` counts CPU time and a blocked process burns
  none
- output is capped at 64 KB per stream, source at 64 KB, plus per-IP rate
  limiting and a bounded number of concurrent runs
- children run with a bare environment, so nothing from the server's own env
  reaches them

The timeout is load-bearing rather than theoretical: `exec_loop` and
`exec_route` in flowcode's VM assign the jump target unconditionally and there
is no instruction budget, so a workflow that jumps backwards runs forever.

**The caveat:** this is defence in depth on a shared kernel, not a virtualisation
boundary. For an untrusted public deployment, put it behind a proxy you control
and consider running it in a VM or under gVisor.

**On `TRUST_PROXY`:** leave it at `0` unless a reverse proxy you control sets
`X-Forwarded-For`. Honouring that header unconditionally lets any client forge
it and get a fresh rate-limit bucket per request — worse than having no limit,
because it looks like one is working.

Submitted programs are never written to the server's logs, and permalinks live
in the URL fragment, which browsers do not send to the server at all.

---

## API

`POST /api/run` — `{"source": "..."}`

A program that fails to compile is a normal outcome and comes back as **200**
with the details in the body. Non-2xx means the *request* was refused: `400`
malformed or empty, `413` over 64 KB, `429` rate limited, `503` too busy.

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

`GET /api/samples` — the bundled workflows, as `[{id, name, description, source}]`.

`GET /healthz` — `{"status": "ok", "samples": 8}`; also pings the database. Backs
the compose healthcheck.

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

`ANY /deploy/{slug}` is public (same posture as `/api/run`, its own rate
limit) — see the limitations below before using it for anything real.

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
- **The KV log is write-side only.** It's populated by best-effort parsing
  `store set key="..." value="..."` out of a run's trace — there is no
  confirmed `store get`/read-back mechanism in FlowCode's runtime, so a
  workflow cannot read a previously stored value back mid-run. Treat the KV
  panel as a debug/audit log, not a working key-value API.

Lifting the last two limitations needs a real host-input/read-back bridge
into `fcplay` (which is a local file in this repo, so it's ours to extend)
and possibly upstream FlowCode changes — tracked as a later phase, not
attempted here.

## Layout

```
runner/fcplay.c    trace driver: cli.c + DEBUG logging + rlimits
server/            Go HTTP server
  main.go            routing, startup, admin token, health
  run.go             POST /api/run (anonymous playground)
  adminauth.go       shared-secret gate for /api/projects...
  projects.go, files.go, versions.go   project/file/version CRUD
  deployments.go     deployment CRUD + public /deploy/{slug}
  triggers.go        trigger CRUD
  executions.go      execution history
  kv.go              best-effort store-set log
  samples.go         sample loading, GET /api/samples
  ratelimit.go       per-IP token bucket
  internal/engine/     the sandboxed compile+run pipeline (fcc/fcplay), shared
                        by every execution path — playground, projects,
                        deployments, and triggers alike
  internal/db/          SQLite schema + embedded migrations
  internal/store/       typed CRUD over the schema, one file per entity
  internal/scheduler/   next-run-time math + the trigger-firing ticker
web/               Vite + CodeMirror 6, no framework
  src/router.js          hash router: playground / dashboard / project
  src/api.js             fetch wrapper for the Projects API
  src/render-result.js   Trace/Bytecode/Diagnostics rendering, shared by the
                          playground and a project's Run panel
  src/views/             dashboard, project workspace, deployments/triggers/
                          executions/kv panels
  src/flowcode-lang.js   syntax mode derived from src/compiler.c
  src/share.js           deflate + base64url permalinks (playground only)
scripts/smoke.py   end-to-end check against a running instance
```

## Tests

```sh
cd server && go test ./...                    # unit tests
python3 scripts/smoke.py http://localhost:8080  # end-to-end, against a running instance
```

The Go end-to-end tests skip unless `FLOWCODE_FCC` and `FLOWCODE_RUNNER` point
at real binaries; set `FLOWCODE_SAMPLES_DIR` as well to check every bundled
sample. CI builds the toolchain so they always run, and both CI and the publish
workflow run `scripts/smoke.py` against the real image under the same
containment flags compose uses.
