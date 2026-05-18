#!/usr/bin/env python3
"""Render JUnit XML files into GitHub Actions job summary + annotations.

Inputs (env):
    JUNIT_GLOB           Glob matching JUnit XML files.
    JUNIT_TITLE          Heading text for the summary section.
    JUNIT_FAIL_ON_EMPTY  "true" to exit non-zero when glob matches nothing.

GitHub-Actions outputs (via $GITHUB_OUTPUT):
    failures             Sum of <failures> + <errors> across all suites.
    total                Sum of <tests> across all suites.

Side effects:
    Appends a markdown section to $GITHUB_STEP_SUMMARY with a per-file
    totals table and a collapsible details block per failed test.
    Emits one `::error` workflow command per failed/errored test case so
    they surface as annotations in the run UI.
"""

from __future__ import annotations

import glob
import os
import re
import sys
import xml.etree.ElementTree as ET
from dataclasses import dataclass


# Strip ANSI SGR escapes from skipped/failure CDATA so the rendered reason is
# plain text (CDATA preserves the colour codes Go emits via harness.Phase).
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")


def _strip_ansi(s: str) -> str:
    return _ANSI_RE.sub("", s)


# Last `<file>.go:<line>: <reason>` line in a CDATA block is the t.Skipf /
# t.Fatal call site — that's the actionable signal. Phase banners and probe
# logs above it are noise for the summary view.
_SKIP_REASON_RE = re.compile(r"([A-Za-z0-9_.\-/]+\.go:\d+:\s*.+)$")


def _extract_skip_reason(cdata: str) -> str:
    cdata = _strip_ansi(cdata).strip()
    if not cdata:
        return ""
    for line in reversed(cdata.splitlines()):
        line = line.strip()
        m = _SKIP_REASON_RE.search(line)
        if m:
            return m.group(1).strip()
    return cdata.splitlines()[-1].strip()


# Workflow commands carry their payload on one line; literal newlines must
# be percent-escaped per GitHub's workflow-command spec.
def _esc(s: str) -> str:
    return (
        s.replace("%", "%25")
        .replace("\r", "%0D")
        .replace("\n", "%0A")
    )


@dataclass
class Case:
    suite: str
    classname: str
    name: str
    time: float
    status: str  # "pass" | "fail" | "error" | "skip"
    message: str
    detail: str


@dataclass
class FileTotals:
    path: str
    tests: int
    failures: int
    errors: int
    skipped: int
    time: float
    cases: list[Case]


def _parse_file(path: str) -> FileTotals:
    root = ET.parse(path).getroot()
    # JUnit XML has two shapes: <testsuite> as root, or <testsuites> wrapping
    # multiple <testsuite> children. Iterating with .iter() handles both.
    suites = list(root.iter("testsuite"))
    tests = sum(int(s.get("tests", 0) or 0) for s in suites)
    failures = sum(int(s.get("failures", 0) or 0) for s in suites)
    errors = sum(int(s.get("errors", 0) or 0) for s in suites)
    skipped = sum(int(s.get("skipped", 0) or 0) for s in suites)
    time = sum(float(s.get("time", 0) or 0) for s in suites)

    cases: list[Case] = []
    for s in suites:
        suite_name = s.get("name", "")
        for tc in s.findall("testcase"):
            status = "pass"
            message = ""
            detail = ""
            for tag in ("failure", "error"):
                node = tc.find(tag)
                if node is not None:
                    status = "fail" if tag == "failure" else "error"
                    message = (node.get("message") or "").strip()
                    detail = (node.text or "").strip()
                    break
            skip_node = tc.find("skipped")
            if skip_node is not None and status == "pass":
                status = "skip"
                # Skipped reason: prefer the CDATA call-site line, fall back
                # to the @message attribute if CDATA is empty.
                cdata = (skip_node.text or "").strip()
                message = _extract_skip_reason(cdata) or (
                    skip_node.get("message") or ""
                ).strip()
            cases.append(
                Case(
                    suite=suite_name,
                    classname=tc.get("classname", ""),
                    name=tc.get("name", ""),
                    time=float(tc.get("time", 0) or 0),
                    status=status,
                    message=message,
                    detail=detail,
                )
            )

    # Drop container test cases: any case whose name is a strict prefix of
    # another case's name (with "/" separator) is a parent t.Run wrapper.
    # Go reports the parent's status as a rollup of its children, so counting
    # both double-tallies the same outcome (e.g. TestSingleNode failing
    # because Phase8b_VPCSubnetE2E failed would show as 2 failures, not 1).
    names = {c.name for c in cases}
    prefixes = {n.rsplit("/", 1)[0] for n in names if "/" in n}
    # A name is a container if some other name starts with "<name>/".
    # Use the prefixes set rather than O(n^2) scanning.
    def _is_container(name: str) -> bool:
        return name in prefixes

    leaf_cases = [c for c in cases if not _is_container(c.name)]

    # Recompute totals from leaf cases so the per-file counts in the summary
    # table match the de-duplicated failure list below it.
    leaf_tests = len(leaf_cases)
    leaf_failures = sum(1 for c in leaf_cases if c.status == "fail")
    leaf_errors = sum(1 for c in leaf_cases if c.status == "error")
    leaf_skipped = sum(1 for c in leaf_cases if c.status == "skip")

    return FileTotals(
        path=path,
        tests=leaf_tests,
        failures=leaf_failures,
        errors=leaf_errors,
        skipped=leaf_skipped,
        time=time,
        cases=leaf_cases,
    )


def _file_label(path: str) -> str:
    base = os.path.basename(path)
    if base.endswith(".xml"):
        base = base[:-4]
    if base.startswith("junit-"):
        base = base[len("junit-"):]
    return base or path


def main() -> int:
    pattern = os.environ.get("JUNIT_GLOB", "").strip()
    title = os.environ.get("JUNIT_TITLE", "Test results").strip() or "Test results"
    fail_on_empty = os.environ.get("JUNIT_FAIL_ON_EMPTY", "").strip().lower() == "true"

    if not pattern:
        print("::error::junit-report: junit-glob input is empty", file=sys.stderr)
        return 1

    paths = sorted(glob.glob(pattern))
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "/dev/null")
    output_path = os.environ.get("GITHUB_OUTPUT", "/dev/null")

    with open(summary_path, "a", encoding="utf-8") as summary, \
            open(output_path, "a", encoding="utf-8") as outputs:

        summary.write(f"## {title}\n\n")

        if not paths:
            summary.write(f"_No JUnit files matched `{pattern}`._\n\n")
            outputs.write("failures=0\n")
            outputs.write("total=0\n")
            if fail_on_empty:
                print(f"::error::junit-report: no files matched {pattern}", file=sys.stderr)
                return 1
            return 0

        totals_by_file = [_parse_file(p) for p in paths]
        grand_total = sum(f.tests for f in totals_by_file)
        grand_fail = sum(f.failures + f.errors for f in totals_by_file)
        grand_skip = sum(f.skipped for f in totals_by_file)
        grand_pass = grand_total - grand_fail - grand_skip
        grand_time = sum(f.time for f in totals_by_file)

        if grand_fail == 0:
            verdict = f"✅ **pass** — {grand_pass} / {grand_total}"
        else:
            verdict = f"❌ **{grand_fail} failed** — {grand_pass} / {grand_total} pass"
        skip_part = f", {grand_skip} skipped" if grand_skip else ""
        summary.write(f"{verdict}{skip_part} &nbsp; · &nbsp; {grand_time:.1f}s\n\n")

        # Compact table — single status column, shorter headers. Drops
        # separate err column (errors fold into fail count) so narrow
        # screens don't wrap.
        summary.write("| | Suite | Pass | Fail | Skip | Time |\n")
        summary.write("|:-:|---|---:|---:|---:|---:|\n")
        for f in totals_by_file:
            fails = f.failures + f.errors
            passed = f.tests - fails - f.skipped
            icon = "✅" if fails == 0 else "❌"
            summary.write(
                f"| {icon} | `{_file_label(f.path)}` | {passed} | {fails} | "
                f"{f.skipped} | {f.time:.1f}s |\n"
            )
        summary.write("\n")

        failed_cases = [
            c for f in totals_by_file for c in f.cases if c.status in ("fail", "error")
        ]
        if failed_cases:
            summary.write("---\n\n**Failures**\n\n")
            for c in failed_cases:
                header = f"{c.classname}.{c.name}" if c.classname else c.name
                # One line per failure: name + truncated message. Full
                # detail lives in the test artifact log; deep nesting and
                # long messages wrap badly on narrow screens.
                msg_short = (c.message or "").splitlines()[0] if c.message else ""
                if len(msg_short) > 120:
                    msg_short = msg_short[:117] + "…"
                line = f"- ❌ `{header}`"
                if msg_short:
                    line += f" — {msg_short}"
                summary.write(line + "\n")

                # Workflow annotation — surfaces in run header + Files-changed view.
                # classname for Go tests is the package path; map to a file when we can.
                file_hint = c.classname.replace(".", "/")
                msg = c.message or f"{c.status}: {header}"
                print(f"::error file={file_hint},title={header}::{_esc(msg)}")
            summary.write("\n")

        skipped_cases = [
            c for f in totals_by_file for c in f.cases if c.status == "skip"
        ]
        if skipped_cases:
            summary.write("---\n\n<details><summary><b>Skipped</b> ")
            summary.write(f"({len(skipped_cases)})</summary>\n\n")
            for c in skipped_cases:
                header = f"{c.classname}.{c.name}" if c.classname else c.name
                reason = (c.message or "").splitlines()[0] if c.message else ""
                if len(reason) > 160:
                    reason = reason[:157] + "…"
                line = f"- ⏭️ `{header}`"
                if reason:
                    line += f" — {reason}"
                summary.write(line + "\n")
            summary.write("\n</details>\n\n")

        outputs.write(f"failures={grand_fail}\n")
        outputs.write(f"total={grand_total}\n")

    return 0


if __name__ == "__main__":
    sys.exit(main())
