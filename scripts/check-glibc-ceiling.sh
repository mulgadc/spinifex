#!/bin/bash
# check-glibc-ceiling.sh — fail when an ELF file needs a glibc symbol version
# newer than a ceiling, so the one release tarball keeps loading on the oldest
# glibc of every OS it ships to.
#
# Usage: scripts/check-glibc-ceiling.sh <ceiling, e.g. 2.34> <elf file>...
set -euo pipefail

if [ $# -lt 2 ]; then
    echo "usage: $0 <ceiling> <elf file>..." >&2
    exit 2
fi
ceiling=$1
shift

failed=0
for f in "$@"; do
    if ! syms=$(objdump -T "$f" 2>&1); then
        case "$syms" in
            *"not a dynamic object"*)
                echo "$f: statically linked, needs no glibc"
                continue
                ;;
        esac
        echo "ERROR: cannot read the dynamic symbols of $f: $syms" >&2
        failed=1
        continue
    fi
    need=$(grep -o 'GLIBC_[0-9][0-9.]*' <<<"$syms" | sed 's/^GLIBC_//' | sort -uV | tail -1 || true)
    if [ -z "$need" ]; then
        echo "$f: needs no versioned glibc symbol"
        continue
    fi
    if [ "$(printf '%s\n%s\n' "$need" "$ceiling" | sort -V | tail -1)" = "$ceiling" ]; then
        echo "$f: needs at most GLIBC_$need (ceiling GLIBC_$ceiling)"
        continue
    fi
    echo "ERROR: $f needs GLIBC_$need, newer than the GLIBC_$ceiling ceiling. Symbols above it:" >&2
    while read -r ver sym; do
        if [ "$(printf '%s\n%s\n' "$ver" "$ceiling" | sort -V | tail -1)" != "$ceiling" ]; then
            echo "  $sym@GLIBC_$ver" >&2
        fi
    done < <(awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^\(?GLIBC_[0-9]/) { v = $i; gsub(/[()]|GLIBC_/, "", v); print v, $NF } }' <<<"$syms" | sort -u)
    failed=1
done
exit "$failed"
