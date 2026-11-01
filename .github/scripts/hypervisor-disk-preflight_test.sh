#!/bin/sh
# hypervisor-disk-preflight_test.sh — drives hypervisor-disk-preflight.sh against
# a stub ssh that answers for each hypervisor from a canned file, so every
# verdict is checked without a real host.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PREFLIGHT="$SCRIPT_DIR/hypervisor-disk-preflight.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

# The stub answers "<root-kb> <pool-kb> <path>" from $FAKE/<host>, or exits 255
# when that host has no file, the way ssh does for a host it cannot reach.
mkdir -p "$TMP/bin" "$TMP/hosts"
cat > "$TMP/bin/ssh" <<'STUB'
#!/bin/sh
for a in "$@"; do case $a in *@*) host=${a#*@} ;; esac; done
cat >/dev/null
if [ -f "$FAKE/$host" ]; then cat "$FAKE/$host"; exit 0; fi
echo "ssh: Could not resolve hostname $host" >&2
exit 255
STUB
chmod +x "$TMP/bin/ssh"
PATH="$TMP/bin:$PATH"
FAKE="$TMP/hosts"
export PATH FAKE

cat > "$TMP/env.tfvars" <<'TFVARS'
nodes = [
  { name = "wattle", wan_ip = "192.0.2.1", storage_pool = "drive1-pool", base_image = "/x.qcow2" },
  # { name = "retired", wan_ip = "192.0.2.9", storage_pool = "drive1-pool", base_image = "/x.qcow2" },
  { name = "banksia", wan_ip = "192.0.2.2", storage_pool = "nvme1-pool", base_image = "/x.qcow2" },
]
TFVARS

GiB=1048576
plenty="$((50 * GiB)) $((200 * GiB)) /mnt/drive1/pool"

# run <label> <want-rc> <want-text>
run() {
    rc=0
    out=$(bash "$PREFLIGHT" "$TMP/env.tfvars" 2>&1) || rc=$?
    if [ "$rc" -ne "$2" ]; then
        fail "$1: exit $rc, want $2"
        echo "$out" | sed 's/^/        /'
        return
    fi
    case $out in
        *"$3"*) pass "$1" ;;
        *) fail "$1: output lacks '$3'"; echo "$out" | sed 's/^/        /' ;;
    esac
}

echo "-- hypervisor-disk-preflight.sh"

echo "$plenty" > "$FAKE/wattle"
echo "$plenty" > "$FAKE/banksia"
run "every host has room" 0 "banksia: root fs 51200 MiB free"

echo "0 $((200 * GiB)) /mnt/drive1/pool" > "$FAKE/wattle"
run "a full root filesystem fails fast" 1 "::error::hypervisor wattle has 0 MiB free on the filesystem holding /etc/libvirt/qemu"

echo "$plenty" > "$FAKE/wattle"
echo "$((50 * GiB)) $((3 * GiB)) /mnt/nvme1/pool" > "$FAKE/banksia"
run "a nearly full pool fails fast" 1 "::error::hypervisor banksia has 3 GiB free in storage pool nvme1-pool"

echo "$plenty" > "$FAKE/banksia"
rm -f "$FAKE/wattle"
run "an unreachable host is reported, not failed" 0 "::warning::hypervisor disk preflight: cannot reach wattle"

echo "$plenty" > "$FAKE/wattle"
echo "$((50 * GiB)) - -" > "$FAKE/banksia"
run "a pool libvirt does not know is reported, not failed" 0 "pool nvme1-pool (-) not found"

echo "0 0 /x" > "$FAKE/retired"
echo "$plenty" > "$FAKE/banksia"
run "a commented-out node is never checked" 0 "wattle: root fs"

if [ "$fails" -ne 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
echo "  hypervisor-disk-preflight ok"
