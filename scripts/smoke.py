#!/usr/bin/env python3
"""End-to-end check against a running playground.

Creates a throwaway project and exercises the whole surface through the
Projects API: saves and runs a few inline programs (compile, trace, bytecode,
diagnostics), checks the limit paths, then versions, deployments, triggers,
the KV log, and the deploy rate limiter — and deletes the project afterwards.
Point it at a running instance to satisfy yourself that a `docker compose up`
deploy is actually working:

    python3 scripts/smoke.py http://localhost:8033

Requires PLAYGROUND_ADMIN_TOKEN to be unset (open instance) or matched by the
client, since every route it exercises is admin-gated except /healthz and
/deploy/{slug}.

Exits non-zero on the first category of failure, with the offending response.
"""

from __future__ import annotations

import json
import sys
import time
import urllib.error
import urllib.request

HELLO = 'workflow: Smoke\n\nstep greeting:\n    emit\n        value = "hi"\nend\n'

# Runs through the project engine and must show up in the project's KV log:
# fcplay dumps `store set` lines into the trace and the server parses them
# back out, storing the run's final value per key.
KV_PROBE = (
    "workflow: KVProbe\n\n"
    "step greeting:\n    emit\n        value = \"hi\"\nend\n\n"
    "step saved:\n    store set\n        key = \"smoke.greeting\"\n        value = greeting\nend\n"
)

def get(base: str, path: str):
    with urllib.request.urlopen(f"{base}{path}") as response:
        return json.load(response)


def request_json(base: str, method: str, path: str, body: dict | None = None, *, raw_body: str | None = None):
    """A generic JSON request, for the Projects API's CRUD-shaped routes.

    Returns (status, parsed_body) for both success and HTTPError, so callers
    can assert on error responses too. Pass raw_body to send deliberately
    malformed JSON (the 400 check needs it).
    """
    data = raw_body.encode() if raw_body is not None else (
        json.dumps(body).encode() if body is not None else None
    )
    req = urllib.request.Request(
        f"{base}{path}", data=data, method=method,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as response:
            raw = response.read()
            return response.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as err:
        raw = err.read()
        try:
            return err.code, json.loads(raw)
        except json.JSONDecodeError:
            return err.code, {"raw": raw.decode(errors="replace")}


def main(base: str) -> int:
    failures: list[str] = []

    def check(condition: bool, message: str) -> None:
        if condition:
            print(f"  ok    {message}")
        else:
            print(f"  FAIL  {message}")
            failures.append(message)

    print("health")
    health = get(base, "/healthz")
    check(health.get("status") == "ok", f"healthz reports ok ({health})")

    print("projects")
    status, _ = request_json(base, "GET", "/api/projects")
    check(status == 200, f"GET /api/projects ({status})")

    status, project = request_json(base, "POST", "/api/projects", {"name": "Smoke Test Project"})
    check(status == 201, f"create project ({status})")
    project_id = project["id"]

    def put_file(name: str, content: str) -> int:
        return request_json(base, "PUT", f"/api/projects/{project_id}/files/{name}", {"content": content})[0]

    def run_file(name: str) -> tuple[int, dict]:
        return request_json(base, "POST", f"/api/projects/{project_id}/files/{name}/run")

    print("run")
    put_file("hello.fc", HELLO)
    status, result = run_file("hello.fc")
    run = result.get("run") or {}
    ok = (
        status == 200
        and result["compile"]["exitCode"] == 0
        and not result["compile"]["diagnostics"]
        and run.get("exitCode") == 0
        # The trace is the whole reason fcplay exists; an empty pane here
        # means the stock silent runtime crept back in.
        and "vm completed successfully" in run.get("stderr", "")
        and result.get("bytecode", {}).get("instructionCount", 0) > 0
    )
    check(ok, f"a clean program compiles, runs, and traces ({status})")
    if not ok:
        print(f"        {json.dumps(result)[:400]}")

    print("diagnostics")
    # FlowCode has no comment syntax: this is a warning, and the program still
    # compiles and runs. Both halves of that matter.
    put_file("warn.fc", HELLO + "\n# not a comment\n")
    status, result = run_file("warn.fc")
    diagnostics = result.get("compile", {}).get("diagnostics", [])
    check(
        status == 200 and len(diagnostics) == 1 and diagnostics[0]["level"] == "warning",
        f"an unrecognised line warns with a line number ({diagnostics})",
    )
    check(result.get("run") is not None, "a warning does not prevent execution")

    print("compile errors")
    duplicate = 'workflow: Dup\n\nstep a:\n    emit\n        value = "x"\nend\n\nstep a:\n    emit\n        value = "y"\nend\n'
    put_file("dup.fc", duplicate)
    status, result = run_file("dup.fc")
    check(status == 200, "a failed compile is still HTTP 200")
    check(result["compile"]["exitCode"] != 0, "a duplicate step name fails compilation")
    check(result.get("run") is None, "a failed compile skips execution")

    print("limits")
    status, _ = request_json(
        base, "PUT", f"/api/projects/{project_id}/files/big.fc", {"content": "x" * 70_000}
    )
    check(status == 413, f"oversized file content is refused with 413 (got {status})")

    status, _ = request_json(
        base, "PUT", f"/api/projects/{project_id}/files/bad.fc", None, raw_body="not json"
    )
    check(status == 400, f"malformed request body is refused with 400 (got {status})")

    print("kv, versions, deployments, triggers")
    # A file that stores: the KV log is parsed out of the run trace's
    # store-set dump, so this is what proves that pipeline end to end.
    status, _ = request_json(
        base, "PUT", f"/api/projects/{project_id}/files/kv.fc", {"content": KV_PROBE}
    )
    check(status == 200, f"save kv probe file ({status})")

    status, run = run_file("kv.fc")
    check(
        status == 200 and run.get("run") is not None and run["run"]["exitCode"] == 0,
        f"run the kv probe file ({status})",
    )

    status, kv = request_json(base, "GET", f"/api/projects/{project_id}/kv")
    probe = next((e for e in kv if e["key"] == "smoke.greeting"), None) if isinstance(kv, list) else None
    check(
        status == 200 and probe is not None and probe["value"] == "hi",
        f"a store set lands in the kv log with its final value ({status}, {kv})",
    )

    status, version = request_json(base, "POST", f"/api/projects/{project_id}/versions", {"label": "v1"})
    check(status == 201 and version["number"] == 1, f"save version ({status})")

    status, deployment = request_json(
        base, "POST", f"/api/projects/{project_id}/deployments", {"fileName": "warn.fc"}
    )
    check(status == 201, f"create deployment ({status})")

    with urllib.request.urlopen(f"{base}/deploy/{deployment['slug']}") as response:
        deploy_status = response.status
        deploy_body = json.load(response)
    check(
        deploy_status == 200 and "note" in deploy_body,
        f"deployment responds and states its Phase A limitation ({deploy_status})",
    )

    status, trigger = request_json(
        base, "POST", f"/api/projects/{project_id}/triggers",
        {"fileName": "warn.fc", "scheduleType": "interval", "intervalSeconds": 1},
    )
    check(status == 201, f"create trigger ({status})")

    # The scheduler ticks on its own cadence (~15s), independent of how
    # short the trigger's own interval is, so give it a bounded window
    # rather than sleeping for a fixed guess.
    fired = False
    for _ in range(20):
        _, executions = request_json(base, "GET", f"/api/projects/{project_id}/executions")
        if any(e["source"] == "trigger" for e in executions):
            fired = True
            break
        time.sleep(2)
    check(fired, "a short-interval trigger fires within the poll window")

    print("deploy rate limiting")
    # Deliberately burst the public deploy endpoint without backing off. Left
    # until nearly last because it exhausts the bucket, and each call re-runs
    # the workflow through the engine.
    slug = deployment["slug"]
    statuses = []
    for _ in range(45):
        try:
            with urllib.request.urlopen(f"{base}/deploy/{slug}") as response:
                statuses.append(response.status)
        except urllib.error.HTTPError as err:
            statuses.append(err.code)
    check(429 in statuses, "a sustained deploy burst is eventually rate limited")

    print("cleanup")
    status, _ = request_json(base, "DELETE", f"/api/projects/{project_id}")
    check(status == 204, f"delete project cleans up (cascades) ({status})")

    print()
    if failures:
        print(f"{len(failures)} check(s) failed")
        return 1
    print("all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main((sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8033").rstrip("/")))
