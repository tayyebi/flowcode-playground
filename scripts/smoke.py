#!/usr/bin/env python3
"""End-to-end check against a running playground.

Compiles and runs every bundled sample through the HTTP API and asserts each one
comes back clean, then exercises the error and limit paths. This is the same
check CI runs against the built image, so it is also the quickest way to satisfy
yourself that a local `docker compose up` is actually working:

    python3 scripts/smoke.py http://localhost:8080

Exits non-zero on the first category of failure, with the offending response.
"""

from __future__ import annotations

import json
import sys
import time
import urllib.error
import urllib.request

HELLO = 'workflow: Smoke\n\nstep greeting:\n    emit\n        value = "hi"\nend\n'


def post_run(base: str, source: str, *, respect_limit: bool = True) -> tuple[int, dict]:
    """POST one program.

    This script makes more requests in a few seconds than the default rate limit
    allows, so by default it backs off and retries on a 429 the way any
    well-behaved client would. Pass respect_limit=False to see the raw 429,
    which is how the rate-limit check below asserts the limiter works at all.
    """
    request = urllib.request.Request(
        f"{base}/api/run",
        data=json.dumps({"source": source}).encode(),
        headers={"Content-Type": "application/json"},
    )

    for attempt in range(6):
        try:
            with urllib.request.urlopen(request) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as err:
            # A refused request is a valid outcome to assert on, not an error.
            body = err.read()
            try:
                payload = json.loads(body)
            except json.JSONDecodeError:
                payload = {"raw": body.decode(errors="replace")}

            if err.code == 429 and respect_limit and attempt < 5:
                delay = int(err.headers.get("Retry-After") or 1)
                time.sleep(min(delay, 10))
                continue
            return err.code, payload

    return 429, {"error": "rate limited after repeated retries"}


def get(base: str, path: str):
    with urllib.request.urlopen(f"{base}{path}") as response:
        return json.load(response)


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

    print("samples")
    samples = get(base, "/api/samples")
    check(len(samples) > 0, f"{len(samples)} samples served")

    for sample in samples:
        status, result = post_run(base, sample["source"])
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
        check(ok, f"sample {sample['id']} compiles, runs, and traces")
        if not ok:
            print(f"        {json.dumps(result)[:400]}")

    print("diagnostics")
    # FlowCode has no comment syntax: this is a warning, and the program still
    # compiles and runs. Both halves of that matter.
    status, result = post_run(base, HELLO + "\n# not a comment\n")
    diagnostics = result.get("compile", {}).get("diagnostics", [])
    check(
        status == 200 and len(diagnostics) == 1 and diagnostics[0]["level"] == "warning",
        f"an unrecognised line warns with a line number ({diagnostics})",
    )
    check(result.get("run") is not None, "a warning does not prevent execution")

    print("compile errors")
    duplicate = 'workflow: Dup\n\nstep a:\n    emit\n        value = "x"\nend\n\nstep a:\n    emit\n        value = "y"\nend\n'
    status, result = post_run(base, duplicate)
    check(status == 200, "a failed compile is still HTTP 200")
    check(result["compile"]["exitCode"] != 0, "a duplicate step name fails compilation")
    check(result.get("run") is None, "a failed compile skips execution")

    print("limits")
    status, _ = post_run(base, "x" * 70_000)
    check(status == 413, f"oversized source is refused with 413 (got {status})")

    status, _ = post_run(base, "")
    check(status == 400, f"empty source is refused with 400 (got {status})")

    print("rate limiting")
    # Deliberately burst without backing off. Left until last, because it
    # exhausts the bucket and every check after it would have to wait.
    statuses = [post_run(base, HELLO, respect_limit=False)[0] for _ in range(40)]
    check(429 in statuses, "a sustained burst is eventually rate limited")

    print()
    if failures:
        print(f"{len(failures)} check(s) failed")
        return 1
    print("all checks passed")
    return 0


if __name__ == "__main__":
    sys.exit(main((sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8080").rstrip("/")))
