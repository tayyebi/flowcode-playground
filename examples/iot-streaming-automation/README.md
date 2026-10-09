# IoT Streaming Automation

A real-time IoT automation workflow that processes streaming sensor events with AI-powered anomaly detection, multi-level alerting, time-series aggregation, and batch archival.

## Modules

| Module    | Purpose                                         |
|-----------|------------------------------------------------ |
| `mqtt`    | Subscribe to sensor topics, publish commands    |
| `http`    | Push alerts to operations API                   |
| `ai`      | Anomaly detection on time-series data           |
| `storage` | Batch archival of sensor readings               |
| `email`   | Critical alert notifications                    |
| `crm`     | Incident tracking notes                         |

## Flow Overview

1. **Subscribe** — Listen to MQTT sensor topic with wildcard for all devices.
2. **Normalize** — Transform raw sensor payload into a standard reading format.
3. **Persist Latest** — Store the most recent reading per device.
4. **Time-Series Update** — Append to rolling history (capped at 1000 points).
5. **Anomaly Detection** — AI model evaluates the reading against historical pattern; classifies as normal, warning, or critical.
6. **Branch on Severity**:
   - **Normal** — Log status, continue.
   - **Warning** — Increment warning counter; after 3 consecutive warnings, escalate via HTTP alert.
   - **Critical** — Parallel: push alert to ops API, send email, issue MQTT emergency shutdown command, create incident note.
7. **Aggregate** — Compute 1-minute rolling window statistics (mean, max, min, stddev) with 24h TTL.
8. **Batch Archive** — Buffer readings; when batch reaches 100, upload to cloud storage and reset buffer.
9. **Emit** — Emit processed event with device ID, severity, and timestamp.

## Capabilities Demonstrated

- MQTT-based streaming event triggers
- Rolling time-series state management
- AI-powered anomaly detection on streaming data
- Multi-level conditional branching (normal / warning / critical)
- Warning counter with threshold-based escalation
- Parallel emergency response (alert + email + device command)
- Sliding window aggregation with TTL
- Batch buffering and conditional archival
- Device command publishing (emergency shutdown)
- Event emission for downstream consumers

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc streaming.fc streaming.fcb
flowcode run streaming.fcb
```

The integrations named above (`http.*`, `email.*`, ...) resolve to the runtime's
built-in stubs, which log the call and pass the token through instead of
performing real I/O -- so the sample runs to completion without credentials or
network access. Load a plugin library exporting the same names to make the calls
real, or pass `--strict` to `flowcode run` to disable the stubs and have
unresolved calls fail.
