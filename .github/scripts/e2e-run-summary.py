#!/usr/bin/env python3
"""Render one run-level table for the e2e nightly into the job summary.

Reading a nightly meant opening nineteen jobs, and the Actions UI does not
distinguish a cell that failed from one that was cancelled out from under it —
a distinction every investigation of this suite has needed.

Inputs (env):
    GITHUB_TOKEN         Token with actions:read.
    GITHUB_REPOSITORY    owner/repo.
    GITHUB_RUN_ID        Run to summarise.
    JUNIT_ROOT           Directory holding downloaded junit-cell-* artifacts.
    GITHUB_STEP_SUMMARY  Appended to.

Test counts come from the junit artifacts; result, duration and runner come
from the jobs API, so a cell that died before writing junit still gets a row.
"""

from __future__ import annotations

import glob
import json
import os
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from datetime import datetime

API = "https://api.github.com"

# A cell whose job produced no verdict is not a pass and not a failure. Keeping
# them apart is the point of the table.
VERDICT_ICON = {
    "success": "✅",
    "failure": "❌",
    "cancelled": "🚫",
    "skipped": "⏭️",
    "timed_out": "⏱️",
}


def fetch_jobs(repo: str, run_id: str, token: str) -> list[dict]:
    jobs: list[dict] = []
    page = 1
    while True:
        url = f"{API}/repos/{repo}/actions/runs/{run_id}/jobs?per_page=100&page={page}"
        req = urllib.request.Request(
            url,
            headers={
                "Authorization": f"Bearer {token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                payload = json.load(resp)
        except (urllib.error.URLError, TimeoutError) as exc:
            print(f"::warning::jobs API page {page} failed: {exc}", file=sys.stderr)
            break
        batch = payload.get("jobs", [])
        jobs.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return jobs


def duration_s(job: dict) -> int | None:
    started, completed = job.get("started_at"), job.get("completed_at")
    if not started or not completed:
        return None
    fmt = "%Y-%m-%dT%H:%M:%SZ"
    try:
        return int((datetime.strptime(completed, fmt) - datetime.strptime(started, fmt)).total_seconds())
    except ValueError:
        return None


def human(seconds: int | None) -> str:
    if seconds is None:
        return "—"
    if seconds < 0:
        return "0s"
    return f"{seconds // 60}m{seconds % 60:02d}s" if seconds >= 60 else f"{seconds}s"


def junit_counts(root: str) -> dict[str, dict]:
    """Map cell number to its aggregate junit counts and failing test names."""
    per_cell: dict[str, dict] = {}
    for path in glob.glob(os.path.join(root, "**", "junit-*.xml"), recursive=True):
        cell = cell_from_artifact_path(path)
        if cell is None:
            continue
        acc = per_cell.setdefault(cell, {"tests": 0, "failures": 0, "skipped": 0, "failing": []})
        try:
            root_el = ET.parse(path).getroot()
        except ET.ParseError as exc:
            print(f"::warning::unparseable junit {path}: {exc}", file=sys.stderr)
            continue
        suites = root_el.iter("testsuite") if root_el.tag != "testsuite" else [root_el]
        for suite in suites:
            acc["tests"] += int(suite.get("tests") or 0)
            acc["failures"] += int(suite.get("failures") or 0) + int(suite.get("errors") or 0)
            acc["skipped"] += int(suite.get("skipped") or 0)
        for case in root_el.iter("testcase"):
            if case.find("failure") is not None or case.find("error") is not None:
                name = case.get("name") or "?"
                if name not in acc["failing"]:
                    acc["failing"].append(name)
    return per_cell


def cell_from_artifact_path(path: str) -> str | None:
    """Recover the cell number from a junit-cell-<n>-<run_id> artifact directory."""
    for part in path.split(os.sep):
        if part.startswith("junit-cell-"):
            rest = part[len("junit-cell-"):]
            cell = rest.split("-", 1)[0]
            return cell if cell.isdigit() else None
    return None


def cell_number(job_name: str) -> str | None:
    """cell-19 (single/static/debian-13) -> 19."""
    if not job_name.startswith("cell-"):
        return None
    cell = job_name[len("cell-"):].split(" ", 1)[0]
    return cell if cell.isdigit() else None


def main() -> int:
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    token = os.environ.get("GITHUB_TOKEN", "")
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "")
    junit_root = os.environ.get("JUNIT_ROOT", "")

    if not (repo and run_id and token and summary_path):
        print("::warning::e2e-run-summary: missing required environment", file=sys.stderr)
        return 0

    jobs = fetch_jobs(repo, run_id, token)
    counts = junit_counts(junit_root) if junit_root and os.path.isdir(junit_root) else {}

    rows = []
    for job in jobs:
        cell = cell_number(job.get("name", ""))
        if cell is None:
            continue
        rows.append((int(cell), job))
    rows.sort(key=lambda r: r[0])

    lines = [f"## e2e nightly — run summary ({len(rows)} cells)", ""]
    if not rows:
        lines.append("No cell jobs found for this run.")
        write(summary_path, lines)
        return 0

    lines += [
        "| Cell | Topology | Result | Duration | Runner | Tests | Failed | Skipped |",
        "| --- | --- | --- | --- | --- | --- | --- | --- |",
    ]

    tally: dict[str, int] = {}
    total_tests = total_failed = total_seconds = 0
    failing_detail: list[tuple[int, list[str]]] = []

    for cell, job in rows:
        name = job.get("name", "")
        # A cell dispatched to a reusable workflow reports as
        # "cell-20 (baremetal/pxe/real-hw) / Bare-Metal Single-Node E2E".
        topo = name.split("(", 1)[1].split(")", 1)[0] if "(" in name else "—"
        verdict = job.get("conclusion") or job.get("status") or "unknown"
        tally[verdict] = tally.get(verdict, 0) + 1
        secs = duration_s(job)
        total_seconds += secs or 0
        c = counts.get(str(cell), {})
        total_tests += c.get("tests", 0)
        total_failed += c.get("failures", 0)
        if c.get("failing"):
            failing_detail.append((cell, c["failing"]))
        lines.append(
            f"| {cell} | {topo} | {VERDICT_ICON.get(verdict, '❔')} {verdict} | {human(secs)} "
            f"| {job.get('runner_name') or '—'} | {c.get('tests', '—')} "
            f"| {c.get('failures', '—')} | {c.get('skipped', '—')} |"
        )

    verdicts = ", ".join(f"{n} {v}" for v, n in sorted(tally.items(), key=lambda kv: -kv[1]))
    lines += [
        "",
        f"**Cells:** {verdicts}",
        f"**Tests:** {total_tests} run, {total_failed} failed",
        f"**Cell wall clock:** {human(total_seconds)} across all cells",
    ]

    slowest = sorted(rows, key=lambda r: duration_s(r[1]) or 0, reverse=True)[:3]
    if slowest:
        lines.append(
            "**Slowest:** " + ", ".join(f"cell {c} ({human(duration_s(j))})" for c, j in slowest)
        )

    if failing_detail:
        lines += ["", "<details><summary>Failing tests</summary>", ""]
        for cell, names in failing_detail:
            lines.append(f"- **cell {cell}** — " + ", ".join(f"`{n}`" for n in names))
        lines += ["", "</details>"]

    write(summary_path, lines)
    return 0


def write(path: str, lines: list[str]) -> None:
    with open(path, "a", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    sys.exit(main())
