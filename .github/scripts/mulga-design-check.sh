#!/usr/bin/env bash
# Warn when a PR body has no filled "Mulga design" line. Reads PR_BODY and
# never fails the job: the check is a prompt, not a gate.
set -euo pipefail
shopt -s nocasematch

section_re='^#{1,6}[[:space:]]*Mulga design[[:space:]]*$'
line_re='^[[:space:]]*(Updated|Not needed)[[:space:]]*:[[:space:]]*(.*)$'
placeholder_re='<[^<>]*>'

filled() {
  local body=$1 line value kept inside=0 pre rest
  body=${body//$'\r'/}
  while [[ $body == *'<!--'*'-->'* ]]; do
    pre=${body%%'<!--'*}
    rest=${body#*'<!--'}
    body=$pre${rest#*'-->'}
  done
  while IFS= read -r line; do
    if [[ $line =~ $section_re ]]; then
      inside=1
    elif ((inside)) && [[ ${line#"${line%%[![:space:]]*}"} == '#'* ]]; then
      return 1
    elif ((inside)) && [[ $line =~ $line_re ]]; then
      value=${BASH_REMATCH[2]} kept=''
      while [[ $value =~ $placeholder_re ]]; do
        kept+=${value%%"${BASH_REMATCH[0]}"*}
        value=${value#*"${BASH_REMATCH[0]}"}
      done
      [[ $kept$value == *[![:space:]]* ]] && return 0
    fi
  done <<<"$body"
  return 1
}

if filled "${PR_BODY:-}"; then
  echo "The Mulga design line is filled in."
else
  echo '::warning title=Mulga design::Add a line under "## Mulga design": "Updated: <mulga PR or bead id>" or "Not needed: <reason>".'
fi
