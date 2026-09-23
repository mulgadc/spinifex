#!/bin/sh
# uninstall-spx_test.sh — tests for the shipped uninstaller.
#
# Everything here runs against the refusal and help paths, which return before
# the script touches the host. Nothing in this file may reach a path that
# removes anything: the script under test is the one that deletes the install,
# so a test that ran it for real would uninstall the machine running CI.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
UNINSTALL="$SCRIPT_DIR/uninstall-spx.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

check() { # check <description> <condition-result>
    if [ "$2" = 0 ]; then pass "$1"; else fail "$1"; fi
}

echo "-- uninstall-spx.sh"

# --------------------------------------------------------------------------
# Help and argument parsing.
# --------------------------------------------------------------------------
help_out=$("$UNINSTALL" --help 2>&1) && rc=0 || rc=$?
check "--help exits 0" "$rc"

# Every flag the parser accepts must appear in the help text. This is what
# catches a flag added to the case arms and never documented, which on a
# destructive script is how an operator finds --purge-data by accident.
for flag in --purge-data --purge-deps --yes --dry-run; do
    case "$help_out" in
        *"$flag"*) pass "--help documents $flag" ;;
        *) fail "--help does not document $flag" ;;
    esac
done

# The header is printed up to the first non-comment line, so a truncated block
# means the range drifted. The last paragraph is the one that used to be cut.
case "$help_out" in
    *"netplan"*) pass "--help prints the whole header" ;;
    *) fail "--help output is truncated before the end of the header" ;;
esac

case "$help_out" in
    *'#'*) fail "--help leaves comment markers in the output" ;;
    *) pass "--help strips comment markers" ;;
esac

"$UNINSTALL" --not-a-real-flag >"$TMP/out" 2>"$TMP/err" && rc=0 || rc=$?
[ "$rc" = 1 ] && pass "unknown option exits 1" || fail "unknown option exited $rc, want 1"
grep -q "unknown option" "$TMP/err" \
    && pass "unknown option explains itself on stderr" \
    || fail "unknown option wrote nothing useful to stderr"

# --------------------------------------------------------------------------
# The multi-node refusal.
#
# Uninstalling one node of a running cluster strands its guests, so this is a
# structural refusal rather than a prompt. The property under test is that
# --yes does not cover it — a flag meaning "don't ask me" must not also mean
# "ignore a safety property".
# --------------------------------------------------------------------------
mkcfg() { # mkcfg <dir> <node>...
    dir="$1"
    shift
    mkdir -p "$dir"
    {
        echo '[network]'
        for n in "$@"; do
            echo "[nodes.$n]"
            echo "advertise = \"10.0.0.1\""
            echo "[nodes.$n.daemon]"
        done
    } >"$dir/spinifex.toml"
}

mkcfg "$TMP/three" node1 node2 node3
mkcfg "$TMP/one" node1

for args in "" "--yes" "--yes --dry-run"; do
    # shellcheck disable=SC2086
    SPX_ETC_DIR="$TMP/three" "$UNINSTALL" $args >"$TMP/out" 2>"$TMP/err" && rc=0 || rc=$?
    label=${args:-"(no flags)"}
    [ "$rc" = 1 ] \
        && pass "three-node config refuses with $label" \
        || fail "three-node config with $label exited $rc, want 1"
    grep -q "3-node cluster" "$TMP/err" \
        && pass "refusal names the node count for $label" \
        || fail "refusal for $label did not name the node count"
    grep -q "spx admin cluster shutdown" "$TMP/err" \
        && pass "refusal for $label says how to proceed" \
        || fail "refusal for $label does not say how to proceed"
done

grep -q "node1 node2 node3" "$TMP/err" \
    && pass "refusal lists the configured nodes" \
    || fail "refusal does not list the configured nodes"

# A single-node config must get past the refusal. Asserted by what it is NOT:
# reaching the refusal message. --dry-run keeps it from touching the host, and
# the exit status is left alone because the dry run's later steps depend on
# whatever is installed on the machine running this test.
SPX_ETC_DIR="$TMP/one" "$UNINSTALL" --dry-run >"$TMP/out" 2>"$TMP/err" || true
grep -q "node cluster" "$TMP/err" \
    && fail "single-node config was refused as multi-node" \
    || pass "single-node config is not refused"

# --------------------------------------------------------------------------
# The dry run's promise.
#
# --dry-run must print the plan and touch nothing, which holds only while every
# mutating command goes through run() or rm_path(). A bare `sudo rm` or `sudo
# systemctl` added later would ignore the flag and delete for real during what
# the operator asked to be a rehearsal. Guarded blocks are allowed, so lines
# inside an `if ! $DRY_RUN` are not the concern — an unguarded one is.
# --------------------------------------------------------------------------
stray=$(awk '
    # The helpers themselves are where the guarded mutation is meant to live.
    /^(run|rm_path)\(\) \{/ { helper = 1 }
    helper                  { if (/^\}$/) helper = 0; next }
    /if ! \$DRY_RUN/ || /\$DRY_RUN && / { guard = 1 }
    /^fi$/ || /^else$/                  { guard = 0 }
    guard                               { next }
    /^[[:space:]]*#/                    { next }
    /^[[:space:]]*(run|exists|rm_path)[[:space:]]/ { next }
    /^[[:space:]]*sudo[[:space:]]+(rm|userdel|groupdel|systemctl[[:space:]]+(disable|stop|restart))/ {
        printf "%d: %s\n", NR, $0
    }
' "$UNINSTALL")

if [ -n "$stray" ]; then
    fail "mutating commands outside run()/rm_path() and outside a \$DRY_RUN guard:"
    echo "$stray" | sed 's/^/         /'
else
    pass "every mutation goes through run()/rm_path() or a \$DRY_RUN guard"
fi

# The nft teardown must name our own table. A global flush would take the
# host's other rules with it.
grep -q 'nft delete table inet spinifex_filter' "$UNINSTALL" \
    && pass "nft teardown deletes only inet spinifex_filter" \
    || fail "nft teardown does not delete inet spinifex_filter by name"
grep -qE 'nft[[:space:]]+(-[a-z][[:space:]]+)*flush ruleset' "$UNINSTALL" \
    && fail "nft teardown flushes the whole ruleset" \
    || pass "nft teardown never flushes the whole ruleset"

# --------------------------------------------------------------------------
if [ "$fails" -gt 0 ]; then
    echo "  $fails check(s) failed"
    exit 1
fi
echo "  uninstall-spx.sh ok"
