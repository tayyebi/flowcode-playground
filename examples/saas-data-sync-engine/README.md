# SaaS Data Sync Engine

A bi-directional data synchronization engine that keeps multiple SaaS systems in sync on a scheduled basis, with diff computation, conflict resolution strategies, cross-system field mapping, and comprehensive audit logging.

## Modules

| Module    | Purpose                                              |
|-----------|------------------------------------------------------|
| `http`    | Fetch from and push to SaaS APIs, health monitoring  |
| `email`   | Conflict notifications to system administrators      |
| `storage` | Sync cycle log archival                              |
| `crm`     | Available for CRM-specific sync targets              |

## Flow Overview

1. **Trigger** — Cron-based schedule runs every 15 minutes.
2. **Load Config** — Retrieve sync configuration from state store (system list, schemas, strategies).
3. **Per-System Sync Loop** — For each configured SaaS system:
   - Fetch remote data with `If-Modified-Since` header for incremental sync.
   - Handle HTTP status: 200 (new data), 304 (no changes), else (error logging).
   - Compute diff between local snapshot and remote data.
   - Detect conflicts using the system's configured strategy.
   - **Conflict Resolution Branch**:
     - `manual` — Notify admin, save conflicts, skip this system.
     - `remote_wins` — Accept all remote changes.
     - `local_wins` — Keep all local values.
     - `merge` — Field-level merge of both sides.
   - Apply creates, updates, and deletes back to the remote API.
   - Update local data snapshot and log sync results.
4. **Cross-System Propagation** — For each configured cross-system mapping:
   - Load source system data from state store.
   - Apply field mapping and filters.
   - Push transformed records to target system's bulk API.
   - Log propagation results.
5. **Parallel Finalization** — Archive sync summary and send monitoring heartbeat.
6. **Emit** — Emit cycle completion event with system and propagation counts.

## Capabilities Demonstrated

- Cron-based scheduled trigger
- Incremental sync with `If-Modified-Since` optimization
- Multi-system loop with per-system error handling and `continue` semantics
- Diff computation (created, updated, deleted records)
- Four conflict resolution strategies (manual, remote_wins, local_wins, merge)
- CRUD operations against remote APIs (POST, PUT, DELETE)
- Cross-system field mapping and data propagation
- Bulk API operations
- State-driven configuration (sync config stored in state store)
- Parallel archival and health monitoring
- Comprehensive per-system and per-cycle audit logging

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc sync.fc sync.fcb
flowcode run sync.fcb
```

The integrations named above (`http.*`, `email.*`, ...) resolve to the runtime's
built-in stubs, which log the call and pass the token through instead of
performing real I/O -- so the sample runs to completion without credentials or
network access. Load a plugin library exporting the same names to make the calls
real, or pass `--strict` to `flowcode run` to disable the stubs and have
unresolved calls fail.
