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
import sys
import xml.etree.ElementTree as ET
from dataclasses import dataclass


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
            if tc.find("skipped") is not None and status == "pass":
                status = "skip"
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

    return FileTotals(
        path=path,
        tests=tests,
        failures=failures,
        errors=errors,
        skipped=skipped,
        time=time,
        cases=cases,
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
        grand_time = sum(f.time for f in totals_by_file)

        verdict = ":white_check_mark: pass" if grand_fail == 0 else f":x: {grand_fail} failed"
        summary.write(
            f"**{verdict}** &middot; {grand_total} tests &middot; "
            f"{grand_skip} skipped &middot; {grand_time:.1f}s total\n\n"
        )

        summary.write("| Suite | Tests | Pass | Fail | Err | Skip | Time |\n")
        summary.write("|---|---:|---:|---:|---:|---:|---:|\n")
        for f in totals_by_file:
            passed = f.tests - f.failures - f.errors - f.skipped
            icon = ":white_check_mark:" if (f.failures + f.errors) == 0 else ":x:"
            summary.write(
                f"| {icon} `{_file_label(f.path)}` | {f.tests} | {passed} | "
                f"{f.failures} | {f.errors} | {f.skipped} | {f.time:.1f}s |\n"
            )
        summary.write("\n")

        failed_cases = [
            c for f in totals_by_file for c in f.cases if c.status in ("fail", "error")
        ]
        if failed_cases:
            summary.write("### Failures\n\n")
            for c in failed_cases:
                header = f"{c.classname}.{c.name}" if c.classname else c.name
                summary.write(f"<details><summary><code>{header}</code></summary>\n\n")
                if c.message:
                    summary.write(f"**{c.message}**\n\n")
                if c.detail:
                    summary.write("```\n" + c.detail + "\n```\n\n")
                summary.write("</details>\n\n")

                # Workflow annotation — surfaces in run header + Files-changed view.
                # classname for Go tests is the package path; map to a file when we can.
                file_hint = c.classname.replace(".", "/")
                msg = c.message or f"{c.status}: {header}"
                print(f"::error file={file_hint},title={header}::{_esc(msg)}")

        outputs.write(f"failures={grand_fail}\n")
        outputs.write(f"total={grand_total}\n")

    return 0


if __name__ == "__main__":
    sys.exit(main())
