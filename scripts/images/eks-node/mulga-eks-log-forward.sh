#!/bin/sh
# mulga-eks-log-forward — copy the warnings and errors of a k3s log to the
# serial console, where the host picks them up for the Elasticsearch sink.
#
# Reads the tailed log on stdin. Lines that matter are redacted first, then
# cut to FORWARD_MAX_BYTES, then rate limited: at most FORWARD_RATE_PER_MIN a
# minute reach the console and the rest are reported as one "N lines dropped"
# line. Every console write has a timeout, so a stuck console loses lines
# instead of stalling the tail or k3s.
#
# Env: FORWARD_CONSOLE (default /dev/console), FORWARD_PREFIX, FORWARD_MAX_BYTES
# (512), FORWARD_RATE_PER_MIN (60), FORWARD_WRITE_TIMEOUT seconds (2) and
# FORWARD_CLOCK_CMD (default: seconds of uptime, read without forking).

CONSOLE=${FORWARD_CONSOLE:-/dev/console}
PREFIX=${FORWARD_PREFIX:-[k3s]}
MAX_BYTES=${FORWARD_MAX_BYTES:-512}
RATE=${FORWARD_RATE_PER_MIN:-60}
WRITE_TIMEOUT=${FORWARD_WRITE_TIMEOUT:-2}
CLOCK_CMD=${FORWARD_CLOCK_CMD:-}

now() {
    if [ -n "${CLOCK_CMD}" ]; then
        sh -c "${CLOCK_CMD}"
    else
        read -r up _ < /proc/uptime && echo "${up%%.*}"
    fi
}

# emit TEXT: one console line, abandoned after WRITE_TIMEOUT. Returns non-zero
# when the console did not take it.
emit() {
    timeout "${WRITE_TIMEOUT}" sh -c 'printf "%s\n" "$1" >> "$2"' sh "$1" "${CONSOLE}" 2>/dev/null
}

# Filter, redact and truncate in one awk pass so lines that do not matter cost
# no fork. Redaction runs before the cut so a secret straddling the limit
# cannot leave a readable prefix behind.
select_lines() {
    LC_ALL=C awk -v max="${MAX_BYTES}" '
    {
        line = $0
        gsub(/\r/, "", line)
        keep = 0
        if (line ~ /level=(warning|error)/) keep = 1
        else if (line ~ /^[WE][0-9][0-9][0-9][0-9] /) keep = 1
        else if (line ~ /NodeNotReady|PLEG|[Ll]ease|[Nn]ode status/) keep = 1
        if (!keep) next

        gsub(/[Bb]earer +[A-Za-z0-9._~+\/=-]+/, "Bearer [REDACTED]", line)
        gsub(/K10[0-9a-f]+(::[A-Za-z0-9-]+:[^ "\047]+)?/, "[REDACTED]", line)
        gsub(/eyJ[A-Za-z0-9_.-]+/, "[REDACTED]", line)
        gsub(/(AKIA|ASIA)[A-Z0-9]+/, "[REDACTED]", line)
        gsub(/X-Amz-Signature=[^& "\047]*/, "X-Amz-Signature=[REDACTED]", line)
        gsub(/X-Amz-Credential=[^& "\047]*/, "X-Amz-Credential=[REDACTED]", line)
        gsub(/X-Amz-Security-Token=[^& "\047]*/, "X-Amz-Security-Token=[REDACTED]", line)
        gsub(/[^[:print:]\t]/, " ", line)
        print substr(line, 1, max)
    }'
}

win_start=0
sent=0
dropped=0

flush_dropped() {
    [ "${dropped}" -gt 0 ] || return 0
    emit "${PREFIX} ${dropped} lines dropped"
    dropped=0
}

select_lines | {
    while IFS= read -r line; do
        t=$(now) || t=0
        if [ "${win_start}" -eq 0 ] || [ $((t - win_start)) -ge 60 ]; then
            flush_dropped
            win_start=${t}
            sent=0
            [ "${win_start}" -eq 0 ] && win_start=1
        fi
        if [ "${sent}" -ge "${RATE}" ]; then
            dropped=$((dropped + 1))
            continue
        fi
        sent=$((sent + 1))
        emit "${PREFIX} ${line}" || dropped=$((dropped + 1))
    done
    flush_dropped
}
exit 0
