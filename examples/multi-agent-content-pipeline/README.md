# Multi-Agent Content Pipeline

A multi-agent AI workflow that orchestrates content creation through five specialized agents: **Planner → Researcher → Writer → Reviewer → Publisher**, with memory, tools, and human-in-the-loop approval.

## Modules

| Module    | Purpose                                    |
|-----------|--------------------------------------------|
| `ai`      | LLM-powered agents (planner, researcher, writer, reviewer, publisher) |
| `http`    | Publishing content to external endpoints   |
| `storage` | Archiving the full content lifecycle       |
| `form`    | Human editor review UI                     |
| `email`   | Notifications and revision requests        |
| `crm`     | Customer relationship note tracking        |
| `memory`  | Cross-run context and feedback persistence |

## Flow Overview

1. **Ingest** — Receive and normalize a content request via webhook.
2. **Plan** — Planner agent decomposes the request into sections, goals, and research needs.
3. **Research** — Researcher agent loops through each planned section and produces factual notes.
4. **Write** — Writer agent synthesizes plan, research, and historical memory into a draft.
5. **Review** — Reviewer agent critiques the draft and proposes edits.
6. **Human Approval** — An editor reviews draft + AI critique in a form; approves or requests changes.
7. **Branch** — If approved, merge edits. If changes requested, store feedback in memory and notify requester.
8. **Publish** — Publisher agent adapts final content to the target channel (blog, LinkedIn, email).
9. **Distribute** — Parallel: publish to endpoint, add CRM note, archive full pipeline artifacts.
10. **Notify** — Email requester that content is live; emit completion event.

## Capabilities Demonstrated

- Multi-agent orchestration with specialized AI models
- Persistent memory across workflow runs
- Human-in-the-loop approval with form rendering
- Conditional branching (approved vs. changes requested)
- Loop-based parallel research
- Parallel execution of publish, CRM sync, and archival
- State persistence at every stage
- Event emission for downstream consumers

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc pipeline.fc pipeline.fcb
flowcode run pipeline.fcb
```

The integrations named above (`http.*`, `email.*`, ...) resolve to the runtime's
built-in stubs, which log the call and pass the token through instead of
performing real I/O -- so the sample runs to completion without credentials or
network access. Load a plugin library exporting the same names to make the calls
real, or pass `--strict` to `flowcode run` to disable the stubs and have
unresolved calls fail.
