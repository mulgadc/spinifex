#!/usr/bin/env bash
# Checks that the install and public-subnet steps of deploy-vultr.sh fail when a command in them
# fails or its effect does not read back. The node commands run here, with curl and aws replaced
# by stand-ins and /root/ moved to a scratch directory.
# preflight runs this with sh
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
set -euo pipefail
exec </dev/null
here="$(cd "$(dirname "$0")" && pwd)"
script="${1:-$here/deploy-vultr.sh}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/root"

cat > "$work/bin/curl" <<'SH'
#!/bin/sh
[ -z "${FAKE_CURL_FAIL:-}" ] || exit 22
while [ "$#" -gt 0 ]; do case "$1" in -o) out="$2"; shift 2 ;; *) shift ;; esac; done
printf 'echo "installer ran $*" >> "%s"\n' "$FAKE_LOG" > "${out:-/dev/stdout}"
SH
cat > "$work/bin/aws" <<'SH'
#!/bin/sh
echo "aws $*" >> "$FAKE_LOG"
case "$*" in
    *"describe-subnets --filters"*) [ -z "${FAKE_DESCRIBE_FAIL:-}" ] || exit 255; echo "subnet-a subnet-b" ;;
    *"describe-subnets --subnet-ids"*) echo "${FAKE_MAP:-True}" ;;
esac
SH
chmod +x "$work/bin/curl" "$work/bin/aws"
export PATH="$work/bin:$PATH" FAKE_LOG="$work/log"

# Load only the functions under test, with on running its command here rather than over SSH.
fns="$(sed -n -e '/^log() /p' -e '/^die() /p' -e '/^install_spinifex() {/,/^}/p' \
    -e '/^public_default_subnets() {/,/^}/p' "$script")"
eval "$fns"
on() { bash -c "${*//\/root\//$work/root/}"; }
# shellcheck disable=SC2034 # read by the eval-loaded functions
DISTRO="" CHANNEL=latest IP=203.0.113.1

fail=0
# expect NAME WANT_RC FN: run FN in a subshell and compare its status, 0 or non-zero, to WANT_RC.
expect() {
    local rc=0
    : > "$FAKE_LOG"
    ("$3") 2>/dev/null || rc=$?
    if { [ "$2" = 0 ] && [ "$rc" = 0 ]; } || { [ "$2" != 0 ] && [ "$rc" != 0 ]; }; then
        echo "ok   $1"
    else
        echo "FAIL $1: exit $rc"; fail=1
    fi
}

FAKE_CURL_FAIL=1 expect "install fails when the download fails" 1 install_spinifex
grep -q 'installer ran' "$FAKE_LOG" && { echo "FAIL the installer ran without a download"; fail=1; }
expect "install runs the downloaded installer" 0 install_spinifex
grep -q 'installer ran --channel latest' "$FAKE_LOG" || { echo "FAIL the installer did not run"; fail=1; }

FAKE_DESCRIBE_FAIL=1 expect "public step fails when describe-subnets fails" 1 public_default_subnets
FAKE_MAP=False expect "public step fails when MapPublicIpOnLaunch reads back False" 1 public_default_subnets
expect "public step turns on every default subnet" 0 public_default_subnets
for s in subnet-a subnet-b; do
    grep -q "modify-subnet-attribute --subnet-id $s --map-public-ip-on-launch" "$FAKE_LOG" ||
        { echo "FAIL $s was not modified"; fail=1; }
    grep -q "describe-subnets --subnet-ids $s" "$FAKE_LOG" || { echo "FAIL $s was not read back"; fail=1; }
done
exit "$fail"
