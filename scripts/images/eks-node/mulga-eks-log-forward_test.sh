#!/bin/sh
# Self-contained POSIX test for mulga-eks-log-forward.sh: the line filter,
# redaction, truncation, rate cap and blocked-console behaviour. The script
# reads the tailed log on stdin and writes to FORWARD_CONSOLE, which the test
# points at a temp file (or a reader-less FIFO), so no root or tty is needed.
#
# Run: sh scripts/images/eks-node/mulga-eks-log-forward_test.sh
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/mulga-eks-log-forward.sh"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT

PASS=0
FAIL=0
check() { # check <desc> <expected> <actual>
    if [ "$2" = "$3" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: $1"
        echo "  expected: [$2]"
        echo "  actual:   [$3]"
    fi
}

# forward <name>: feeds stdin to the script with a per-case console file and a
# fixed clock, leaving the forwarded text in ${OUT}.
forward() {
    CASE="${WORK}/$1"
    mkdir -p "${CASE}"
    OUT="${CASE}/console"
    : > "${OUT}"
    FORWARD_CONSOLE="${OUT}" FORWARD_PREFIX="[k3s-agent]" \
        FORWARD_CLOCK_CMD="echo 1000" sh "${SCRIPT}" 2>/dev/null || true
}
forwarded() { grep -c . "${OUT}" || true; }

# --- filter: lines that must pass ---
forward filter_pass <<'EOF'
time="2026-11-03T03:00:00Z" level=warning msg="slow response from apiserver"
time="2026-11-03T03:00:01Z" level=error msg="failed to renew"
W1103 03:00:02.123456    1234 reflector.go:561] watch of *v1.Pod ended
E1103 03:00:03.123456    1234 kubelet.go:2400] node not found
I1103 03:00:04.123456    1234 kubelet_node_status.go:73] Node status update failed
I1103 03:00:05.123456    1234 controller.go:193] Failed to update lease
I1103 03:00:06.123456    1234 kubelet.go:2200] PLEG is not healthy
time="2026-11-03T03:00:07Z" level=info msg="Event: NodeNotReady kubelet"
EOF
check "filter: all eight interesting lines pass" "8" "$(forwarded)"
check "filter: lines carry the prefix" "8" "$(grep -c '^\[k3s-agent\] ' "${OUT}" || true)"

# --- filter: lines that must be dropped ---
forward filter_drop <<'EOF'
time="2026-11-03T03:00:00Z" level=info msg="Starting k3s agent"
time="2026-11-03T03:00:01Z" level=debug msg="tick"
I1103 03:00:02.123456    1234 server.go:100] Version: v1.32.5
I1103 03:00:03.123456    1234 kubelet.go:1000] SyncLoop (UPDATE, "api")
EOF
check "filter: info and debug lines are dropped" "0" "$(forwarded)"

# --- redaction: one case per secret shape ---
redact() { # redact <name> <line> <secret-that-must-not-appear>
    forward "redact_$1" <<EOF
time="2026-11-03T03:00:00Z" level=error msg="$2"
EOF
    check "redact(${1}): line still forwarded" "1" "$(forwarded)"
    if grep -qF "$3" "${OUT}"; then
        check "redact(${1}): secret removed" "absent" "present"
    else
        check "redact(${1}): secret removed" "absent" "absent"
    fi
    check "redact(${1}): marker present" "1" "$(grep -c 'REDACTED' "${OUT}" || true)"
}
redact bearer "request failed Authorization: Bearer abc123SECRETvalue456" "abc123SECRETvalue456"
redact k3s_token "join failed token K1009f3c2d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2::server:s3cr3tPassw0rd" "s3cr3tPassw0rd"
redact jwt "bad token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXNlY3JldA" "eyJhbGciOiJIUzI1NiJ9"
redact akia "credentials AKIAIOSFODNN7EXAMPLE rejected" "AKIAIOSFODNN7EXAMPLE"
redact asia "credentials ASIAIOSFODNN7EXAMPLE rejected" "ASIAIOSFODNN7EXAMPLE"
redact amz_signature "GET /obj?X-Amz-Signature=0123456789abcdef0123456789abcdef&X-Amz-Expires=60 failed" "0123456789abcdef0123456789abcdef"
redact amz_credential "GET /obj?X-Amz-Credential=ZZZsecretscope%2F20261103%2Fap%2Fs3%2Faws4_request failed" "ZZZsecretscope"

# --- truncation: text over 512 bytes is cut ---
LONG=$(awk 'BEGIN { for (i = 0; i < 2000; i++) printf "a"; }')
forward truncate <<EOF
time="2026-11-03T03:00:00Z" level=error msg="${LONG}"
EOF
check "truncate: still forwarded" "1" "$(forwarded)"
BODY=$(sed 's/^\[k3s-agent\] //' "${OUT}" | head -n 1)
check "truncate: body is exactly 512 bytes" "512" "$(printf '%s' "${BODY}" | wc -c | tr -d ' ')"
check "truncate: keeps the start of the line" "time=\"2026-11-03T03:00:00Z\"" "$(printf '%s' "${BODY}" | cut -c1-27)"

# --- redaction runs before the cut: a secret over the limit leaves no prefix ---
# The pad puts the 512-byte cut right after the bare marker (AKIA, eyJ, K10).
# A cut fragment that long matches no redaction pattern, so redacting after the
# cut would leave it in the output.
HEAD='time="2026-11-03T03:00:00Z" level=error msg="'
straddle() { # straddle <name> <secret> <bytes of the secret kept by the cut>
    PAD=$(awk -v n=$((512 - ${#HEAD} - 1 - $3)) 'BEGIN { for (i = 0; i < n; i++) printf "x"; }')
    forward "straddle_$1" <<EOF
${HEAD}${PAD} $2 tail"
EOF
    check "straddle(${1}): line forwarded" "1" "$(forwarded)"
    check "straddle(${1}): cut keeps the marker length" "$3" "$(sed 's/^\[k3s-agent\] //' "${OUT}" | head -n 1 | awk -v p="${PAD} " '{ print length($0) - index($0, p) - length(p) + 1 }')"
    if grep -qF "$(printf '%s' "$2" | cut -c1-"$3")" "${OUT}"; then
        check "straddle(${1}): no fragment survives the cut" "absent" "present"
    else
        check "straddle(${1}): no fragment survives the cut" "absent" "absent"
    fi
}
straddle jwt "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2ln" 3
straddle akia "AKIAIOSFODNN7EXAMPLE" 4
straddle k3s_token "K1009f3c2d4e5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2::server:s3cr3tPassw0rd" 3

# --- rate cap: 60 a minute, then one summary of what was dropped ---
gen() { # gen <count>
    awk -v n="$1" 'BEGIN { for (i = 0; i < n; i++) printf "time=\"t\" level=error msg=\"loop %d\"\n", i }'
}
gen 100 > "${WORK}/burst"
forward rate_cap < "${WORK}/burst"
check "rate cap: 60 lines forwarded plus one summary" "61" "$(forwarded)"
check "rate cap: one dropped summary" "1" "$(grep -c '40 lines dropped' "${OUT}" || true)"
check "rate cap: first line kept" "1" "$(grep -c 'loop 0"' "${OUT}" || true)"
check "rate cap: line past the cap is not forwarded" "0" "$(grep -c 'loop 60"' "${OUT}" || true)"

# --- rate cap: a new minute starts a new window and reports the old drops ---
# The clock command advances 60 s every 70 calls, so lines 0-69 share a window
# (10 dropped) and lines 70-74 land in the next one.
CASE="${WORK}/rate_window"
mkdir -p "${CASE}"
OUT="${CASE}/console"
: > "${OUT}"
echo 0 > "${CASE}/calls"
cat > "${CASE}/clock" <<EOF
#!/bin/sh
n=\$(cat "${CASE}/calls")
echo \$((n + 1)) > "${CASE}/calls"
echo \$((1000 + n / 70 * 60))
EOF
chmod +x "${CASE}/clock"
gen 75 | FORWARD_CONSOLE="${OUT}" FORWARD_PREFIX="[k3s-agent]" FORWARD_CLOCK_CMD="${CASE}/clock" sh "${SCRIPT}" 2>/dev/null || true
check "window: 60 + 5 forwarded plus one summary" "66" "$(forwarded)"
check "window: summary names the 10 dropped" "1" "$(grep -c '10 lines dropped' "${OUT}" || true)"
check "window: new window forwards its lines" "1" "$(grep -c 'loop 74"' "${OUT}" || true)"

# --- blocked console: lines are dropped, the forwarder neither stalls nor fails ---
CASE="${WORK}/blocked"
mkdir -p "${CASE}"
mkfifo "${CASE}/console"
gen 3 > "${CASE}/in"
(
    FORWARD_CONSOLE="${CASE}/console" FORWARD_PREFIX="[k3s-agent]" FORWARD_CLOCK_CMD="echo 1000" \
        FORWARD_WRITE_TIMEOUT=1 sh "${SCRIPT}" < "${CASE}/in"
) &
PID=$!
i=0
while kill -0 "${PID}" 2>/dev/null && [ "${i}" -lt 20 ]; do
    sleep 1
    i=$((i + 1))
done
if kill -0 "${PID}" 2>/dev/null; then
    kill "${PID}" 2>/dev/null || true
    check "blocked console: forwarder finished" "finished" "stalled"
else
    check "blocked console: forwarder finished" "finished" "finished"
fi
rc=0
wait "${PID}" || rc=$?
check "blocked console: exit 0" "0" "${rc}"

# --- the service is wired in, and nothing depends on it ---
INITD="${SCRIPT_DIR}/mulga-eks-log-forward.initd"
check "initd ships" "yes" "$([ -f "${INITD}" ] && echo yes || echo no)"
check "manifest installs the initd" "1" "$(grep -c 'mulga-eks-log-forward.initd:/etc/init.d/mulga-eks-log-forward' "${SCRIPT_DIR}/manifest.conf" || true)"
check "manifest installs the script" "1" "$(grep -c 'mulga-eks-log-forward.sh:/usr/local/sbin/mulga-eks-log-forward' "${SCRIPT_DIR}/manifest.conf" || true)"
DEPS=0
for f in "${SCRIPT_DIR}"/*.initd; do
    [ "${f}" = "${INITD}" ] && continue
    if grep -E '^[[:space:]]*(need|use|want)[[:space:]].*mulga-eks-log-forward' "${f}" >/dev/null 2>&1; then
        DEPS=$((DEPS + 1))
    fi
done
check "nothing depends on the forwarder" "0" "${DEPS}"
if [ -f "${INITD}" ]; then
    check "initd does not need anything it could block boot on" "0" "$(grep -cE '^[[:space:]]*need[[:space:]]' "${INITD}" || true)"
fi

echo "mulga-eks-log-forward_test: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
