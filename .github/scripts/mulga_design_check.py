#!/usr/bin/env python3
"""Warn when a PR body has no filled "Mulga design" line. Reads PR_BODY; never fails the job."""

import os
import re

COMMENT_RE = re.compile(r"<!--.*?-->", re.S)
SECTION_RE = re.compile(r"^#{1,6}\s*Mulga design\s*$", re.I)
LINE_RE = re.compile(r"^\s*(Updated|Not needed)\s*:\s*(.*?)\s*$", re.I)
PLACEHOLDER_RE = re.compile(r"<[^<>]*>")


def filled(body):
    inside = False
    for line in COMMENT_RE.sub("", body or "").splitlines():
        if SECTION_RE.match(line):
            inside = True
        elif inside and line.lstrip().startswith("#"):
            return False
        elif inside:
            m = LINE_RE.match(line)
            if m and PLACEHOLDER_RE.sub("", m.group(2)).strip():
                return True
    return False


def main():
    if filled(os.environ.get("PR_BODY", "")):
        print("The Mulga design line is filled in.")
    else:
        print('::warning title=Mulga design::Add a line under "## Mulga design": "Updated: <mulga PR or bead id>" or "Not needed: <reason>".')
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
