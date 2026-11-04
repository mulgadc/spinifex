#!/bin/sh
# Tests node-install-api-token.sh and node-pool-state.sh with stand-in tools.
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fails=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fails=$((fails + 1)); fi; }

# token install: A, A again, then B across a rotation; the file ends as B, owned by the daemon
# user alone (0400) in a 0711 directory that only root may change
me="$(id -un)"
export API_TOKEN_DIR="$work/tok" API_TOKEN_ROOT="$me" API_TOKEN_ROOT_GROUP="$(id -gn)" API_TOKEN_USER="$me"
install_token() { sh "$here/node-install-api-token.sh"; }
check "first install" "$(printf 'TOKENA' | install_token)" installed
check "same token" "$(printf 'TOKENA' | install_token)" unchanged
check "rotation" "$(printf 'TOKENB' | install_token)" installed
check "file holds B" "$(cat "$work/tok/api-token")" TOKENB
check "file mode and owner" "$(stat -c '%U %a' "$work/tok/api-token")" "$me 400"
check "dir mode and owner" "$(stat -c '%U %a' "$work/tok")" "$me 711"
check "no temp left after install" "$(ls -A "$work/tok")" api-token
check "empty refused" "$(printf '' | install_token 2>/dev/null || echo failed)" failed
check "file kept after refusal" "$(cat "$work/tok/api-token")" TOKENB

# an older install converges on the unchanged path too
chmod 0640 "$work/tok/api-token"
chmod 0755 "$work/tok"
check "unchanged path repairs" "$(printf 'TOKENB' | install_token)" unchanged
check "mode repaired on unchanged" "$(stat -c %a "$work/tok/api-token")" 400
check "dir mode repaired on unchanged" "$(stat -c %a "$work/tok")" 711

# who can read it: only the daemon's identity. This needs two real identities, so it runs as root only.
if [ "$(id -u)" = 0 ]; then
    chmod 0711 "$work"
    export API_TOKEN_ROOT=root API_TOKEN_ROOT_GROUP=root API_TOKEN_USER=daemon
    for mode in installed unchanged; do
        [ "$mode" = installed ] && rm -f "$work/tok/api-token"
        check "$mode path" "$(printf 'TOKENB' | install_token)" "$mode"
        check "daemon reads ($mode)" "$(setpriv --reuid=daemon --regid=daemon --clear-groups cat "$work/tok/api-token")" TOKENB
        check "another identity in the shared group cannot read ($mode)" \
            "$(setpriv --reuid=nobody --regid=daemon --clear-groups cat "$work/tok/api-token" 2>/dev/null || echo denied)" denied
    done
    export API_TOKEN_ROOT="$me" API_TOKEN_ROOT_GROUP="$(id -gn)" API_TOKEN_USER="$me"
else
    echo "skip reading as the daemon and as another service: needs root"
fi

# a link planted where the file or the directory belongs is refused, and nothing it points at is touched
echo SENTINEL > "$work/sentinel"
rm -f "$work/tok/api-token"
ln -s "$work/sentinel" "$work/tok/api-token"
check "symlinked file refused" "$(printf 'TOKENC' | sh "$here/node-install-api-token.sh" 2>/dev/null || echo refused)" refused
check "symlink target untouched" "$(cat "$work/sentinel")" SENTINEL
ln -s "$work/tok" "$work/tok-link"
check "symlinked dir refused" "$(printf 'TOKENC' | API_TOKEN_DIR="$work/tok-link" sh "$here/node-install-api-token.sh" 2>/dev/null || echo refused)" refused
check "dir owned by someone else refused" "$(printf 'TOKENC' | API_TOKEN_ROOT=nobody-such install_token 2>/dev/null || echo refused)" refused
rm -f "$work/tok/api-token"
printf 'TOKENB' | install_token >/dev/null
check "file owned by a stranger refused" "$(printf 'TOKENC' | API_TOKEN_ROOT=$me API_TOKEN_USER=nobody-such install_token 2>/dev/null || echo refused)" refused
check "no temp left" "$(ls -A "$work/tok")" api-token

# data disk choice: the largest blank disk, the lowest path among equals, the rest left alone
choose_dir="$work/choose"
mkdir -p "$choose_dir/bin"
cat > "$choose_dir/bin/blockdev" <<'SH'
#!/bin/sh
# blockdev --getsize64 <id>: the fixture's size for that disk
cat "$CHOOSE_DIR/size.$(basename "$2")"
SH
chmod +x "$choose_dir/bin/blockdev"
eval "$(sed -n '/^choose_data_disk()/,/^}/p' "$here/host-data-disk.sh")"
size_of() { printf '%s' "$2" > "$choose_dir/size.$1"; }
pick() { CHOOSE_DIR="$choose_dir" PATH="$choose_dir/bin:$PATH" choose_data_disk "$@" 2>"$choose_dir/err"; }
size_of nvme-eui.0b 1700000000000
size_of nvme-eui.0a 1700000000000
size_of nvme-eui.09 447000000000
size_of nvme-eui.0c 1900000000000
check "largest of three wins" "$(pick /dev/disk/by-id/nvme-eui.09 /dev/disk/by-id/nvme-eui.0b /dev/disk/by-id/nvme-eui.0c)" /dev/disk/by-id/nvme-eui.0c
check "equal sizes: the lowest path" "$(pick /dev/disk/by-id/nvme-eui.09 /dev/disk/by-id/nvme-eui.0b /dev/disk/by-id/nvme-eui.0a)" /dev/disk/by-id/nvme-eui.0a
check "order of the list does not matter" "$(pick /dev/disk/by-id/nvme-eui.0b /dev/disk/by-id/nvme-eui.0a /dev/disk/by-id/nvme-eui.09)" /dev/disk/by-id/nvme-eui.0a
check "what was left is said" "$(grep -c 'left untouched: /dev/disk/by-id/nvme-eui.09 /dev/disk/by-id/nvme-eui.0b' "$choose_dir/err")" 1
check "one disk is just chosen" "$(pick /dev/disk/by-id/nvme-eui.09)" /dev/disk/by-id/nvme-eui.09
check "one disk says nothing" "$(wc -c < "$choose_dir/err" | tr -d ' ')" 0

# pool state: the stand-in journal serves only the lines of the invocation asked for
mkdir "$work/bin"
cat > "$work/bin/systemctl" <<'SH'
#!/bin/sh
cat "$FAKE_DIR/invocation"
SH
cat > "$work/bin/journalctl" <<'SH'
#!/bin/sh
for a in "$@"; do
    case "$a" in _SYSTEMD_INVOCATION_ID=*) id="${a#_SYSTEMD_INVOCATION_ID=}" ;; esac
done
cat "$FAKE_DIR/journal.$id" 2>/dev/null
SH
chmod +x "$work/bin/systemctl" "$work/bin/journalctl"
export FAKE_DIR="$work" PATH="$work/bin:$PATH"
state() { sh "$here/node-pool-state.sh" latitude; }

echo old > "$work/invocation"
echo 'INFO floating pool ready pool=latitude source=latitude' > "$work/journal.old"
check "current success" "$(state)" ready

echo new > "$work/invocation"
echo 'ERROR floating: the provider refused the pool'"'"'s first call; public addresses will fail until fixed pool=latitude' > "$work/journal.new"
check "old success then current failure" "$(state)" refused

echo 'INFO starting' > "$work/journal.new"
check "current invocation not yet ready" "$(state)" waiting

echo 'INFO floating pool ready pool=other source=latitude' > "$work/journal.new"
check "another pool does not count" "$(state)" waiting

echo 'INFO floating pool ready pool=latitude source=latitude' > "$work/journal.new"
check "current success" "$(state)" ready

[ "$fails" -eq 0 ]
