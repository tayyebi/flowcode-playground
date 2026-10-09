# Customer Onboarding

An end-to-end customer onboarding workflow demonstrating every major Flowcode capability: automation, integrations, human-in-the-loop, forms, branching, loops, parallelism, state, and modular composition.

## Modules

| Module    | Purpose                                      |
|-----------|----------------------------------------------|
| `http`    | Fetch uploaded customer documents            |
| `email`   | Rejection notices and welcome emails         |
| `form`    | Human review and correction UI               |
| `ai`      | Vision-based data extraction from documents  |
| `crm`     | Customer record creation                     |
| `storage` | Document archival to cloud buckets           |

## Flow Overview

1. **Receive** — Fetch the raw customer document via HTTP.
2. **Extract** — AI vision model extracts structured customer profile data.
3. **Validate** — Transform step validates the extracted profile.
4. **Human Review** — Render a review form; await human approval or rejection.
5. **Branch** — If approved, merge corrections. If rejected, notify ops and stop.
6. **Upload** — Loop through all customer documents and upload to storage.
7. **Parallel** — Create CRM record and send welcome email simultaneously.
8. **Combine** — Merge parallel results into a single output.
9. **Persist** — Store the onboarding result in the state store.
10. **Emit** — Emit completion event with metadata for downstream consumers.

## Capabilities Demonstrated

- Multi-step automation pipeline
- AI-powered data extraction (vision + structured schema)
- Human-in-the-loop (form render + submit)
- Conditional branching (approved vs. rejected)
- Loop-based document upload
- Parallel execution (CRM + email)
- State persistence
- Event emission
- Transformations and data merging
- Module-based integrations

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc onboarding.fc onboarding.fcb
flowcode run onboarding.fcb
```

The integrations named above (`http.*`, `email.*`, ...) resolve to the runtime's
built-in stubs, which log the call and pass the token through instead of
performing real I/O -- so the sample runs to completion without credentials or
network access. Load a plugin library exporting the same names to make the calls
real, or pass `--strict` to `flowcode run` to disable the stubs and have
unresolved calls fail.
