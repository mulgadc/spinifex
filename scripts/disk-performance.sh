#!/bin/bash
# disk-performance.sh — fio random 70/30 read-write swept across block sizes.
#
# Usage: disk-performance.sh
#
# Environment:
#   BENCH_DIR    Where fio writes its data files (default: $HOME/bench)
#   OUT_DIR      Where JSON results are written (default: /tmp/spinifex-disk-bench)
#   SIZE         Per-job file size (default: 256M)
#   JOBS         Concurrent jobs (default: 4)
#   BLOCK_SIZES  Block sizes to sweep (default: "4k 16k 128k 1M")
set -euo pipefail

BENCH_DIR="${BENCH_DIR:-$HOME/bench}"
OUT_DIR="${OUT_DIR:-/tmp/spinifex-disk-bench}"
SIZE="${SIZE:-256M}"
JOBS="${JOBS:-4}"
BLOCK_SIZES="${BLOCK_SIZES:-4k 16k 128k 1M}"

# BENCH_DIR stays on the disk under test and must not move to /tmp: where /tmp
# is tmpfs this would measure RAM and report a number that looks excellent and
# means nothing. Only the JSON results go to /tmp.
#
# SIZE defaults small because JOBS x SIZE at direct=1 already defeats the host
# page cache. Measuring a guest volume through nbdkit/viperblock/predastore is
# a different question and wants several GiB per job — raise SIZE for that.

if ! command -v fio >/dev/null || ! command -v jq >/dev/null; then
    sudo apt-get update -qq
    sudo apt-get install -y -qq fio sysstat jq util-linux
fi

mkdir -p "$BENCH_DIR" "$OUT_DIR"

# The data files, not the results. Left behind, the next run reads back a file
# it did not write.
trap 'rm -rf "${BENCH_DIR:?}"' EXIT

echo "disk-performance: $JOBS jobs x $SIZE, randrw 70/30, direct=1, data in $BENCH_DIR"

for bs in $BLOCK_SIZES; do
    fio --name="randrw_70_30_$bs" \
        --directory="$BENCH_DIR" \
        --rw=randrw \
        --rwmixread=70 \
        --bs="$bs" \
        --size="$SIZE" \
        --numjobs="$JOBS" \
        --iodepth=32 \
        --ioengine=libaio \
        --direct=1 \
        --group_reporting \
        --output-format=json \
        --output="$OUT_DIR/$bs.json" >/dev/null

    # group_reporting folds every job into jobs[0], so this is the whole run.
    # bw is KiB/s, clat is nanoseconds.
    jq -r --arg bs "$bs" '.jobs[0] |
        "  \($bs)\tread \(.read.iops | round) IOPS \(.read.bw / 1024 | floor) MiB/s" +
        "\twrite \(.write.iops | round) IOPS \(.write.bw / 1024 | floor) MiB/s"' \
        "$OUT_DIR/$bs.json"
done

echo "disk-performance: results in $OUT_DIR"
