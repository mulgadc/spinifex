#!/usr/bin/env python3
"""Write one Elasticsearch doc per CI job, so a failure joins to its host.

mulga-ci-runs says whether a run failed. This says which runner, env,
hypervisors and disk pools a job used, and how it ended, so a flake can be
correlated with host metrics for the same hypervisor and time window.

Commands:
    placement TFVARS   Print CI_HYPERVISORS=... and CI_POOLS=... for an env.
    record             Upsert the job-start doc (placement fields). Best effort.
    finalize           Upsert the outcome doc for every self-hosted job of a run.

Both writers upsert the same doc id, so their fields merge.

Env (record): CI_JOB_ID, RUN_ID, RUN_ATTEMPT, WORKFLOW, RUNNER_NAME, CI_ENV,
    CI_HYPERVISORS, CI_POOLS, CI_HYPERVISOR_POOLS, SPINIFEX_OTEL_ATTRS, JOB_NAME, BRANCH, SHA.
Env (finalize): GITHUB_TOKEN, REPO, RUN_ID, RUN_ATTEMPT.
Env (both): MULGA_ES_SINK host:port; ES_INDEX (default mulga-ci-jobs).
Finalize flags: --jobs-json FILE and --annotations-json FILE read recorded API
    payloads instead of calling GitHub; --dry-run prints the bulk body.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

API = "https://api.github.com"
DEFAULT_INDEX = "mulga-ci-jobs"
DEFAULT_SINK = "192.168.1.13:9200"
ANNOTATION_BUDGET_S = 120
_TS = "%Y-%m-%dT%H:%M:%SZ"

# Step names that decide the failure class. First failing step wins.
_STEP_CLASSES = [
    ("runner", re.compile(r"^(Set up job|Complete job|Reap Orphan|Clean Workspace)", re.I)),
    ("teardown", re.compile(r"^Teardown", re.I)),
    (
        "provision",
        re.compile(
            r"(Provision|Bootstrap|Build Install|Preflight hypervisor|Resolve (VM|cluster)|"
            r"Import .*AMI|Build .*AMI|Install kubectl|Map hypervisor)",
            re.I,
        ),
    ),
    (
        "test",
        re.compile(
            r"(^Run (?!actions/)|^Fail job if any suite failed|^Check every split suite|"
            r"^Workload and benchmark)",
            re.I,
        ),
    ),
]

# Annotation text GitHub writes on a job. Order matters: a human cancel also
# leaves "The operation was canceled." behind.
_CANCEL_BY = re.compile(r"The run was cancel(?:l)?ed by @([A-Za-z0-9-]+)")
_SUPERSEDED = re.compile(r"Canceling since a higher priority waiting request")
_TIMEOUT = re.compile(r"exceeded the maximum execution time")
_RUNNER_LOST = re.compile(r"lost communication with the server|received a shutdown signal")


def parse_tfvars(text: str) -> tuple[list[str], list[str], list[str]]:
    """Return sorted hypervisors, pools and host:pool pairs of an env tfvars.

    The pairs keep which pool belongs to which host; the other two lists lose it.
    """
    hosts: set[str] = set()
    pools: set[str] = set()
    pairs: set[str] = set()
    for line in text.splitlines():
        if line.lstrip().startswith("#"):
            continue
        m = re.search(r'\{\s*name\s*=\s*"([^"]+)"', line)
        if not m:
            continue
        hosts.add(m.group(1))
        p = re.search(r'storage_pool\s*=\s*"([^"]+)"', line)
        if p:
            pools.add(p.group(1))
            pairs.add(f"{m.group(1)}:{p.group(1)}")
    return sorted(hosts), sorted(pools), sorted(pairs)


def otel_attr(attrs: str, key: str) -> str:
    for pair in attrs.split(","):
        k, _, v = pair.partition("=")
        if k.strip() == key:
            return v.strip()
    return ""


def cell_from_name(name: str) -> str:
    """cell-24 (x/y) -> 24; the e2e.yml job names map to their cell label."""
    m = re.match(r"^cell-(\d+)\b", name)
    if m:
        return m.group(1)
    if name == "Single-Node E2E":
        return "single"
    if name == "Multi-Node E2E":
        return "multi"
    m = re.match(r"^Single-Node E2E \((.+)\)$", name)
    if m:
        leg = m.group(1)
        return leg if leg.startswith("single-") else f"single-{leg}"
    return ""


def _secs(a: str | None, b: str | None) -> int | None:
    if not a or not b:
        return None
    try:
        d = datetime.strptime(b, _TS) - datetime.strptime(a, _TS)
    except ValueError:
        return None
    return max(int(d.total_seconds()), 0)


def classify(job: dict, annotations: list[str]) -> tuple[str, str, str]:
    """Return (failure_class, failed_step, cancelled_by) for a job."""
    conclusion = job.get("conclusion") or ""
    if conclusion == "success":
        return "", "", ""

    failed_step = ""
    for step in job.get("steps") or []:
        if step.get("conclusion") == "failure":
            failed_step = step.get("name", "")
            break

    if conclusion == "timed_out":
        return "timeout", failed_step, ""

    for msg in annotations:
        m = _CANCEL_BY.search(msg)
        if m:
            return "cancelled_human", failed_step, m.group(1)
    for msg in annotations:
        if _TIMEOUT.search(msg):
            return "timeout", failed_step, ""
        if _SUPERSEDED.search(msg):
            return "cancelled_superseded", failed_step, ""
        if _RUNNER_LOST.search(msg):
            return "runner", failed_step, ""

    if conclusion == "cancelled":
        return "cancelled", failed_step, ""
    for cls, rx in _STEP_CLASSES:
        if failed_step and rx.search(failed_step):
            return cls, failed_step, ""
    return "other", failed_step, ""


def outcome_doc(job: dict, annotations: list[str], fallback: dict) -> dict:
    cls, failed_step, cancelled_by = classify(job, annotations)
    doc = {
        "@timestamp": job.get("started_at") or job.get("created_at"),
        "run_id": str(job.get("run_id", fallback.get("run_id", ""))),
        "run_attempt": str(job.get("run_attempt", fallback.get("run_attempt", ""))),
        "job_id": str(job["id"]),
        "job_name": job.get("name", ""),
        "workflow": job.get("workflow_name", ""),
        "runner_name": job.get("runner_name") or "",
        "conclusion": job.get("conclusion") or "",
        "failure_class": cls,
        "failed_step": failed_step,
        "started_at": job.get("started_at"),
        "completed_at": job.get("completed_at"),
        "duration_seconds": _secs(job.get("started_at"), job.get("completed_at")),
        "queue_seconds": _secs(job.get("created_at"), job.get("started_at")),
        "branch": job.get("head_branch") or fallback.get("branch", ""),
        "sha": job.get("head_sha") or fallback.get("sha", ""),
        "job_url": job.get("html_url", ""),
    }
    if cancelled_by:
        doc["cancelled_by"] = cancelled_by
    cell = cell_from_name(job.get("name", ""))
    if cell:
        doc["cell"] = cell
    return {k: v for k, v in doc.items() if v not in (None, "")}


def placement_doc(env: dict[str, str]) -> dict | None:
    job_id = env.get("CI_JOB_ID", "")
    if not job_id:
        return None
    attrs = env.get("SPINIFEX_OTEL_ATTRS", "")
    doc = {
        "@timestamp": datetime.now(timezone.utc).strftime(_TS),
        "run_id": env.get("RUN_ID", ""),
        "run_attempt": env.get("RUN_ATTEMPT", ""),
        "job_id": job_id,
        "job_name": env.get("JOB_NAME", ""),
        "workflow": env.get("WORKFLOW", ""),
        "cell": otel_attr(attrs, "ci.cell"),
        "runner_name": env.get("RUNNER_NAME", ""),
        "env": env.get("CI_ENV", ""),
        "hypervisors": [h for h in env.get("CI_HYPERVISORS", "").split(",") if h],
        "pools": [p for p in env.get("CI_POOLS", "").split(",") if p],
        "hypervisor_pools": [p for p in env.get("CI_HYPERVISOR_POOLS", "").split(",") if p],
        "branch": env.get("BRANCH", ""),
        "sha": env.get("SHA", ""),
    }
    return {k: v for k, v in doc.items() if v not in (None, "", [])}


def doc_id(doc: dict) -> str:
    return f"{doc['run_id']}-{doc['run_attempt']}-{doc['job_id']}"


def bulk_body(docs: list[dict]) -> str:
    lines: list[str] = []
    for d in docs:
        lines.append(json.dumps({"update": {"_id": doc_id(d)}}))
        lines.append(json.dumps({"doc": d, "doc_as_upsert": True}))
    return "\n".join(lines) + "\n"


def template_keywords(sink: str, name: str) -> set[str] | None:
    """Keyword fields of the sink's index template for the index, or None when
    there is no template. Without one the first doc would create the index
    with dynamic mappings and no retention."""
    try:
        with urllib.request.urlopen(f"http://{sink}/_index_template/{name}", timeout=10) as resp:
            body = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        reason = f"no index template {name}" if exc.code == 404 else f"template check returned {exc.code}"
    except (urllib.error.URLError, OSError, ValueError) as exc:
        reason = f"sink unreachable: {exc}"
    else:
        found: set[str] = set()
        for entry in body.get("index_templates", []):
            props = entry.get("index_template", {}).get("template", {}).get("mappings", {}).get("properties", {})
            found.update(k for k, v in props.items() if v.get("type") == "keyword")
        return found
    print(f"::notice::ci-job-doc: not writing to {sink}/{name}: {reason}")
    return None


def field_is_keyword(mapping: dict, field: str) -> bool:
    """True when every index in a GET _mapping/field response maps the field
    as keyword."""
    if not mapping:
        return False
    for idx in mapping.values():
        entry = idx.get("mappings", {}).get(field, {}).get("mapping", {})
        if not any(v.get("type") == "keyword" for v in entry.values()):
            return False
    return True


def index_keeps_field(sink: str, index: str, field: str) -> bool:
    """False when the index already exists and does not map the field as
    keyword. A missing index takes its mapping from the template."""
    try:
        with urllib.request.urlopen(f"http://{sink}/{index}/_mapping/field/{field}", timeout=10) as resp:
            return field_is_keyword(json.loads(resp.read().decode("utf-8")), field)
    except urllib.error.HTTPError as exc:
        return exc.code == 404
    except (urllib.error.URLError, OSError, ValueError):
        return False


def drop_unmapped(body: str, keywords: set[str]) -> str:
    """Remove the host and pool pair field from each doc until the template
    maps it as keyword, so a dynamic mapping never decides the join."""
    if "hypervisor_pools" in keywords or "hypervisor_pools" not in body:
        return body
    print("::notice::ci-job-doc: hypervisor_pools is not keyword-mapped on the template, left out")
    out: list[str] = []
    for line in body.splitlines():
        obj = json.loads(line)
        if "doc" in obj:
            obj["doc"].pop("hypervisor_pools", None)
        out.append(json.dumps(obj))
    return "\n".join(out) + "\n"


WRITTEN, FAILED, SKIPPED = 0, 1, 2


def post_bulk(sink: str, index: str, body: str) -> int:
    """Return WRITTEN once the sink accepts the bulk, SKIPPED when the sink has
    no template yet and nothing was sent, FAILED otherwise."""
    keywords = template_keywords(sink, index)
    if keywords is None:
        return SKIPPED
    if not index_keeps_field(sink, index, "hypervisor_pools"):
        keywords = keywords - {"hypervisor_pools"}
    body = drop_unmapped(body, keywords)
    req = urllib.request.Request(
        f"http://{sink}/{index}/_bulk",
        data=body.encode("utf-8"),
        headers={"Content-Type": "application/x-ndjson"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, OSError, ValueError) as exc:
        print(f"::warning::ci-job-doc: bulk POST to {sink}/{index} failed: {exc}")
        return FAILED
    if payload.get("errors"):
        for item in payload.get("items", []):
            err = next(iter(item.values())).get("error")
            if err:
                print(f"::warning::ci-job-doc: bulk rejected a doc: {json.dumps(err)}")
                return FAILED
    return WRITTEN


def _get(url: str, token: str, wait: int = 30) -> object:
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        },
    )
    with urllib.request.urlopen(req, timeout=wait) as resp:
        return json.load(resp)


def fetch_jobs(repo: str, run_id: str, attempt: str, token: str) -> list[dict]:
    jobs: list[dict] = []
    page = 1
    while True:
        url = f"{API}/repos/{repo}/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100&page={page}"
        batch = _get(url, token).get("jobs", [])  # type: ignore[union-attr]
        jobs.extend(batch)
        if len(batch) < 100:
            return jobs
        page += 1


def fetch_annotations(repo: str, job_id: int, token: str) -> list[str]:
    try:
        items = _get(f"{API}/repos/{repo}/check-runs/{job_id}/annotations?per_page=50", token, 10)
    except (urllib.error.URLError, OSError, ValueError) as exc:
        print(f"::warning::ci-job-doc: annotations for job {job_id} failed: {exc}")
        return []
    return [a.get("message", "") for a in items]  # type: ignore[union-attr]


def wanted(job: dict) -> bool:
    """Self-hosted jobs that finished on a runner. The caller's own job is
    still running and GitHub-hosted jobs hold no env, so both drop out."""
    return (
        " / " not in job.get("name", "")
        and "self-hosted" in (job.get("labels") or [])
        and job.get("status") == "completed"
        and job.get("conclusion") != "skipped"
        and bool(job.get("runner_name"))
    )


def cmd_finalize(args: argparse.Namespace) -> int:
    sink = os.environ.get("MULGA_ES_SINK", "") or DEFAULT_SINK
    index = os.environ.get("ES_INDEX", "") or DEFAULT_INDEX
    fallback = {
        "run_id": os.environ.get("RUN_ID", ""),
        "branch": os.environ.get("BRANCH", ""),
        "sha": os.environ.get("SHA", ""),
    }
    repo = os.environ.get("REPO", "")
    token = os.environ.get("GITHUB_TOKEN", "")

    try:
        if args.jobs_json:
            with open(args.jobs_json, encoding="utf-8") as fh:
                jobs = json.load(fh).get("jobs", [])
        else:
            jobs = fetch_jobs(repo, fallback["run_id"], os.environ.get("RUN_ATTEMPT", "1"), token)
    except (urllib.error.URLError, OSError, ValueError) as exc:
        print(f"::warning::ci-job-doc: could not read jobs: {exc}")
        return 0

    recorded: dict[str, list[str]] = {}
    if args.annotations_json:
        with open(args.annotations_json, encoding="utf-8") as fh:
            recorded = json.load(fh)

    # Jobs that run on the runner itself provision no tofu env, so no start
    # doc exists. They carry env "none" rather than a blank placement.
    pattern = os.environ.get("CI_NO_PLACEMENT_JOBS", "")
    no_placement = re.compile(pattern) if pattern else None

    docs = []
    # Annotations are enrichment: stop asking once the budget is spent so the
    # outcomes still get posted inside the job's limit.
    budget_end = time.monotonic() + ANNOTATION_BUDGET_S
    for job in jobs:
        if not wanted(job):
            continue
        notes: list[str] = []
        if job.get("conclusion") != "success":
            if args.annotations_json:
                notes = recorded.get(str(job["id"]), [])
            elif time.monotonic() < budget_end:
                notes = fetch_annotations(repo, job["id"], token)
        doc = outcome_doc(job, notes, fallback)
        if no_placement and no_placement.search(job.get("name", "")):
            doc["env"] = "none"
        docs.append(doc)

    if not docs:
        print("ci-job-doc: no self-hosted jobs to record")
        return 0
    body = bulk_body(docs)
    if args.dry_run:
        sys.stdout.write(body)
        return 0
    rc = post_bulk(sink, index, body)
    if rc == WRITTEN:
        print(f"ci-job-doc: upserted {len(docs)} job docs to {sink}/{index}")
    elif rc == SKIPPED:
        print(f"::notice::ci-job-doc: skipped {len(docs)} job docs, none written to {sink}/{index}")
    return 0


def cmd_record(_: argparse.Namespace) -> int:
    doc = placement_doc(dict(os.environ))
    if doc is None:
        print("ci-job-doc: no CI_JOB_ID, placement not recorded")
        return 0
    sink = os.environ.get("MULGA_ES_SINK", "") or DEFAULT_SINK
    index = os.environ.get("ES_INDEX", "") or DEFAULT_INDEX
    post_bulk(sink, index, bulk_body([doc]))
    return 0


def cmd_placement(args: argparse.Namespace) -> int:
    try:
        with open(args.tfvars, encoding="utf-8") as fh:
            hosts, pools, pairs = parse_tfvars(fh.read())
    except OSError:
        return 0
    print(f"CI_HYPERVISORS={','.join(hosts)}")
    print(f"CI_POOLS={','.join(pools)}")
    print(f"CI_HYPERVISOR_POOLS={','.join(pairs)}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("placement")
    p.add_argument("tfvars")
    p.set_defaults(fn=cmd_placement)
    sub.add_parser("record").set_defaults(fn=cmd_record)
    f = sub.add_parser("finalize")
    f.add_argument("--jobs-json")
    f.add_argument("--annotations-json")
    f.add_argument("--dry-run", action="store_true")
    f.set_defaults(fn=cmd_finalize)
    args = ap.parse_args()
    return args.fn(args)


if __name__ == "__main__":
    sys.exit(main())
