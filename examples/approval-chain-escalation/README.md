# Approval Chain with Escalations and SLAs

A complex multi-tier approval workflow with amount-based routing, SLA enforcement, automatic escalation on timeout, and full audit trail.

## Modules

| Module    | Purpose                                      |
|-----------|----------------------------------------------|
| `http`    | Webhook trigger for approval submissions     |
| `email`   | Approver notifications and requester updates |
| `form`    | Approval decision UI at each tier            |
| `storage` | Audit log archival                           |
| `crm`     | Department-level tracking notes              |

## Flow Overview

1. **Submit** — Receive approval request via webhook with amount, department, and SLA.
2. **SLA Setup** — Compute deadline from submission time and SLA hours; store with TTL.
3. **Tier Resolution** — Determine approver tier based on request amount:
   - ≤ $1,000 → Team Lead
   - ≤ $10,000 → Department Head
   - ≤ $100,000 → VP Finance
   - \> $100,000 → CFO
4. **Notify & Render** — Email the approver and render the decision form.
5. **Await Decision** — Wait for form submission with SLA timeout.
6. **Branch on Decision**:
   - **Approved** — Record approval; if multi-tier is required, escalate to next tier with the same SLA-aware flow.
   - **Rejected** — Record rejection, notify requester, stop.
   - **Timeout (SLA Breach)** — Mark SLA as breached, escalate to manager, await escalation decision.
7. **Parallel Finalization** — Upload audit log, update CRM, notify requester.
8. **Emit** — Emit completion event with request ID, amount, and final status.

## Capabilities Demonstrated

- Amount-based dynamic approver routing
- Multi-tier approval chain with conditional escalation
- SLA enforcement with timeout-based automatic escalation
- Nested branching (decision → tier check → escalation decision)
- Form-based human-in-the-loop at multiple stages
- TTL-based state expiration for SLA tracking
- Parallel finalization (audit + CRM + notification)
- Full audit trail persistence
- Graceful stop on rejection at any level

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc approval.fc approval.fcb
flowcode run approval.fcb
```

The integrations named above (`http.*`, `email.*`, ...) resolve to the runtime's
built-in stubs, which log the call and pass the token through instead of
performing real I/O -- so the sample runs to completion without credentials or
network access. Load a plugin library exporting the same names to make the calls
real, or pass `--strict` to `flowcode run` to disable the stubs and have
unresolved calls fail.
