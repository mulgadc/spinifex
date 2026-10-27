#!/bin/sh
# setup_host_test.sh — tests for setup.sh's DNS handover to systemd-resolved,
# its chrony drop-in and its nbdkit plugin directory.
#
# setup.sh is sourced with INSTALL_SPINIFEX_LIB_ONLY=1 so main never runs. PATH
# holds stub commands and links to a few real tools only, so no host resolver,
# package manager or service is ever touched.
# shellcheck disable=SC2016  # stub bodies are single-quoted so they expand when the stub runs
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SETUP="$SCRIPT_DIR/setup.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "  ok   - $1"; }
fail() { echo "  FAIL - $1"; fails=$((fails + 1)); }

echo "-- setup.sh host DNS, chrony and nbdkit"

mkdir -p "$TMP/sys"
for tool in bash awk sed grep readlink basename dirname cp ln mkdir cat head tail wc tee rm chmod; do
    ln -s "$(command -v "$tool")" "$TMP/sys/$tool"
done

# stub <name> <body> — a stub command; $S is the fixture's state directory.
stub() {
    printf '#!/bin/sh\nS="%s"\n%s\n' "$TMP/state" "$2" > "$TMP/bin/$1"
    chmod +x "$TMP/bin/$1"
}

# host — a fresh fixture: resolved installed and inactive, resolvconf the
# resolvectl link, NetworkManager running with no DNS settings, a dnf host
# whose first enabled repository is yum.oracle.com.
host() {
    rm -rf "${TMP:?}/state" "${TMP:?}/root" "${TMP:?}/bin"
    mkdir -p "$TMP/state/active" "$TMP/root/etc" "$TMP/root/usr/lib/systemd/system" "$TMP/bin"
    : > "$TMP/root/usr/lib/systemd/system/systemd-resolved.service"
    : > "$TMP/state/active/NetworkManager.service"
    printf 'nameserver 192.168.1.1\nsearch example.lan\n' > "$TMP/root/etc/resolv.conf"
    printf '[main]\n# dns=default\nconfigure-and-quit=no\n' > "$TMP/state/nm-config"
    printf 'Global:\nLink 2 (br-wan): 192.168.1.1\n' > "$TMP/state/resolvectl-dns"
    printf '[ol10_UEKR8]\nbaseurl = https://mirror.example/uek/\nenabled = 0\n[ol10_baseos_latest]\nbaseurl = https://yum.oracle.com/repo/OracleLinux/OL10/baseos/latest/x86_64/\nenabled = 1\n' > "$TMP/state/dnf-dump"
    stub systemctl '[ "$1" = is-active ] && { [ -e "$S/active/$3" ]; exit; }
echo "$*" >> "$S/calls"
[ "$1 $2" = "enable --now" ] && : > "$S/active/$3"
exit 0'
    stub NetworkManager 'cat "$S/nm-config"'
    stub resolvectl '[ "$1" = dns ] && cat "$S/resolvectl-dns"'
    stub resolvectl-real ':'
    ln -s resolvectl "$TMP/bin/resolvconf"
    stub getent '[ ! -e "$S/getent-fail" ] && echo "$2" >> "$S/looked-up"'
    stub dnf '[ "$1 $2" = "config-manager --dump" ] && cat "$S/dnf-dump"'
    stub sleep ':'
}

# run <function...> — runs setup.sh functions against the fixture. Sets RC and OUT.
run() {
    RC=0
    OUT=$(PATH="$TMP/bin:$TMP/sys" DISTRO_ROOT="$TMP/root" \
        SPINIFEX_RESOLV_CONF="$TMP/root/etc/resolv.conf" SPINIFEX_RESOLV_CONF_SAVED="$TMP/root/saved/resolv.conf.orig" \
        INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. "$1"; shift; for f in "$@"; do $f; done' _ "$SETUP" "$@" 2>&1) || RC=$?
}

calls() { cat "$TMP/state/calls" 2>/dev/null || true; }
saved() { cat "$TMP/root/saved/resolv.conf.orig" 2>/dev/null || echo "<none>"; }
expect_untouched() {
    if [ "$RC" = 0 ] && [ -z "$(calls)" ] && [ ! -L "$TMP/root/etc/resolv.conf" ] && [ "$(saved)" = "<none>" ]; then
        pass "$1"
    else
        fail "$1: rc=$RC calls='$(calls)' saved='$(saved)': $OUT"
    fi
}
expect_fatal() { # expect_fatal <desc> <text>...
    desc=$1; shift
    if [ "$RC" = 0 ]; then fail "$desc (succeeded): $OUT"; return; fi
    for want in "$@"; do
        case "$OUT" in *"$want"*) ;; *) fail "$desc (output lacks '$want'): $OUT"; return ;; esac
    done
    pass "$desc"
}

# --------------------------------------------------------------------------
# DNS handover.
# --------------------------------------------------------------------------
host
run hand_dns_to_resolved
if [ "$RC" = 0 ] && [ "$(readlink "$TMP/root/etc/resolv.conf")" = /run/systemd/resolve/stub-resolv.conf ] \
    && [ "$(saved)" = "$(printf 'nameserver 192.168.1.1\nsearch example.lan')" ] \
    && [ "$(calls)" = "$(printf 'enable --now systemd-resolved.service\nrestart NetworkManager.service')" ] \
    && [ "$(cat "$TMP/state/looked-up")" = yum.oracle.com ]; then
    pass "hands DNS to resolved: saves resolv.conf, starts resolved, restarts NM, links the stub, looks up the repo host"
else
    fail "handover: rc=$RC calls='$(calls)' saved='$(saved)' link='$(readlink "$TMP/root/etc/resolv.conf")': $OUT"
fi

: > "$TMP/state/calls"
rm -f "$TMP/state/looked-up"
run hand_dns_to_resolved
if [ "$RC" = 0 ] && [ -z "$(calls)" ] && [ "$(cat "$TMP/state/looked-up")" = yum.oracle.com ]; then
    pass "a rerun after a handover checks DNS again and changes nothing"
else
    fail "rerun after handover: rc=$RC calls='$(calls)': $OUT"
fi

host
mkdir -p "$TMP/root/saved"
echo "the original" > "$TMP/root/saved/resolv.conf.orig"
run hand_dns_to_resolved
if [ "$RC" = 0 ] && [ "$(saved)" = "the original" ]; then
    pass "a rerun never overwrites the saved resolv.conf"
else
    fail "saved copy overwritten: rc=$RC saved='$(saved)': $OUT"
fi

host
rm "$TMP/root/usr/lib/systemd/system/systemd-resolved.service"
run hand_dns_to_resolved
expect_untouched "resolved not installed: DNS is left alone"

host
: > "$TMP/state/active/systemd-resolved.service"
run hand_dns_to_resolved
expect_untouched "resolved already active, with no earlier handover: DNS is left alone"

host
rm "$TMP/bin/resolvconf"
stub resolvconf ':'
run hand_dns_to_resolved
expect_untouched "a real resolvconf: DNS is left alone"

host
rm "$TMP/state/active/NetworkManager.service"
run hand_dns_to_resolved
case "$OUT" in
    *"NetworkManager is not running"*) expect_untouched "NetworkManager not running: DNS is left alone, with a warning" ;;
    *) fail "NetworkManager not running: no warning: $OUT" ;;
esac

for setting in dns=none dns=dnsmasq systemd-resolved=false; do
    host
    printf '[main]\n# dns=default\n%s\n[logging]\ndns=none\n' "$setting" > "$TMP/state/nm-config"
    run hand_dns_to_resolved
    expect_fatal "NetworkManager $setting stops the install, naming it" "NetworkManager sets $setting"
    [ -z "$(calls)" ] || fail "NetworkManager $setting: acted before refusing: $(calls)"
done

host
printf '[main]\ndns=systemd-resolved\n[logging]\ndns=none\n' > "$TMP/state/nm-config"
run hand_dns_to_resolved
[ "$RC" = 0 ] && pass "dns=systemd-resolved, and dns= outside [main], are accepted" || fail "dns=systemd-resolved refused: $OUT"

host
printf 'Global:\nLink 2 (br-wan):\n' > "$TMP/state/resolvectl-dns"
run hand_dns_to_resolved
expect_fatal "no upstream servers stops the install, naming the check and printing the restore" \
    "'resolvectl dns' lists no DNS servers" "systemctl disable --now systemd-resolved" "cp -a --remove-destination $TMP/root/saved/resolv.conf.orig"

host
: > "$TMP/state/getent-fail"
run hand_dns_to_resolved
expect_fatal "a repository host that no longer resolves stops the install, naming it" "yum.oracle.com no longer resolves" "systemctl restart NetworkManager"

host
printf '[local]\nbaseurl = file:///srv/repo/\nenabled = 1\n' > "$TMP/state/dnf-dump"
run hand_dns_to_resolved
case "$RC:$OUT" in
    0:*"not checking that names resolve"*) pass "a file:// repository skips the name check with a warning" ;;
    *) fail "file:// repository: rc=$RC: $OUT" ;;
esac

host
printf '[local]\nbaseurl = http://10.0.0.5/repo/\nenabled = 1\n' > "$TMP/state/dnf-dump"
run hand_dns_to_resolved
case "$RC:$OUT" in
    0:*"10.0.0.5 is an IP address"*) pass "a repository at an IP address skips the name check with a warning" ;;
    *) fail "IP repository: rc=$RC: $OUT" ;;
esac

# --------------------------------------------------------------------------
# Repository URLs and their hosts.
# --------------------------------------------------------------------------
host
printf '[a]\nbaseurl = \nenabled = 1\nmirrorlist = https://mirrors.example/list\n[b]\nbaseurl = https://b.example/\nenabled = 1\n' > "$TMP/state/dnf-dump"
run first_repo_url
[ "$OUT" = https://mirrors.example/list ] && pass "dnf: a mirrorlist stands in for an empty baseurl" || fail "dnf mirrorlist: '$OUT'"
printf '[a]\nbaseurl = https://a1.example/, https://a2.example/\nenabled = 1\n' > "$TMP/state/dnf-dump"
run first_repo_url
[ "$OUT" = https://a1.example/ ] && pass "dnf: the first of several baseurls" || fail "dnf baseurl list: '$OUT'"

host
rm "$TMP/bin/dnf"
stub apt-get '[ "$1" = indextargets ] && printf "http://deb.debian.org/debian/\nhttp://deb.debian.org/debian-security/\n"'
run first_repo_url
[ "$OUT" = http://deb.debian.org/debian/ ] && pass "apt: the first repository URI" || fail "apt: '$OUT'"

for case in "https://yum.oracle.com/repo/=yum.oracle.com" "http://user@mirror.lan:8080/x=mirror.lan" \
    "http://[2001:db8::1]:80/x=[2001:db8::1]" "file:///srv/repo=" "cdrom:[Debian]/=" "=" ; do
    url=${case%=*}
    want=${case##*=}
    run "url_host $url"
    [ "$OUT" = "$want" ] && pass "url_host '$url' is '$want'" || fail "url_host '$url' = '$OUT', want '$want'"
done

# --------------------------------------------------------------------------
# chrony drop-in.
# --------------------------------------------------------------------------
host
mkdir -p "$TMP/root/etc/chrony"
printf 'pool 2.debian.pool.ntp.org iburst\nmakestep 1 3\nconfdir /etc/chrony/conf.d\n' > "$TMP/root/etc/chrony/chrony.conf"
before=$(cat "$TMP/root/etc/chrony/chrony.conf")
run install_chrony_conf
if [ "$RC" = 0 ] && grep -qx 'makestep 1 -1' "$TMP/root/etc/chrony/conf.d/spinifex.conf" 2>/dev/null \
    && [ "$(cat "$TMP/root/etc/chrony/chrony.conf")" = "$before" ]; then
    case "$OUT" in
        *WARN*) fail "Debian chrony layout warned: $OUT" ;;
        *) pass "Debian: the drop-in goes in the confdir, chrony.conf is not edited" ;;
    esac
else
    fail "Debian chrony: rc=$RC: $OUT"
fi

host
printf 'pool 2.pool.ntp.org iburst\nmakestep 1.0 3\nrtcsync' > "$TMP/root/etc/chrony.conf"
run install_chrony_conf
run install_chrony_conf
if [ "$RC" = 0 ] && grep -qx 'makestep 1 -1' "$TMP/root/etc/chrony.d/spinifex.conf" 2>/dev/null \
    && [ "$(grep -c '^confdir' "$TMP/root/etc/chrony.conf")" = 1 ] \
    && [ "$(tail -n 1 "$TMP/root/etc/chrony.conf")" = "confdir /etc/chrony.d" ] \
    && grep -qx rtcsync "$TMP/root/etc/chrony.conf"; then
    pass "EL: one confdir line is appended after the stock makestep, and the drop-in goes there"
else
    fail "EL chrony: rc=$RC: $OUT: $(cat "$TMP/root/etc/chrony.conf")"
fi

host
printf 'confdir /etc/chrony.d\nmakestep 1.0 3\n' > "$TMP/root/etc/chrony.conf"
run install_chrony_conf
case "$RC:$OUT" in
    0:*"makestep on line 2, after its confdir line"*) pass "a makestep after the confdir line warns that it overrides the drop-in" ;;
    *) fail "late makestep: rc=$RC: $OUT" ;;
esac

host
run install_chrony_conf
case "$RC:$OUT" in
    0:*"Skipping the chrony drop-in"*) pass "no chrony config: the drop-in is skipped with a warning" ;;
    *) fail "no chrony: rc=$RC: $OUT" ;;
esac

# --------------------------------------------------------------------------
# nbdkit plugin directory.
# --------------------------------------------------------------------------
host
stub nbdkit '[ "$1" = --dump-config ] && printf "binary=/usr/sbin/nbdkit\nplugindir=/usr/lib64/nbdkit/plugins\n"'
OUT=$(PATH="$TMP/bin:$TMP/sys" INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. "$1"; resolve_nbdkit_plugindir; echo "DIR=$NBDKIT_PLUGINDIR"' _ "$SETUP" 2>&1) && RC=0 || RC=$?
[ "$OUT" = DIR=/usr/lib64/nbdkit/plugins ] && pass "the plugin directory comes from nbdkit --dump-config" || fail "nbdkit plugindir: rc=$RC: $OUT"

host
stub nbdkit 'exit 1'
run resolve_nbdkit_plugindir
expect_fatal "a broken nbdkit stops the install, naming it" "nbdkit --dump-config"

host
run resolve_nbdkit_plugindir
expect_fatal "no nbdkit stops the install, naming it" "nbdkit --dump-config"

if [ "$fails" -gt 0 ]; then
    echo "$fails failure(s)"
    exit 1
fi
