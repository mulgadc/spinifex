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
    printf 'RDS_ENGINE=mariadb\n'
} >"${CASE}/agent.env"

# Simulate OpenRC sourcing the service before start_pre loads agent.env.
export RDS_HANDOFF_DIR="${source_time_handoff}"
# shellcheck source=rds-agent.initd
. "${SCRIPT_DIR}/rds-agent.initd"
export AGENT_ENV="${CASE}/agent.env"
export handoff_timeout=0

EEND_STATUS=
ebegin() { :; }
einfo() { :; }
eend() { EEND_STATUS=$1; }

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
if [ "${RDS_ENGINE:-}" != "mariadb" ]; then
    echo "FAIL: start_pre did not export RDS_ENGINE from agent.env" >&2
    exit 1
fi

echo "rds-agent.initd: all tests passed"
