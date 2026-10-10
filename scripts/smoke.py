#!/usr/bin/env python3
"""End-to-end check against a running playground.

Authentication is OIDC for humans; this script uses the instance's service
token (the internal `__service_token` setting) with `Authorization: Bearer`.
Point it at a running instance and its database:

    python3 scripts/smoke.py http://localhost:8033 data/playground.db

Checks: health, login redirect, auth rejection without a token, then the
project surface through the JSON API — create project, save + run a workflow
(compile, trace, bytecode, app-call transcript), KV extraction, versions,
deployments, the public deploy URL, triggers — and deletes the project
afterwards. Exits non-zero on the first failure, printing the offending
response.
"""

from __future__ import annotations

import json
import sqlite3
import sys
import urllib.error
import urllib.request

HELLO = 'workflow: Smoke\n\nstep greeting:\n    emit\n        value = "hi"\nend\n'

KV_PROBE = (
    "workflow: KVProbe\n\n"
    "step greeting:\n    emit\n        value = \"hi-there\"\nend\n\n"
    "step saved:\n    store set\n        key = \"smoke.greeting\"\nend\n"
)


class Fail(Exception):
    pass


def req(base: str, method: str, path: str, token: str | None = None,
        body: dict | None = None, *, raw: bool = False):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    r = urllib.request.Request(f"{base}{path}", data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(r) as resp:
            payload = resp.read()
            if raw:
                return resp.status, payload.decode(errors="replace")
            return resp.status, (json.loads(payload) if payload else None)
    except urllib.error.HTTPError as e:
        payload = e.read()
        if raw:
            return e.code, payload.decode(errors="replace")
        try:
            return e.code, (json.loads(payload) if payload else None)
        except json.JSONDecodeError:
            return e.code, {"raw": payload.decode(errors="replace")}


def expect(cond: bool, what: str, detail=None):
    if not cond:
        raise Fail(f"{what}: {detail if detail is not None else 'unexpected'}")
    print(f"  ok: {what}")


def service_token(db_path: str) -> str:
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        row = con.execute(
            "SELECT value FROM system_settings WHERE key = '__service_token'"
        ).fetchone()
    finally:
        con.close()
    if not row:
        raise Fail("no __service_token in the database — boot the server once first")
    return row[0]


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__)
        return 2
    base, db_path = sys.argv[1].rstrip("/"), sys.argv[2]
    token = service_token(db_path)

    print("health & auth gates")
    status, body = req(base, "GET", "/healthz")
    expect(status == 200 and body.get("status") == "ok", "/healthz ok", body)

    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            return None

    no_follow = urllib.request.build_opener(NoRedirect)
    try:
        with no_follow.open(f"{base}/projects") as resp:
            raise Fail(f"/projects unauthenticated returned {resp.status}, expected redirect")
    except urllib.error.HTTPError as e:
        expect(e.code in (303, 302) and "/login" in e.headers.get("Location", ""),
               "/projects unauthenticated redirects to /login",
               (e.code, e.headers.get("Location")))
    status, _ = req(base, "GET", "/api/projects")
    expect(status == 401, "JSON API rejects requests without a token", status)
    status, _ = req(base, "GET", "/api/projects", token=token)
    expect(status == 200, "service token authenticates the JSON API", status)

    print("project surface")
    status, proj = req(base, "POST", "/api/projects", token=token,
                       body={"name": "smoke-test"})
    expect(status == 201 and proj.get("id"), "project created", proj)
    pid = proj["id"]

    status, _ = req(base, "PUT", f"/api/projects/{pid}/files/main.fc", token=token,
                    body={"content": KV_PROBE})
    expect(status == 200, "file saved")

    status, run = req(base, "POST", f"/api/projects/{pid}/files/main.fc/run", token=token)
    expect(status == 200, "run executed", run)
    expect(run["run"]["exitCode"] == 0, "run exited 0", run["run"])
    expect('store set key = "smoke.greeting"' in run["run"]["stderr"],
           "trace carries the store-set dump")
    expect(run["bytecode"]["instructionCount"] >= 2, "bytecode decoded")

    status, kv = req(base, "GET", f"/api/projects/{pid}/kv", token=token)
    expect(status == 200 and any(e["key"] == "smoke.greeting" for e in kv),
           "KV log extracted from the trace", kv)

    status, ver = req(base, "POST", f"/api/projects/{pid}/versions", token=token,
                      body={"label": "smoke"})
    expect(status == 201 and ver.get("number"), "version saved", ver)

    status, dep = req(base, "POST", f"/api/projects/{pid}/deployments", token=token,
                      body={"fileName": "main.fc"})
    expect(status == 201 and dep.get("slug"), "deployment created", dep)
    status, dtext = req(base, "GET", f"/deploy/{dep['slug']}?format=text", raw=True)
    expect(status == 200 and "vm completed successfully" in dtext,
           "public deploy URL runs the workflow", dtext[:120])

    status, trig = req(base, "POST", f"/api/projects/{pid}/triggers", token=token,
                       body={"fileName": "main.fc", "scheduleType": "interval",
                             "intervalSeconds": 3600})
    expect(status == 201 and trig.get("id"), "trigger created", trig)

    status, _ = req(base, "DELETE", f"/api/projects/{pid}", token=token)
    expect(status == 204, "project deleted")

    print("all smoke checks passed")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Fail as f:
        print(f"FAIL: {f}", file=sys.stderr)
        sys.exit(1)
