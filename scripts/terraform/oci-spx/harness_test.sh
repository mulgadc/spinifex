#!/usr/bin/env bash
# harness_test.sh — the OCI harness's defaults without a tenancy:
# setup-identity.sh grants ocibsd unless told otherwise, and validate-topology.sh
# accepts the retired -ocibsd topology names only to destroy their state.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup-identity.sh passes ebs_provider=ocibsd when --ebs-provider is omitted"
# A copy beside a stub oci_env.py, which the script runs Terraform through.
mkdir -p "$TMP/identity/scripts"
cp "$HERE/setup-identity.sh" "$TMP/identity/"
printf '%s\n' 'import sys' 'open(sys.argv[0] + ".calls", "a").write(" ".join(sys.argv[1:]) + "\n")' \
    > "$TMP/identity/scripts/oci_env.py"
touch "$TMP/key.pub"
run_identity() {
    : > "$TMP/identity/scripts/oci_env.py.calls"
    OCI_SSH_PUBLIC_KEY="$TMP/key.pub" OCI_STATE_ROOT="$TMP/identity" \
        bash "$TMP/identity/setup-identity.sh" --dry-run "$@" > "$TMP/identity/out" 2>&1
}
plan_call() { grep ' plan ' "$TMP/identity/scripts/oci_env.py.calls"; }
if run_identity && plan_call | grep -q -- '-var ebs_provider=ocibsd '; then
    pass "the plan grants ocibsd by default"
else
    fail "default plan: $(cat "$TMP/identity/out") $(plan_call)"
fi
if run_identity --ebs-provider viperblockd && plan_call | grep -q -- '-var ebs_provider=viperblockd '; then
    pass "--ebs-provider viperblockd is passed through"
else
    fail "viperblockd plan: $(cat "$TMP/identity/out") $(plan_call)"
fi

echo "-- validate-topology.sh takes a retired topology name only with --destroy-only"
validate() {
    OCI_STATE_ROOT="$TMP/state" bash "$HERE/validate-topology.sh" \
        --ssh-public-key "$TMP/absent.pub" "$@" 2>&1
}
for name in vm-single-ocibsd vm-multi-ocibsd; do
    out=$(validate --topology "$name")
    case "$out" in
        *"accepted only with --destroy-only; run ${name%-ocibsd}"*) pass "$name alone is refused, naming ${name%-ocibsd}" ;;
        *) fail "$name alone: $out" ;;
    esac
    # Topology validation is what it must get past; the absent key stops it next.
    out=$(validate --topology "$name" --destroy-only)
    case "$out" in
        *"no readable SSH public key"*) pass "$name --destroy-only gets past topology validation" ;;
        *) fail "$name --destroy-only: $out" ;;
    esac
done
out=$(validate --topology vm-single --ebs-provider qemunbd)
case "$out" in
    *"--ebs-provider must be ocibsd or viperblockd"*) pass "an unknown --ebs-provider is refused" ;;
    *) fail "--ebs-provider qemunbd: $out" ;;
esac

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
