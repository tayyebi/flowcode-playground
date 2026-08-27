# FlowCode Playground

A web playground for [FlowCode](https://github.com/tayyebi/flowcode): write a
workflow, press Run, and see the compiler's diagnostics, the bytecode it emitted,
and a trace of the VM executing it — without installing a C toolchain.

```
docker compose up -d
```

Then open <http://localhost:8080>.

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
| `PLAYGROUND_RATE_PER_MINUTE` | `30` | Per-IP run budget |
| `PLAYGROUND_RATE_BURST` | `10` | Per-IP burst allowance |
| `TRUST_PROXY` | `0` | Set to `1` **only** behind a reverse proxy you control — see below |

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

Requires Go 1.23+, Node 22+, and a C compiler.

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

`GET /healthz` — `{"status": "ok", "samples": 8}`; backs the compose healthcheck.

---

## Layout

```
runner/fcplay.c    trace driver: cli.c + DEBUG logging + rlimits
server/            Go HTTP server, no module dependencies
  exec.go            sandboxed subprocess execution
  disasm.go          .fcb bytecode decoder
  run.go             POST /api/run
  samples.go         sample loading, GET /api/samples
  ratelimit.go       per-IP token bucket
web/               Vite + CodeMirror 6, no framework
  src/flowcode-lang.js   syntax mode derived from src/compiler.c
  src/share.js           deflate + base64url permalinks
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
