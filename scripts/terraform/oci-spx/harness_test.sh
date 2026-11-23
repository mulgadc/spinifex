#!/bin/sh
# harness_test.sh — the OCI harness's defaults without a tenancy:
# setup-identity.sh grants ocibsd unless told otherwise, and validate-topology.sh
# accepts the retired -ocibsd topology names only to destroy their state.
set -u

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
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

echo "-- validate-topology.sh marks a state directory disposable only for a run it tears down"
# A copy beside a stub oci_env.py that records each Terraform call and exits STUB_RC.
mkdir -p "$TMP/vt/scripts" "$TMP/vtstate"
cp "$HERE/validate-topology.sh" "$TMP/vt/"
printf '%s\n' 'import os, sys' 'open(sys.argv[0] + ".calls", "a").write(" ".join(sys.argv[1:]) + "\n")' \
    'sys.exit(int(os.environ.get("STUB_RC", "0")))' > "$TMP/vt/scripts/oci_env.py"
printf '%s\n' '#!/bin/sh' 'exit 0' > "$TMP/hook"
chmod +x "$TMP/hook"
touch "$TMP/key" "$TMP/key.pub" "$TMP/existing.tfstate"
vtdir="$TMP/vtstate/.validate-vm-single"
marker="$vtdir/disposable"
tf_calls="$TMP/vt/scripts/oci_env.py.calls"
# With no credential hook a plain run stops before any cloud call.
run_vt() {
    : > "$tf_calls"
    env -u OCI_CREDENTIAL_HOOK OCI_STATE_ROOT="$TMP/vtstate" bash "$TMP/vt/validate-topology.sh" \
        --topology vm-single --ebs-provider viperblockd \
        --ssh-public-key "$TMP/key.pub" --ssh-private-key "$TMP/key" "$@" > "$TMP/vt/out" 2>&1
}
reset_vt() { rm -rf "$vtdir"; mkdir -p "$vtdir"; }
instances='{"resources": [{"instances": [{}]}]}'

reset_vt; run_vt
if [ -e "$marker" ]; then pass "a plain run is disposable"; else fail "plain run: $(cat "$TMP/vt/out")"; fi
for owned in tfstate record unreadable empty shape; do
    reset_vt
    case "$owned" in
        tfstate) printf '%s\n' "$instances" > "$vtdir/terraform.tfstate" ;;
        record) printf 'c\tocid1.compartment\tus-test-1\n' > "$vtdir/ocibsd-clusters" ;;
        unreadable) echo '{' > "$vtdir/terraform.tfstate" ;;
        empty) : > "$vtdir/terraform.tfstate" ;;
        shape) echo '{"resources": [{}]}' > "$vtdir/terraform.tfstate" ;;
    esac
    run_vt
    if [ "$?" != 0 ] && [ ! -e "$marker" ] && [ ! -s "$tf_calls" ] && grep -q "does not own" "$TMP/vt/out"; then
        pass "a plain run refuses an unmarked directory with a $owned"
    else
        fail "unmarked $owned: $(cat "$TMP/vt/out") $(cat "$tf_calls")"
    fi
done
reset_vt; printf '%s\n' "$instances" > "$vtdir/terraform.tfstate"; touch "$marker"; run_vt
if grep -q "credential hook is set" "$TMP/vt/out"; then
    pass "a plain run takes over a killed disposable run"
else
    fail "marked leftover: $(cat "$TMP/vt/out")"
fi
reset_vt; touch "$marker"; run_vt --keep
if [ ! -e "$marker" ]; then pass "--keep removes an earlier run's marker"; else fail "--keep left the marker"; fi
existing_marker="$TMP/vtstate/.validate-vm-single-existing/disposable"
mkdir -p "${existing_marker%/*}"; touch "$existing_marker"; run_vt --existing-state-file "$TMP/existing.tfstate"
if [ ! -e "$existing_marker" ]; then pass "existing infrastructure is never disposable"; else fail "--existing-state-file left the marker"; fi
reset_vt; run_vt --dry-run
if [ ! -e "$marker" ]; then pass "--dry-run does not mark a directory"; else fail "--dry-run wrote the marker"; fi
reset_vt; touch "$marker"; run_vt --dry-run --keep
if [ -e "$marker" ]; then pass "--dry-run --keep leaves the marker"; else fail "--dry-run --keep removed the marker"; fi
touch "$existing_marker"; run_vt --dry-run --existing-state-file "$TMP/existing.tfstate"
if [ -e "$existing_marker" ]; then pass "--dry-run with existing infrastructure leaves the marker"; else fail "--dry-run --existing-state-file removed the marker"; fi
reset_vt; printf '%s\n' "$instances" > "$vtdir/terraform.tfstate"; run_vt --dry-run --destroy-only
if [ "$?" != 0 ] && [ ! -s "$tf_calls" ] && [ ! -e "$marker" ]; then
    pass "--dry-run --destroy-only is refused"
else
    fail "--dry-run --destroy-only: $(cat "$TMP/vt/out") $(cat "$tf_calls")"
fi
reset_vt; STUB_RC=1 run_vt --destroy-only
if [ ! -e "$marker" ]; then pass "a failed --destroy-only leaves the directory unmarked"; else fail "a failed destroy wrote the marker"; fi
reset_vt; run_vt --destroy-only
if [ -e "$marker" ]; then pass "a destroyed deployment is handed to the sweep"; else fail "--destroy-only: $(cat "$TMP/vt/out")"; fi
reset_vt; STUB_RC=1 run_vt --keep-on-fail --credential-hook "$TMP/hook"
if [ ! -e "$marker" ] && grep -q "keep-on-fail" "$TMP/vt/out"; then
    pass "a failure left up by --keep-on-fail is not swept"
else
    fail "--keep-on-fail: $(cat "$TMP/vt/out")"
fi

echo "-- e2e-oci.sh sweeps only directories marked disposable"
# Stubs record what would have reached OCI: a destroy, or a volume sweep by cluster.
mkdir -p "$TMP/e2e/scripts" "$TMP/root"
cp "$HERE/e2e-oci.sh" "$TMP/e2e/"
printf '%s\n' 'import sys' 'open(sys.argv[0] + ".calls", "a").write(" ".join(sys.argv[1:]) + "\n")' \
    > "$TMP/e2e/scripts/oci_env.py"
printf '%s\n' '#!/bin/sh' 'echo "$*" >> "$0.calls"' > "$TMP/e2e/validate-topology.sh"
chmod +x "$TMP/e2e/validate-topology.sh"
: > "$TMP/e2e/scripts/oci_env.py.calls"
: > "$TMP/e2e/validate-topology.sh.calls"
state_dir() { # name, cluster, disposable, instances
    mkdir -p "$TMP/root/.validate-$1"
    printf '%s\tocid1.compartment\tus-test-1\n' "$2" > "$TMP/root/.validate-$1/ocibsd-clusters"
    [ "$3" = 1 ] && touch "$TMP/root/.validate-$1/disposable"
    [ "$4" = 1 ] && printf '%s\n' '{"resources": [{"instances": [{}]}]}' > "$TMP/root/.validate-$1/terraform.tfstate"
    return 0
}
state_dir vm-single killed 1 1
state_dir vm-multi destroyed 1 0
state_dir bm kept 0 1
state_dir bm-existing existing 0 0
state_dir vm-single-principal unreadable 1 0
echo '{' > "$TMP/root/.validate-vm-single-principal/terraform.tfstate"
state_dir vm-multi-ocibsd truncated 1 0
: > "$TMP/root/.validate-vm-multi-ocibsd/terraform.tfstate"
state_dir vm-single-ocibsd shape 1 0
echo '{"resources": [{}]}' > "$TMP/root/.validate-vm-single-ocibsd/terraform.tfstate"
SPX_REF=dev OCI_STATE_ROOT="$TMP/root" OCI_ARTIFACT_DIR="$TMP/art" \
    bash "$TMP/e2e/e2e-oci.sh" --sweep-only > "$TMP/e2e/out" 2>&1
destroys=$(cat "$TMP/e2e/validate-topology.sh.calls")
swept=$(grep -o -- '--cluster [a-z]*' "$TMP/e2e/scripts/oci_env.py.calls" | sort | tr '\n' ' ')
if [ "$destroys" = "--topology vm-single --destroy-only" ]; then
    pass "only the killed disposable run is destroyed"
else
    fail "destroys: '$destroys'; $(cat "$TMP/e2e/out")"
fi
if [ "$swept" = "--cluster destroyed --cluster killed " ]; then
    pass "only disposable clusters have their volumes swept"
else
    fail "volume sweeps: '$swept'; $(cat "$TMP/e2e/out")"
fi
for odd in vm-single-principal vm-multi-ocibsd vm-single-ocibsd; do
    if grep -q "sweep $odd: FAIL" "$TMP/art/verdict.txt" && [ -s "$TMP/root/.validate-$odd/ocibsd-clusters" ]; then
        pass "$odd: state that cannot be read fails the sweep and keeps its record"
    else
        fail "$odd state: $(cat "$TMP/art/verdict.txt")"
    fi
done
state_dir vm-single killed 1 1
: > "$TMP/e2e/scripts/oci_env.py.calls"
: > "$TMP/e2e/validate-topology.sh.calls"
SPX_REF=dev OCI_STATE_ROOT="$TMP/root" OCI_ARTIFACT_DIR="$TMP/art-dry" OCI_SOURCE=release \
    bash "$TMP/e2e/e2e-oci.sh" --dry-run > "$TMP/e2e/out" 2>&1
if [ ! -s "$TMP/e2e/validate-topology.sh.calls" ] || ! grep -q destroy-only "$TMP/e2e/validate-topology.sh.calls"; then
    if [ ! -s "$TMP/e2e/scripts/oci_env.py.calls" ]; then
        pass "--dry-run destroys and sweeps nothing"
    else
        fail "--dry-run swept: $(cat "$TMP/e2e/scripts/oci_env.py.calls")"
    fi
else
    fail "--dry-run destroyed: $(cat "$TMP/e2e/validate-topology.sh.calls")"
fi

echo "-- e2e-oci.sh with the real validator leaves what it does not own"
# Real e2e-oci.sh and validate-topology.sh beside a stub oci_env.py. Its apply
# writes Terraform instances and fails, as a build that half-completed would.
mkdir -p "$TMP/int/scripts"
cp "$HERE/e2e-oci.sh" "$HERE/validate-topology.sh" "$TMP/int/"
cat > "$TMP/int/scripts/oci_env.py" <<'PY'
import sys
args = sys.argv[1:]
open(sys.argv[0] + ".calls", "a").write(" ".join(args) + "\n")
if "apply" in args:
    open(args[args.index("-state") + 1], "w").write('{"resources": [{"instances": [{}]}]}\n')
    sys.exit(1)
PY
int_calls="$TMP/int/scripts/oci_env.py.calls"
run_int() { # state root, then environment assignments
    root="$1"; shift
    : > "$int_calls"
    env -u OCI_CREDENTIAL_HOOK "$@" OCI_STATE_ROOT="$root" OCI_ARTIFACT_DIR="$root.art" \
        OCI_TOPOLOGIES=vm-single OCI_EBS_PROVIDER=viperblockd OCI_SOURCE=release SPX_REF=dev \
        OCI_SSH_PUBLIC_KEY="$TMP/key.pub" OCI_SSH_PRIVATE_KEY="$TMP/key" \
        bash "$TMP/int/e2e-oci.sh" > "$root.out" 2>&1
}
kept="$TMP/kept/.validate-vm-single"
mkdir -p "$kept"
printf '%s\n' "$instances" > "$kept/terraform.tfstate"
printf 'kept\tocid1.compartment\tus-test-1\n' > "$kept/ocibsd-clusters"
cp "$kept/terraform.tfstate" "$TMP/kept.tfstate"
cp "$kept/ocibsd-clusters" "$TMP/kept.clusters"
run_int "$TMP/kept"
if [ ! -s "$int_calls" ] && [ ! -e "$kept/disposable" ] && cmp -s "$kept/terraform.tfstate" "$TMP/kept.tfstate" \
    && cmp -s "$kept/ocibsd-clusters" "$TMP/kept.clusters"; then
    pass "a full run over a kept deployment applies, destroys and sweeps nothing"
else
    fail "kept deployment: calls '$(cat "$int_calls")'; $(tail -20 "$TMP/kept.out")"
fi
mkdir -p "$TMP/kof"
run_int "$TMP/kof" OCI_KEEP_ON_FAIL=1
if grep -q ' apply ' "$int_calls" && ! grep -q ' destroy ' "$int_calls" \
    && ! grep -q 'sweep: vm-single still holds resources' "$TMP/kof.out" \
    && [ -s "$TMP/kof/.validate-vm-single/terraform.tfstate" ]; then
    pass "a failure left up by OCI_KEEP_ON_FAIL survives the same run's final sweep"
else
    fail "keep-on-fail: calls '$(cat "$int_calls")'; $(tail -20 "$TMP/kof.out")"
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
