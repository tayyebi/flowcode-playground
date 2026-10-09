# Examples

Every FlowCode example lives here — this directory is the single home for
them, and the playground serves it two ways:

- **Samples** (every directory without a `project.json`): read-only programs
  offered by the anonymous playground's sample picker via `GET /api/samples`.
- **Project examples** (directories with a `project.json` manifest): seeded as
  full saved projects — files, an initial run, version snapshots, a public
  deployment, and scheduled triggers — automatically, exactly once per
  database (see the README's "Seeding" section).

## Samples

| Example | Shows |
|---|---|
| [`hello-world/`](hello-world/) | The smallest useful workflow: emit + `store set`. Start here. |
| [`url-to-markdown/`](url-to-markdown/) | Form input → `http.get` fetch → `ai.generate` → storage; the smallest realistic flow. |
| [`customer-onboarding/`](customer-onboarding/) | A tour of every module: http, ai, form, email, crm, storage, branching, loops, parallelism. |
| [`approval-chain-escalation/`](approval-chain-escalation/) | Multi-tier approvals, SLA enforcement via TTL store, escalation on timeout. |
| [`ecommerce-order-pipeline/`](ecommerce-order-pipeline/) | Inventory loops, three-way availability branching, payment rollback, parallel finalize. |
| [`iot-streaming-automation/`](iot-streaming-automation/) | `mqtt.subscribe` trigger, time-series store, AI anomaly detection, batch archival. |
| [`multi-agent-content-pipeline/`](multi-agent-content-pipeline/) | Five AI agents in sequence, memory module, human review gate. |
| [`saas-data-sync-engine/`](saas-data-sync-engine/) | `cron` trigger, If-Modified-Since sync, conflict-resolution branching. |

## Project examples

| Example | Shows |
|---|---|
| [`watchtower/`](watchtower/) | An uptime-monitoring project wired to every playground feature: versions (including a deliberately broken one that gets restored), a public on-demand deployment, interval and daily triggers, and a populated execution history + KV log. |

## Conventions

The examples follow FlowCode's builtin-plugin conventions: plugin calls
(`http.*`, `email.*`, `ai.*`, …) are no-op pass-throughs, `match` arms and
`{{templates}}` are documented intent rather than runtime behaviour, and the
language has no comment syntax. Read them as illustrations of intended
style; each example's README says what it does and does not demonstrate.

Every example must keep compiling and running with the current compiler:
`go test ./...` in `server/` exercises all of them through the same engine
the playground uses (when `FLOWCODE_FCC`/`FLOWCODE_RUNNER` are set).

To add an example: create a directory with one `.fc` file (plus a `README.md`
following the shape above) and it becomes a sample automatically. Give it a
`project.json` manifest instead if it should seed as a saved project —
`watchtower/project.json` is the reference.
