#!/bin/sh
# Runs as root on a node. Reads the API token on stdin and installs it for the daemon.
# The directory belongs to root and is not writable by anyone else, so no service can create or
# swap an entry in it. The file belongs to the daemon's user alone (mode 0400): the other
# services share the spinifex group, so group access would hand them the team-wide token. The
# temporary file has an unpredictable name and is created exclusively. A symlink, or an owner
# other than root or the daemon's user, where the directory or the file should be is refused.
# Ownership and mode are set on every run, so an older install converges. The file's content is
# replaced only when the token differs. Prints installed or unchanged.
set -eu
dir="${API_TOKEN_DIR:-/etc/spinifex/latitude}"
root="${API_TOKEN_ROOT-root}"
rootgroup="${API_TOKEN_ROOT_GROUP-root}"
user="${API_TOKEN_USER-spinifex-daemon}"
umask 077

fail() { echo "$*" >&2; exit 1; }
owner_of() { stat -c %U "$1"; }

[ ! -L "$dir" ] || fail "$dir is a symlink"
if [ ! -e "$dir" ]; then mkdir -p "$dir"; fi
[ -d "$dir" ] || fail "$dir is not a directory"
[ "$(owner_of "$dir")" = "$root" ] || fail "$dir is not owned by $root"
chown "$root:$rootgroup" "$dir"
chmod 0711 "$dir"

target="$dir/api-token"
[ ! -L "$target" ] || fail "$target is a symlink"
if [ -e "$target" ]; then
    [ -f "$target" ] || fail "$target is not a regular file"
    holder="$(owner_of "$target")"
    [ "$holder" = "$root" ] || [ "$holder" = "$user" ] || fail "$target is owned by $holder"
fi

tmp="$(mktemp "$dir/.api-token.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
[ -s "$tmp" ] || fail "empty token on stdin"
if [ -f "$target" ] && cmp -s "$tmp" "$target"; then
    chown "$user" "$target"
    chmod 0400 "$target"
    echo unchanged
    exit 0
fi
chown "$user" "$tmp"
chmod 0400 "$tmp"
mv -f "$tmp" "$target"
echo installed
