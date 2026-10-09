#!/usr/bin/env python3
"""Seed the Watchtower demo project on a running playground.

Creates one project with three standalone workflows — a site health check,
a daily digest, and an incident escalation path — and wires up every Phase A
feature around them: version snapshots (including a deliberately broken one
that gets restored), a public on-demand-check deployment, and interval/daily
triggers. Each file is run once so the KV log and execution history are
populated immediately, then the summary prints the URLs to open.

    python3 scripts/seed-watchtower.py http://localhost:8033 [--reset] [--token SECRET]

--reset deletes an existing project with the same slug first. --token is only
needed when the instance sets PLAYGROUND_ADMIN_TOKEN.

The workflows follow the bundled samples' convention: FlowCode's builtin
plugins are no-op pass-throughs, `match` arms and `{{templates}}` are
documented intent rather than runtime behaviour, and there is no comment
syntax — see docs/demo.md before presenting any of this as more than it is.
"""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request

SLUG = "watchtower"

CHECK_V1 = """\
use http
use email

workflow: WatchtowerCheck

trigger:
    webhook "/watchtower/check"
end

step sites:
    emit
        value = "example.com api.example.com cdn.example.com"
end

step probe:
    loop site in sites
        http.get
            url = "https://{{site}}/health"
    end
end

step verdict:
    transform watchtower.verdict
        input = probe
end

match verdict.status
    down ->
        step alert:
            on_error retry
            email.send
                to = "oncall@example.com"
                subject = "Watchtower: site down"
                body = "health check failed, see the run trace"
        end
        stop
    ok ->
        step quiet:
            store set
                key = "watchtower.status"
        end
end

step recorded:
    store set
        key = "watchtower.last_check"
end

step done:
    emit
        value = "check_complete"
        metadata = verdict
end
"""

# v2 adds a degraded arm and per-site telemetry inside the loop.
CHECK_V2 = """\
use http
use email
use crm
use memory

workflow: WatchtowerCheck

trigger:
    webhook "/watchtower/check"
end

step sites:
    emit
        value = "example.com api.example.com cdn.example.com"
end

step probe:
    loop site in sites
        http.get
            url = "https://{{site}}/health"
        memory.store
            key = "probe.{{site}}"
            value = "latency observed"
    end
end

step verdict:
    transform watchtower.verdict
        input = probe
end

match verdict.status
    down ->
        step alert:
            on_error retry
            email.send
                to = "oncall@example.com"
                subject = "Watchtower: site down"
                body = "health check failed, see the run trace"
        end
        stop
    degraded ->
        step noted:
            crm.addNote
                customer = "watchtower"
                note = "degraded latency observed on one or more sites"
        end
    ok ->
        step quiet:
            store set
                key = "watchtower.status"
        end
end

step recorded:
    store set
        key = "watchtower.last_check"
end

step done:
    emit
        value = "check_complete"
        metadata = verdict
end
"""

# A duplicate `step done` at the end: compiles to an error, which is the
# point — this version exists to be broken and then restored.
CHECK_V3_BROKEN = CHECK_V2 + """
step done:
    emit
        value = "duplicate step name"
end
"""

DIGEST = """\
use ai
use form
use email
use storage

workflow: WatchtowerDailyDigest

step collect:
    memory.fetch
        key = "watchtower.status"
end

step summary:
    ai.generate
        prompt = "summarize today's availability"
        input = collect
end

step rendered:
    form.render
        form_id = "watchtower_daily_digest"
        data = summary
end

step mailed:
    email.send
        to = "ops@example.com"
        subject = "Watchtower daily digest"
        body = "availability summary for the last 24h"
end

step archived:
    storage.upload
        bucket = "watchtower-digests"
        file = rendered
end

step saved:
    store set
        key = "watchtower.digest.last"
end

step done:
    emit
        value = "digest_sent"
        metadata = mailed
end
"""

ESCALATE = """\
use form
use email
use crm
use storage

workflow: WatchtowerEscalate

step incident:
    emit
        value = "incident opened by watchtower"
end

step reviewForm:
    form.render
        form_id = "incident_review"
        data = incident
end

step reviewed:
    await form.submit
        form_id = "incident_review"
end

match reviewed.status
    approved ->
        step remediation:
            transform merge
                incident = incident
                review = reviewed
        end

        parallel:

            step pageOncall:
                email.send
                    to = "oncall@example.com"
                    subject = "Remediation approved"
                    body = "begin remediation"
            end

            step archive:
                storage.upload
                    bucket = "watchtower-incidents"
                    file = remediation
            end

        end

        step combined:
            transform combine
                paging = pageOncall
                archive = archive
        end
    rejected ->
        step logged:
            crm.addNote
                customer = "watchtower"
                note = "incident closed without remediation"
        end
        stop
end

step saved:
    store set
        key = "watchtower.incident.last"
end

step closed:
    emit
        value = "incident_closed"
        metadata = combined
end
"""

DESCRIPTION = (
    "Uptime monitoring suite. check.fc probes the site list on a schedule "
    "(also deployed as an on-demand URL), digest.fc mails the daily report, "
    "escalate.fc is the human-approval incident path."
)


def request_json(
    base: str, method: str, path: str, body: dict | None = None, *, token: str | None = None
) -> tuple[int, dict | list]:
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"} if data else {}
    req = urllib.request.Request(f"{base}{path}", data=data, headers=headers, method=method)
    if token:
        req.add_header("Cookie", f"playground_admin={token}")
    try:
        with urllib.request.urlopen(req) as response:
            payload = response.read()
            return response.status, json.loads(payload) if payload else {}
    except urllib.error.HTTPError as err:
        raw = err.read()
        try:
            return err.code, json.loads(raw)
        except json.JSONDecodeError:
            return err.code, {"raw": raw.decode(errors="replace")}


def die(context: str, status: int, body) -> None:
    print(f"error: {context}: HTTP {status}: {json.dumps(body)[:400]}", file=sys.stderr)
    sys.exit(1)


def expect(context: str, status: int, body, want: int) -> None:
    if status != want:
        die(context, status, body)


def run_file(base: str, project_id: int, name: str, token: str | None) -> None:
    status, run = request_json(base, "POST", f"/api/projects/{project_id}/files/{name}/run", {}, token=token)
    expect(f"run {name}", status, run, 200)
    if run["compile"]["exitCode"] != 0 or run.get("run", {}).get("exitCode") != 0:
        die(f"run {name}", 200, run)
    print(f"  ran {name}: completed, trace recorded")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("base", nargs="?", default="http://localhost:8033", help="playground base URL")
    parser.add_argument("--reset", action="store_true", help="delete an existing project with this slug first")
    parser.add_argument("--token", help="PLAYGROUND_ADMIN_TOKEN when the instance sets one")
    args = parser.parse_args()
    base = args.base.rstrip("/")
    auth = {"token": args.token} if args.token else {}

    if args.reset:
        status, projects = request_json(base, "GET", "/api/projects", **auth)
        expect("list projects", status, projects, 200)
        for p in projects:
            if p["slug"] == SLUG:
                status, body = request_json(base, "DELETE", f"/api/projects/{p['id']}", **auth)
                expect("delete existing project", status, body, 204)
                print(f"reset: deleted existing {SLUG} project (id {p['id']})")

    print("creating project")
    status, project = request_json(
        base, "POST", "/api/projects", {"name": "Watchtower", "description": DESCRIPTION}, **auth
    )
    expect("create project", status, project, 201)
    pid = project["id"]
    print(f"  project {project['slug']} (id {pid})")

    print("saving files (check.fc at v1)")
    for name, content in [("check.fc", CHECK_V1), ("digest.fc", DIGEST), ("escalate.fc", ESCALATE)]:
        status, body = request_json(base, "PUT", f"/api/projects/{pid}/files/{name}", {"content": content}, **auth)
        expect(f"save {name}", status, body, 200)

    print("running each file once (populates kv + executions)")
    for name in ("check.fc", "digest.fc", "escalate.fc"):
        run_file(base, pid, name, args.token)

    print("versioning: v1 baseline")
    status, v1 = request_json(base, "POST", f"/api/projects/{pid}/versions", {"label": "v1 baseline monitor"}, **auth)
    expect("save v1", status, v1, 201)

    print("versioning: v2 with degraded arm + loop telemetry")
    status, body = request_json(base, "PUT", f"/api/projects/{pid}/files/check.fc", {"content": CHECK_V2}, **auth)
    expect("update check.fc to v2", status, body, 200)
    status, v2 = request_json(base, "POST", f"/api/projects/{pid}/versions", {"label": "v2 degraded arm + telemetry"}, **auth)
    expect("save v2", status, v2, 201)

    print("versioning: v3 is deliberately broken, then restored to v2")
    status, body = request_json(base, "PUT", f"/api/projects/{pid}/files/check.fc", {"content": CHECK_V3_BROKEN}, **auth)
    expect("update check.fc to broken v3", status, body, 200)
    status, v3 = request_json(base, "POST", f"/api/projects/{pid}/versions", {"label": "v3 (broken: duplicate step)"}, **auth)
    expect("save v3", status, v3, 201)
    status, body = request_json(base, "POST", f"/api/projects/{pid}/versions/{v2['number']}/restore", {}, **auth)
    expect("restore v2", status, body, 200)

    print("deploying check.fc as an on-demand URL")
    status, deployment = request_json(base, "POST", f"/api/projects/{pid}/deployments", {"fileName": "check.fc"}, **auth)
    expect("create deployment", status, deployment, 201)

    print("scheduling triggers")
    status, every_minute = request_json(
        base, "POST", f"/api/projects/{pid}/triggers",
        {"fileName": "check.fc", "scheduleType": "interval", "intervalSeconds": 60}, **auth
    )
    expect("create interval trigger", status, every_minute, 201)
    status, daily = request_json(
        base, "POST", f"/api/projects/{pid}/triggers",
        {"fileName": "digest.fc", "scheduleType": "daily", "dailyTimeUtc": "09:00"}, **auth
    )
    expect("create daily trigger", status, daily, 201)

    print()
    print("seeded. open:")
    print(f"  dashboard     {base}/#/dashboard")
    print(f"  project       {base}/#/projects/{pid}")
    print(f"  on-demand run {base}/deploy/{deployment['slug']}  (check.fc, ignores request data)")
    print(f"  triggers      check.fc every {every_minute['intervalSeconds']}s"
          f" (next {every_minute['nextRunAt']}), digest.fc daily at {daily['dailyTimeUtc']} UTC"
          f" (next {daily['nextRunAt']})")
    print()
    print("execution history will gain a trigger row within ~a minute; see docs/demo.md for the walkthrough.")


if __name__ == "__main__":
    main()
