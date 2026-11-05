#!/bin/sh
# build-qboot_test.sh — a host ROM matching the recorded hash is installed
# without the container; any other host ROM, or none, builds in it, and a
# container ROM with the wrong hash still fails.
set -eu

SCRIPTS_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- build-qboot.sh host ROM"

COMMIT=$(sed -n 's/^QBOOT_COMMIT="\(.*\)"$/\1/p' "$SCRIPTS_DIR/build-qboot.sh")
printf 'pinned rom\n' > "$TMP/good.rom"
printf 'other rom\n' > "$TMP/other.rom"
GOOD_SHA=$(sha256sum "$TMP/good.rom" | cut -d' ' -f1)

# git and docker stubs. git fakes a checkout of the pinned commit; docker
# prints $STUB_ROM as the firmware and records that it ran.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/git" <<STUB
#!/bin/sh
case "\$1" in
init) mkdir -p "\$3"; printf 'GPL-2.0\n' > "\$3/LICENSE"; exit 0 ;;
esac
shift 2
case "\$1" in
rev-parse) echo "$COMMIT" ;;
archive) while [ "\$1" != "-o" ]; do shift; done; printf 'source\n' > "\$2" ;;
esac
STUB
cat > "$TMP/bin/docker" <<'STUB'
#!/bin/sh
[ "$1" = info ] && exit 0
echo ran >> "$DOCKER_LOG"
cat "$STUB_ROM"
STUB
chmod +x "$TMP/bin/git" "$TMP/bin/docker"

# run <name> <host-rom> <container-rom>: build in a fresh fixture repo.
run() {
    repo="$TMP/$1"
    mkdir -p "$repo/scripts"
    sed "s/^QBOOT_SHA256=.*/QBOOT_SHA256=\"$GOOD_SHA\"/" "$SCRIPTS_DIR/build-qboot.sh" > "$repo/scripts/build-qboot.sh"
    : > "$TMP/$1.docker"
    PATH="$TMP/bin:$PATH" QBOOT_HOST_ROM="$2" STUB_ROM="$3" DOCKER_LOG="$TMP/$1.docker" \
        bash "$repo/scripts/build-qboot.sh" > "$TMP/$1.log" 2>&1
}

# outputs <name>: the ROM is the pinned one and the licence and source exist.
outputs() {
    cmp -s "$TMP/$1/build/microvm/qboot.rom" "$TMP/good.rom" &&
        [ -s "$TMP/$1/build/microvm/qboot.LICENSE" ] &&
        [ -s "$TMP/$1/build/qboot/qboot-$COMMIT.tar.gz" ]
}

if run match "$TMP/good.rom" "$TMP/other.rom" && outputs match && [ ! -s "$TMP/match.docker" ]; then
    pass "matching host ROM installed, container not run"
else
    fail "matching host ROM: $(tr '\n' ' ' < "$TMP/match.log")"
fi

if run mismatch "$TMP/other.rom" "$TMP/good.rom" && outputs mismatch && [ -s "$TMP/mismatch.docker" ] &&
    grep -q 'has sha256' "$TMP/mismatch.log"; then
    pass "mismatched host ROM builds in the container"
else
    fail "mismatched host ROM: $(tr '\n' ' ' < "$TMP/mismatch.log")"
fi

if run missing "$TMP/absent.rom" "$TMP/good.rom" && outputs missing && [ -s "$TMP/missing.docker" ]; then
    pass "missing host ROM builds in the container"
else
    fail "missing host ROM: $(tr '\n' ' ' < "$TMP/missing.log")"
fi

if run badbuild "$TMP/absent.rom" "$TMP/other.rom"; then
    fail "container ROM with the wrong hash was accepted"
elif [ -e "$TMP/badbuild/build/microvm/qboot.rom" ]; then
    fail "container ROM with the wrong hash was installed"
else
    pass "container ROM with the wrong hash fails and installs nothing"
fi

[ "$fails" -eq 0 ] || { echo "$fails failure(s)"; exit 1; }
