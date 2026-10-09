# Watchtower — the demo walkthrough

Watchtower is a seeded demo project: an uptime-monitoring suite whose three
workflows are wired to every feature of the playground. It seeds itself
automatically the first time a fresh database starts (see the README's
"Seeding" section); no manual step is involved.

Then open `/#/dashboard` → Watchtower.

## What's in the project

| File | What it is | Wired to |
|---|---|---|
| `check.fc` | probes a list of sites, routes a verdict, alerts on `down` | interval trigger (60s), public on-demand URL, versions v1/v2/v3 |
| `digest.fc` | renders and mails the daily availability report | daily trigger (09:00 UTC) |
| `escalate.fc` | human-approval incident path with parallel fan-out | manual runs |

The version history tells a story: v1 is the baseline monitor, v2 adds a
`degraded` match arm and per-site telemetry in the loop, and v3 is
deliberately broken (duplicate step name) — saved so the Diagnostics panel has
something to show, then restored to v2.

## The walkthrough, step by step

1. **Project view.** Open the project. Three files, each independently
   runnable — a project is a folder of standalone workflows, not a linked
   multi-file program (`fcc` compiles one file at a time).

2. **Run `check.fc`, read the Trace tab.** The trace shows the VM start, each
   builtin plugin invocation in order (three `http.get` calls — the loop ran
   three iterations), the `store set` dump lines, and the completion line.

3. **Bytecode tab.** Same run, decoded: `EMIT` carries the site list, `LOOP`
   shows its jump target as a clickable link (click it — the disassembly
   scrolls to the target instruction), `ROUTE` is the compiled `match`. This
   is the view that makes the language's control flow concrete.

4. **Run `escalate.fc`.** Trace shows `form.render`, then the `await form.submit`
   pause, then both `parallel` branches' plugin calls in order.

5. **Diagnostics, live.** In the editor, add a line `# temporary note` and
   press Run: the Diagnostics badge lights up with a warning and an inline
   marker appears at that line — FlowCode has **no comment syntax**, so a
   mistyped line is silently dropped from a program that still looks
   successful. Warnings matter in this language; that's why they get a badge.
   Then duplicate a step name (`step done:` twice) for a hard error, and use
   **Versions → restore v2** to put the file back.

6. **Executions panel.** Rows with three different `source` values:
   `project-run` (your manual runs), `deployment` (step 7), and `trigger`
   (step 8). Every path runs through the same sandboxed engine.

7. **Deployment.** Open the on-demand URL from the seed output (or click it
   from the Deployments panel). It re-runs `check.fc` and returns the raw
   compile/run result, plus an `X-FlowCode-Deploy-Note` header stating the
   Phase A limitation. Refresh the Executions panel: a new `deployment` row.

8. **Trigger.** The `check.fc` trigger fires every 60s; within a minute or two
   the Executions panel gains `trigger` rows with no interaction from you.
   The `digest.fc` trigger shows `daily 09:00 UTC` with its computed
   `next_run_at` — the schedule math is visible without the trigger firing.
   Pause it with the panel's toggle when you're done presenting.

9. **KV panel.** `watchtower.*` keys with the value each run's `store set`
   left behind. This is the monitor's "last known state" log.

10. **Optional: the anonymous playground and permalinks.** Copy `check.fc`
    into `/`, press Run, and use Share — the program travels in the URL
    fragment, which the browser never sends to the server.

## Say this out loud (the honest part)

Presenting Watchtower without these caveats oversells what the platform can
do today. All of these are stated in the README and are visible in the
product itself:

- **The plugins are no-op pass-throughs.** No HTTP call, email, or upload
  actually happens; `http.get` logs and forwards its token. Watchtower
  demonstrates orchestration shape, not real effects.
- **`match` arms are documented intent.** They compile sequentially and the
  token does not select an arm at runtime — the `ROUTE` jump is
  unconditional. Same for `{{site}}` templates: they land in the bytecode as
  literal text.
- **Deployments ignore the request.** Method, query, and body are discarded
  (the response and header say so).
- **The KV log is write-side only.** `memory.fetch` in `digest.fc` cannot
  read what a previous run stored; each key shows the final value of the
  latest run that wrote it.
- **No comments.** Any `#` or `//` line in a `.fc` file is a warning and is
  dropped from the program — which is exactly what step 5 demonstrates.
