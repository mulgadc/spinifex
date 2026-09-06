#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
CASE=$(mktemp -d)
trap 'rm -rf "${CASE}"' EXIT

source_time_handoff="${CASE}/source-time"
configured_handoff="${CASE}/configured"
mkdir -p "${configured_handoff}"
touch "${configured_handoff}/bootstrap.env"
{
    printf 'RDS_HANDOFF_DIR=%s\n' "${configured_handoff}"
    printf 'RDS_ENGINE=postgres\n'
} >"${CASE}/agent.env"

# Simulate OpenRC sourcing the service before start_pre loads agent.env.
export RDS_HANDOFF_DIR="${source_time_handoff}"
# shellcheck source=rds-agent.initd
. "${SCRIPT_DIR}/rds-agent.initd"
export AGENT_ENV="${CASE}/agent.env"
export handoff_timeout=0

EEND_STATUS=
# A file rather than a variable: the mirror pipes tail into a while loop, which
# POSIX sh runs in a subshell, so an assignment there would not survive it.
CONSOLE="${CASE}/console"
: >"${CONSOLE}"
ebegin() { :; }
einfo() { printf '%s\n' "$*" >>"${CONSOLE}"; }
eerror() { printf '%s\n' "$*" >>"${CONSOLE}"; }
eend() { EEND_STATUS=$1; }
SYNCED=
sync() { SYNCED=1; }

start_pre
start_post

if [ "${HANDOFF_ENV}" != "${configured_handoff}/bootstrap.env" ]; then
    echo "FAIL: handoff path did not use RDS_HANDOFF_DIR from agent.env" >&2
    exit 1
fi
if [ "${EEND_STATUS}" != "0" ]; then
    echo "FAIL: post-start wait did not find the configured handoff" >&2
    exit 1
fi
# The agent checks this against the engine its own image bakes. Left out of the
# export list it would read as no assertion at all, so a VM launched as the
# wrong engine would bootstrap instead of refusing.
if [ "${RDS_ENGINE:-}" != "postgres" ]; then
    echo "FAIL: start_pre did not export RDS_ENGINE from agent.env" >&2
    exit 1
fi

# cloud-init writes agent.env once and never again, so the copy the next boot
# reads is whatever reached the disk. A reboot is a reset with no guest
# shutdown, and an unflushed agent.env leaves the agent with no gateway URL.
if [ "${SYNCED}" != "1" ]; then
    echo "FAIL: the handoff wait did not flush the config cloud-init wrote" >&2
    exit 1
fi

# A start that fails is the one case OpenRC runs no start_post for, so without
# this the console carries nothing but "died" and the reason is lost with the
# guest.
export output_log="${CASE}/agent.log"
printf 'rds-agent: startup failed err="build IMDS signer: no credentials"\n' >"${output_log}"
: >"${CONSOLE}"
default_start() { return 1; }
if start; then
    echo "FAIL: start reported success when the agent died" >&2
    exit 1
fi
if ! grep -q "build IMDS signer" "${CONSOLE}"; then
    echo "FAIL: a failed start did not mirror the agent log to the console" >&2
    exit 1
fi

echo "rds-agent.initd: all tests passed"
