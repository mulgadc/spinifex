#!/bin/bash
# reinstall_test.sh: validate-topology.sh against stubbed hosts, with no tenancy.
# A stub ssh runs the formation probe against a fixture config per host, and runs
# install scripts locally against a stub sudo that only logs.
# The suites run under sh, and this one needs bash.
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
set -u

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
export HANG_TAG="reinstall_test_hang_$$"
trap 'pkill -f "$HANG_TAG" 2>/dev/null; rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

mkdir -p "$TMP/bin" "$TMP/remote"
# NODE_STATE is formed or fresh. HANG_PROBE makes the cloud-init probe never return,
# and NO_MARKER makes it answer at once that boot-finished is absent.
cat > "$TMP/bin/ssh" <<'EOF'
#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in ubuntu@*) host="${1#ubuntu@}"; shift; break ;; *) shift ;; esac; done
cmd="$*"
printf '[%s] %s\n' "$host" "$cmd" >> "$T/ssh.log"
formed() { [ "$NODE_STATE" = formed ]; }
case "$cmd" in
    "sudo sh -s") sed "s#/etc/spinifex/spinifex.toml#$T/toml/$host#" | sh -s; exit $? ;;
    *"bash -s"*) export PATH="$T/remote:$PATH" HOST="$host"; eval "$cmd" >> "$T/remote.log" 2>&1; exit $? ;;
    *boot-finished*) [ -z "${HANG_PROBE:-}" ] || exec -a "$HANG_TAG" tail -f /dev/null
                     [ -z "${NO_MARKER:-}" ] || exit 1
                     exit 0 ;;
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
ssh-keygen -q -t ed25519 -N '' -f "$TMP/key"
# After the key exists: formation's host-key removal must not reach the caller's known_hosts.
cp "$TMP/bin/sleep" "$TMP/bin/ssh-keygen"
chmod +x "$TMP/bin/"* "$TMP/remote/"*
printf '10.0.0.1\n10.0.0.2\n10.0.0.3\n' > "$TMP/hosts-vm-multi"
printf '10.0.0.1\n' > "$TMP/hosts-vm-single"

# run <topology> <node state> <seed checkpoint 0|1> [validate-topology.sh options]
# Each host's config is formed (has data_dir) or absent. FRESH_HOST gets a config
# with only the credential seed, and FAIL_HOST a config that cannot be read.
# Sets RC, ELAPSED and RESULTS.
run() {
    local topo="$1" state="$2" checkpoint="$3" host dir
    shift 3
    rm -rf "$TMP/state" "$TMP/toml" "$TMP"/*.log
    dir="$TMP/state/.validate-$topo-existing"
    mkdir -p "$dir" "$TMP/toml"
    [ "$checkpoint" = 0 ] || : > "$dir/stage-formation.done"
    while read -r host; do
        if [ "$host" = "${FAIL_HOST:-}" ]; then
            mkdir "$TMP/toml/$host"
        elif [ "$host" = "${FRESH_HOST:-}" ]; then
            printf '[nodes.node3.ebs]\nprovider = "ocibsd"\n' > "$TMP/toml/$host"
        elif [ "$state" = formed ]; then
            printf 'data_dir = "/var/lib/spinifex"\n' > "$TMP/toml/$host"
        fi
    done < "$TMP/hosts-$topo"
    local start=$SECONDS
    T="$TMP" NODE_STATE="$state" PATH="$TMP/bin:$PATH" OCI_STATE_ROOT="$TMP/state" \
        OCI_CLOUD_INIT_TIMEOUT=3 timeout 30 bash "$HERE/validate-topology.sh" \
        --topology "$topo" --existing-hosts-file "$TMP/hosts-$topo" --instance-principal \
        --ebs-provider viperblockd --no-external-pool \
        --ssh-public-key "$TMP/key.pub" --ssh-private-key "$TMP/key" "$@" > "$TMP/out" 2>&1
    RC=$?
    ELAPSED=$((SECONDS - start))
    RESULTS="$dir/results.txt"
}
count() { grep -c -- "$1" "$2" 2>/dev/null; }
# The formed path reached the gate after formation: install and formation are behind it.
past_formation() { grep -q '^ebs provider choice: SKIP' "$RESULTS" 2>/dev/null; }

echo "-- --reinstall on a formed three-node cluster keeps its clustered OVN configuration"
run vm-multi formed 1 --reinstall
n=$(count 'setup-ovn.sh --management' "$TMP/sudo.log")
kept=$(count 'keeping the clustered OVN configuration' "$TMP/remote.log")
if [ "${n:-0}" != 0 ]; then
    fail "$n node(s) ran setup-ovn.sh --management on a formed cluster"
elif [ "${kept:-0}" != 3 ] || ! past_formation; then
    fail "the reinstall did not run on all 3 nodes and reach the gate after formation: $(grep -m1 ERROR "$TMP/out")"
else
    pass "each node reinstalled and kept its OVN configuration"
fi

echo "-- a formed cluster opened from a new state directory is not formed again"
run vm-multi formed 0
if grep -q 'forming the cluster' "$TMP/out"; then
    fail "the run started install-node.sh formation on a formed cluster"
elif ! past_formation; then
    fail "the run did not reach the gate after formation: $(grep -m1 ERROR "$TMP/out")"
else
    pass "formation did not start on a formed cluster"
fi

echo "-- --reinstall on a formed single node keeps its OVN configuration and does not check init"
run vm-single formed 0 --reinstall
n=$(count 'setup-ovn.sh --management' "$TMP/sudo.log")
if [ "${n:-0}" != 0 ]; then
    fail "the formed node ran setup-ovn.sh --management"
elif ! past_formation; then
    fail "the run did not reach the gate after formation: $(grep -m1 ERROR "$TMP/out")"
else
    pass "the node reinstalled and kept its OVN configuration"
fi

echo "-- a first install still runs the standalone setup-ovn.sh"
run vm-multi fresh 0
n=$(count 'setup-ovn.sh --management' "$TMP/sudo.log")
if [ "${n:-0}" != 3 ]; then
    fail "fresh install ran setup-ovn.sh --management on ${n:-0} of 3 nodes: $(tail -3 "$TMP/out")"
elif ! grep -q 'forming the cluster' "$TMP/out"; then
    fail "the fresh cluster did not go to formation"
else
    pass "each fresh node ran setup-ovn.sh --management, then formation started"
fi

# stopped_at_gate <case>: the run stopped with the gate's error within its limit.
stopped_at_gate() {
    if [ "$RC" = 124 ]; then
        fail "$1: the run was still waiting on cloud-init after 30 s"
    elif ! grep -q 'cloud-init did not finish on 10.0.0.1 within 3 s' "$TMP/out"; then
        fail "$1: no cloud-init limit error: $(grep -m1 -E 'installing|ERROR' "$TMP/out")"
    elif [ "$ELAPSED" -gt 15 ]; then
        fail "$1: the run took ${ELAPSED} s for a 3 s limit"
    elif grep -q 'installing Spinifex' "$TMP/out"; then
        fail "$1: the run went on to the installer"
    else
        pass "$1: the run stopped at the gate in ${ELAPSED} s"
    fi
}

echo "-- a cloud-init probe that never returns stops the gate at its elapsed limit"
HANG_PROBE=1 run vm-multi formed 1
stopped_at_gate "probe never returns"

echo "-- a boot-finished marker that never appears stops the gate at its elapsed limit"
NO_MARKER=1 run vm-multi fresh 0
stopped_at_gate "marker never appears"

# stopped_unchanged <expected message>: the run failed with the message and no
# install or formation step reached a host.
stopped_unchanged() {
    local msg="$1"
    if [ "$RC" = 0 ] || ! grep -q "$msg" "$TMP/out"; then
        fail "the run did not stop with \"$msg\": $(grep -m1 ERROR "$TMP/out")"
    elif [ -s "$TMP/sudo.log" ] || grep -q -e 'installing Spinifex' -e 'forming the cluster' "$TMP/out"; then
        fail "the run changed a host before it stopped"
    else
        pass "the run stopped before it changed a host"
    fi
}

echo "-- a host whose config cannot be read stops the run"
FAIL_HOST=10.0.0.2 run vm-multi formed 1 --reinstall
stopped_unchanged "formation state of 10.0.0.2"

echo "-- hosts that disagree on formation stop the run"
FRESH_HOST=10.0.0.3 run vm-multi formed 0
stopped_unchanged "the hosts disagree"

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
