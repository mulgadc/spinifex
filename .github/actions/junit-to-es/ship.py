#!/usr/bin/env python3
"""Ship per-test JUnit results to the Elasticsearch sink.

One doc per leaf test case, bulk-indexed into mulga-ci-test-results. The
mulga-ci-runs summary says which suite failed; this says which test, with the
failure text, so a per-test pass-rate trend outlives artifact retention.

Inputs (env):
    JUNIT_GLOB      Glob matching JUnit XML files. No matches is not an error:
                    a leg that never ran produces none.
    JUNIT_CELL      Cell label for these results (single, multi, gpu, 20, ...).
    MULGA_ES_SINK   host:port of the sink. Anonymous requests are superuser
                    there, so no credentials are involved.
    ES_INDEX        Target index; defaults to mulga-ci-test-results.
    RUN_ID, RUN_ATTEMPT, WORKFLOW, BRANCH, SHA, ARTIFACT_URL
"""

from __future__ import annotations

import glob
import json
import os
import re
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET
from datetime import datetime, timezone

# Go test output carries colour codes into the JUnit CDATA (harness.Phase
# emits them), which are noise in a stored failure message.
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")

# Enough failure text to recognise a failure; the artifact log stays the full
# record.
_MAX_MESSAGE = 4000

# go-junit-report emits this when a `=== RUN` line has no matching result line,
# which means the log it was fed was truncated or filtered. It is a parse gap,
# not a test outcome, so it is stored under its own status and left out of every
# pass rate rather than counted as a failure.
_NO_RESULT = "No test result found"


def _strip_ansi(s: str) -> str:
    return _ANSI_RE.sub("", s)


def _truncate(s: str) -> str:
    s = s.strip()
    return s if len(s) <= _MAX_MESSAGE else s[: _MAX_MESSAGE - 1] + "…"


def _suite_from_path(path: str) -> str:
    base = os.path.basename(path)
    if base.endswith(".xml"):
        base = base[:-4]
    if base.startswith("junit-"):
        base = base[len("junit-") :]
    return base


def _cases(path: str) -> list[dict]:
    """Parse one JUnit file into leaf test cases."""
    try:
        root = ET.parse(path).getroot()
    except ET.ParseError as exc:
        print(f"::warning::junit-to-es: {path} is not parseable ({exc}), skipped")
        return []

    # JUnit XML has two shapes: <testsuite> as root, or <testsuites> wrapping
    # several. .iter() handles both.
    out: list[dict] = []
    for s in root.iter("testsuite"):
        for tc in s.findall("testcase"):
            status = "pass"
            fail_type = ""
            message = ""
            for tag in ("failure", "error"):
                node = tc.find(tag)
                if node is not None:
                    status = "fail" if tag == "failure" else "error"
                    fail_type = tag
                    message = _strip_ansi(
                        ((node.get("message") or "") + "\n" + (node.text or "")).strip()
                    )
                    if (node.get("message") or "").strip() == _NO_RESULT:
                        status = "unknown"
                    break
            if status == "pass" and tc.find("skipped") is not None:
                status = "skip"
            out.append(
                {
                    "name": tc.get("name", ""),
                    "classname": tc.get("classname", ""),
                    "time": float(tc.get("time", 0) or 0),
                    "status": status,
                    "fail_type": fail_type,
                    "message": message,
                }
            )

    # Drop container cases: Go reports a parent t.Run as a rollup of its
    # children, so keeping both double-counts one outcome. Same rule the job
    # summary renderer applies.
    names = {c["name"] for c in out}
    containers = {n.rsplit("/", 1)[0] for n in names if "/" in n}
    return [c for c in out if c["name"] not in containers]


def _doc(cell: str, suite: str, case: dict, ts: str, ci: dict) -> dict:
    full = case["name"]
    top, _, sub = full.partition("/")
    doc = {
        "@timestamp": ts,
        "suite": suite,
        "status": case["status"],
        "duration_seconds": case["time"],
        "artifact_url": ci["artifact_url"],
        "labels": {"ci_run_id": ci["run_id"], "ci_cell": cell},
        "test": {
            "name": top,
            "subtest": sub,
            "full_name": full,
            "classname": case["classname"],
        },
        "ci": {
            "workflow": ci["workflow"],
            "branch": ci["branch"],
            "sha": ci["sha"],
            "run_attempt": ci["run_attempt"],
        },
    }
    if case["fail_type"]:
        doc["failure"] = {
            "type": case["fail_type"],
            "message": _truncate(case["message"]),
        }
    return doc


def _doc_id(ci: dict, cell: str, suite: str, full_name: str) -> str:
    # Deterministic so a replayed post overwrites instead of double-counting.
    # run_attempt is part of it because a re-run is a distinct result.
    return f"{ci['run_id']}-{ci['run_attempt']}-{cell}-{suite}-{full_name}"


def _bulk(sink: str, index: str, body: str) -> int:
    req = urllib.request.Request(
        f"http://{sink}/{index}/_bulk",
        data=body.encode("utf-8"),
        headers={"Content-Type": "application/x-ndjson"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, OSError) as exc:
        print(f"::error::junit-to-es: bulk POST to {sink}/{index} failed: {exc}")
        return 1

    if payload.get("errors"):
        for item in payload.get("items", []):
            err = next(iter(item.values())).get("error")
            if err:
                print(f"::error::junit-to-es: bulk rejected a doc: {json.dumps(err)}")
                return 1
    return 0


def main() -> int:
    pattern = os.environ.get("JUNIT_GLOB", "").strip()
    if not pattern:
        print("::error::junit-to-es: JUNIT_GLOB is empty")
        return 1
    cell = os.environ.get("JUNIT_CELL", "").strip()
    if not cell:
        print("::error::junit-to-es: JUNIT_CELL is empty")
        return 1
    sink = os.environ.get("MULGA_ES_SINK", "").strip()
    if not sink:
        print("::error::junit-to-es: MULGA_ES_SINK is empty")
        return 1
    index = os.environ.get("ES_INDEX", "").strip() or "mulga-ci-test-results"

    ci = {
        "run_id": os.environ.get("RUN_ID", ""),
        "run_attempt": os.environ.get("RUN_ATTEMPT", ""),
        "workflow": os.environ.get("WORKFLOW", ""),
        "branch": os.environ.get("BRANCH", ""),
        "sha": os.environ.get("SHA", ""),
        "artifact_url": os.environ.get("ARTIFACT_URL", ""),
    }
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    lines: list[str] = []
    counts: dict[str, int] = {"pass": 0, "fail": 0, "error": 0, "skip": 0, "unknown": 0}
    for path in sorted(glob.glob(pattern)):
        suite = _suite_from_path(path)
        for case in _cases(path):
            doc = _doc(cell, suite, case, ts, ci)
            counts[case["status"]] = counts.get(case["status"], 0) + 1
            meta = {"index": {"_id": _doc_id(ci, cell, suite, doc["test"]["full_name"])}}
            lines.append(json.dumps(meta))
            lines.append(json.dumps(doc))

    if not lines:
        # Not an error on its own: the leg may have been skipped, or died
        # before any test ran. The run summary doc records that separately.
        print(f"junit-to-es: no JUnit files matched {pattern}, nothing to post")
        return 0

    rc = _bulk(sink, index, "\n".join(lines) + "\n")
    if rc == 0:
        print(
            f"junit-to-es: posted {sum(counts.values())} test docs "
            f"(cell {cell}) to {sink}/{index} — {counts['pass']} pass, "
            f"{counts['fail']} fail, {counts['error']} error, "
            f"{counts['skip']} skip, {counts['unknown']} unknown"
        )
    return rc


if __name__ == "__main__":
    sys.exit(main())
