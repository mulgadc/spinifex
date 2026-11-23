#!/bin/bash
# reinstall_test.sh: validate-topology.sh against three stubbed hosts, with no
# tenancy. A stub ssh answers each remote call as a formed or fresh node would,
# and runs install scripts locally against a stub sudo that only logs.
set -u

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
trap 'pkill -f "tail -f /dev/null # reinstall_test" 2>/dev/null; rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

mkdir -p "$TMP/bin" "$TMP/remote"
# NODE_STATE is formed or fresh. HANG_PROBE makes the cloud-init probe never return.
cat > "$TMP/bin/ssh" <<'EOF'
#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in ubuntu@*) host="${1#ubuntu@}"; shift; break ;; *) shift ;; esac; done
cmd="$*"
printf '[%s] %s\n' "$host" "$cmd" >> "$T/ssh.log"
formed() { [ "$NODE_STATE" = formed ]; }
case "$cmd" in
    *"bash -s"*) PATH="$T/remote:$PATH" HOST="$host" bash -s >> "$T/remote.log" 2>&1; exit $? ;;
    *boot-finished*) [ -z "${HANG_PROBE:-}" ] || exec tail -f /dev/null # reinstall_test
                     exit 0 ;;
    *"^data_dir = "*) formed ;;
    *"command -v spx"*) formed ;;
    *"spx version"*) echo "spx v-test" ;;
    *ovn-remote*) formed && echo '"tcp:10.0.0.1:6642,tcp:10.0.0.2:6642,tcp:10.0.0.3:6642"' ;;
    *cluster/status*) formed && printf 'Status: cluster member\nServers:\n  a at tcp:10.0.0.1:6643\n  b at tcp:10.0.0.2:6643\n  c at tcp:10.0.0.3:6643\n' ;;
    *) exit 0 ;;
esac
EOF
cat > "$TMP/remote/sudo" <<'EOF'
#!/bin/bash
case "$1" in fuser|grep|pkill) exit 1 ;; esac
printf '[%s] sudo %s\n' "$HOST" "$*" >> "$T/sudo.log"
EOF
printf '#!/bin/bash\nexit 3\n' > "$TMP/remote/systemctl"
printf '#!/bin/bash\nexit 1\n' > "$TMP/remote/pgrep"
cat > "$TMP/remote/curl" <<'EOF'
#!/bin/bash
printf '[%s] curl %s\n' "$HOST" "$*" >> "$T/sudo.log"
EOF
printf '#!/bin/bash\nexit 0\n' > "$TMP/bin/sleep"
cp "$TMP/bin/sleep" "$TMP/remote/sleep"
cp "$TMP/bin/sleep" "$TMP/bin/scp"
chmod +x "$TMP/bin/"* "$TMP/remote/"*
printf '10.0.0.1\n10.0.0.2\n10.0.0.3\n' > "$TMP/hosts"
ssh-keygen -q -t ed25519 -N '' -f "$TMP/key"

# run <node state> <seed checkpoint 0|1> [validate-topology.sh options]
run() {
    local state="$1" checkpoint="$2"
    shift 2
    rm -rf "$TMP/state" "$TMP"/*.log
    mkdir -p "$TMP/state/.validate-vm-multi-existing"
    [ "$checkpoint" = 0 ] || : > "$TMP/state/.validate-vm-multi-existing/stage-formation.done"
    T="$TMP" NODE_STATE="$state" PATH="$TMP/bin:$PATH" OCI_STATE_ROOT="$TMP/state" \
        OCI_SSH_CALL_TIMEOUT=2 timeout 30 bash "$HERE/validate-topology.sh" \
        --topology vm-multi --existing-hosts-file "$TMP/hosts" --instance-principal \
        --ebs-provider viperblockd --no-external-pool \
        --ssh-public-key "$TMP/key.pub" --ssh-private-key "$TMP/key" "$@" > "$TMP/out" 2>&1
}
standalone_ovn_calls() { grep -c 'setup-ovn.sh --management' "$TMP/sudo.log" 2>/dev/null; }

echo "-- --reinstall on a formed three-node cluster keeps its clustered OVN configuration"
run formed 1 --reinstall
n=$(standalone_ovn_calls)
if [ "${n:-0}" = 0 ]; then
    pass "no node ran the standalone setup-ovn.sh"
else
    fail "$n node(s) ran setup-ovn.sh --management on a formed cluster"
fi

echo "-- a formed cluster opened from a new state directory is not formed again"
run formed 0
if grep -q 'forming the cluster' "$TMP/out"; then
    fail "the run started install-node.sh formation on a formed cluster"
else
    pass "formation did not start on a formed cluster"
fi

echo "-- a first install still runs the standalone setup-ovn.sh"
run fresh 0
n=$(standalone_ovn_calls)
if [ "${n:-0}" = 3 ]; then
    pass "each fresh node ran setup-ovn.sh --management"
else
    fail "fresh install ran setup-ovn.sh --management on ${n:-0} of 3 nodes: $(tail -3 "$TMP/out")"
fi

echo "-- a cloud-init probe that never returns does not hold the run"
HANG_PROBE=1 run formed 1
rc=$?
if [ "$rc" = 124 ]; then
    fail "the run was still waiting on cloud-init after 30 s"
elif grep -q 'cloud-init' "$TMP/out" && [ "$rc" != 0 ]; then
    pass "the run stopped with a cloud-init error"
else
    fail "unexpected result $rc: $(tail -3 "$TMP/out")"
fi

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
