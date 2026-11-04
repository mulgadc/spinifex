#!/bin/sh
# lambda-testlink-build_test.sh — the test-link make targets refuse a malformed
# image digest and an unsupported host or GOARCH before touching any output.
#
# Every case is refused on the recipe's first lines, so nothing is compiled.
set -eu

LIB_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(dirname -- "$(dirname -- "$LIB_DIR")")
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- make build-lambda-testlink / build-lambda-testlink-image guards"

PWN="$TMP/pwn"
OUT="$TMP/out"
HEX=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
mkdir -p "$OUT"
printf 'old daemon\n' > "$OUT/spx"
printf 'old initializer\n' > "$OUT/hardkill.so"
PAIR=$(cat "$OUT/spx" "$OUT/hardkill.so" | cksum)

# unchanged <name> — the existing pair is intact, no scratch was made and no
# injected command ran.
unchanged() {
    if [ -e "$PWN" ]; then
        fail "$1: injected command ran"
        rm -f "$PWN"
    elif [ -e "$OUT.tmp" ]; then
        fail "$1: scratch directory created"
    elif [ "$(cat "$OUT/spx" "$OUT/hardkill.so" | cksum)" != "$PAIR" ]; then
        fail "$1: existing pair replaced"
    else
        pass "$1"
    fi
}

# refused <name> <want> <log> <status> — make failed with the expected message.
refused() {
    if [ "$4" -eq 0 ]; then
        fail "$1: make succeeded"
    elif ! grep -q "$2" "$3"; then
        fail "$1: wrong refusal: $(tr '\n' ' ' < "$3")"
    else
        unchanged "$1"
    fi
}

testlink() {
    make -s -C "$REPO_ROOT" --no-print-directory build-lambda-testlink LAMBDA_TESTLINK_DIR="$OUT" "$@" > "$TMP/log" 2>&1
}

DIGEST_MSG="LAMBDA_IMAGE_DIGEST must be 64 lowercase hex digits"
NL='
'
for v in '' "\$(shell touch $PWN)" "\$\$(touch $PWN)" "\`touch $PWN\`" "$HEX; touch $PWN" \
    "$HEX\" -X main.x=\"y" "$HEX -X main.x=y" "$HEX${NL}touch $PWN" \
    0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF "${HEX%?}" "${HEX}0"; do
    # GNU make itself expands a command-line $(shell ...) while parsing, for
    # any target, so that one literal is tested only from the environment.
    case "$v" in
    "\$(shell"*) ;;
    *)
        status=0
        testlink LAMBDA_IMAGE_DIGEST="$v" || status=$?
        refused "command-line digest [$v]" "$DIGEST_MSG" "$TMP/log" "$status"
        ;;
    esac
    status=0
    LAMBDA_IMAGE_DIGEST="$v" testlink || status=$?
    refused "environment digest [$v]" "$DIGEST_MSG" "$TMP/log" "$status"
done

status=0
env -u LAMBDA_IMAGE_DIGEST make -s -C "$REPO_ROOT" --no-print-directory build-lambda-testlink LAMBDA_TESTLINK_DIR="$OUT" > "$TMP/log" 2>&1 || status=$?
refused "digest unset" "$DIGEST_MSG" "$TMP/log" "$status"

ARCH_MSG="supports only an x86_64 host building linux/amd64"
SHIM="$TMP/shim"
mkdir -p "$SHIM"
printf '#!/bin/sh\necho aarch64\n' > "$SHIM/uname"
chmod +x "$SHIM/uname"

status=0
GOARCH=arm64 testlink LAMBDA_IMAGE_DIGEST="$HEX" || status=$?
refused "GOARCH=arm64" "$ARCH_MSG" "$TMP/log" "$status"
status=0
PATH="$SHIM:$PATH" testlink LAMBDA_IMAGE_DIGEST="$HEX" || status=$?
refused "aarch64 host" "$ARCH_MSG" "$TMP/log" "$status"

# The image target refuses before building qboot or any agent binary.
for c in "GOARCH=arm64" "PATH=$SHIM:$PATH"; do
    status=0
    env "$c" make -s -C "$REPO_ROOT" --no-print-directory build-lambda-testlink-image > "$TMP/log" 2>&1 || status=$?
    if [ "$status" -eq 0 ]; then
        fail "image target [${c%%=*}]: make succeeded"
    elif ! grep -q "$ARCH_MSG" "$TMP/log" || grep -q "qboot\|go build" "$TMP/log"; then
        fail "image target [${c%%=*}]: did not refuse first: $(tr '\n' ' ' < "$TMP/log")"
    else
        pass "image target [${c%%=*}] refused first"
    fi
done

if [ "$fails" -ne 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
