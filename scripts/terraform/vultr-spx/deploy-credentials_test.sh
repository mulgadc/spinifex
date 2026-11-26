#!/usr/bin/env bash
# Checks that write_profile in deploy-vultr.sh never puts the secret key in a command's arguments,
# keeps the other profiles in the credentials file, and leaves that file readable by its owner only.
# preflight runs this with sh
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
set -euo pipefail
exec </dev/null
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/home"
real_python="$(command -v python3)"

SECRET=FAKESECRETKEY42
for c in aws python3; do
    cat > "$work/bin/$c" <<SH
#!/bin/sh
echo "$c \$*" >> "\$FAKE_ARGV"
[ "$c" = python3 ] && exec "$real_python" "\$@"
exit 0
SH
    chmod +x "$work/bin/$c"
done
export FAKE_ARGV="$work/argv" PATH="$work/bin:$PATH" HOME="$work/home"
: > "$FAKE_ARGV"

# Load only the functions under test: the script runs a command when it is sourced.
eval "$(sed -n '/^write_credentials()/,/^}/p; /^write_profile()/,/^}/p' "$here/deploy-vultr.sh")"
log() { :; }
die() { echo "die: $*" >&2; exit 1; }
on() {
    case "$1" in
        *ca.pem*) echo CA ;;
        *) printf 'aws_access_key_id = AK1\naws_secret_access_key = %s\n' "$SECRET" ;;
    esac
}
# shellcheck disable=SC2034 # read by the loaded functions
PROFILE=spinifex-vultr-t1 IP=192.0.2.10 SPX_REGION=us-east-1 CA_FILE="$work/ca.pem"

mkdir -m 700 "$HOME/.aws"
printf '[other]\naws_access_key_id = X\naws_secret_access_key = Y\n\n[spinifex-vultr-t1]\naws_access_key_id = OLD\naws_session_token = T\n' > "$HOME/.aws/credentials"
write_profile

fails=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fails=$((fails + 1)); fi; }
creds="$HOME/.aws/credentials"
check "the secret is in no argument" "$(grep -c "$SECRET" "$FAKE_ARGV" || true)" 0
check "the secret is in the credentials file" "$(grep -c "^aws_secret_access_key = $SECRET\$" "$creds")" 1
check "the key replaces the old one" "$(grep -c '^aws_access_key_id = AK1$' "$creds")" 1
check "the old session token is gone" "$(grep -c aws_session_token "$creds" || true)" 0
check "other profiles are kept" "$(sed -n '/^\[other\]/,/^$/p' "$creds" | tr '\n' ' ')" "[other] aws_access_key_id = X aws_secret_access_key = Y  "
check "the credentials file is 0600" "$(stat -c %a "$creds")" 600
check "the other settings go through aws configure set" "$(grep -c '^aws configure set --profile spinifex-vultr-t1 ' "$FAKE_ARGV")" 3
[ "$fails" = 0 ] || exit 1
