#!/usr/bin/env bash
# Checks that neither API token is ever an argument of an external command in the node token
# paths of deploy-latitude.sh. External commands are stand-ins that record their arguments and
# their standard input; the test fails if a token is in the first and proves it is in the second.
# preflight runs this with sh
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
set -euo pipefail
exec </dev/null
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/home"
real_python="$(command -v python3)"

AUTH=FAKEAUTHTOKEN9
NODE=FAKENODETOKEN7

# Every command the script runs outside bash records "name args" and, for the first line of
# standard input, "name stdin: ...".
for c in curl ssh scp aws tofu ssh-keygen; do
    cat > "$work/bin/$c" <<'SH'
#!/bin/sh
echo "$(basename "$0") $*" >> "$FAKE_ARGV"
if [ ! -t 0 ]; then
    in="$(cat)"
    [ -z "$in" ] || echo "$(basename "$0") stdin: $in" >> "$FAKE_STDIN"
fi
case "$(basename "$0")" in
    curl) printf '{"data":[{"attributes":{"token_last_slice":"%s","read_only":false}}]}\n200' "$(printf '%s' "$FAKE_NODE" | tail -c 5)" ;;
    ssh)
        if [ -n "${FAKE_SSH_EVAL:-}" ]; then
            # Behave like a login on a node: skip the options and host, run the rest with the
            # connection variable of this connection, and let sudo reset the environment.
            while [ "$#" -gt 0 ]; do case "$1" in ubuntu@*) shift; break ;; *) shift ;; esac; done
            SSH_CONNECTION="$FAKE_SSH_CONNECTION" bash -c "$*"
        else
            echo installed
        fi ;;
esac
SH
    chmod +x "$work/bin/$c"
done
cat > "$work/bin/sudo" <<'SH'
#!/bin/sh
while [ "$#" -gt 0 ]; do case "$1" in -*) shift ;; *) break ;; esac; done
exec env -i PATH="$PATH" "$@"
SH
chmod +x "$work/bin/sudo"
cat > "$work/bin/python3" <<SH
#!/bin/sh
echo "python3 \$*" >> "\$FAKE_ARGV"
exec "$real_python" "\$@"
SH
chmod +x "$work/bin/python3"

export FAKE_ARGV="$work/argv" FAKE_STDIN="$work/stdin" FAKE_NODE="$NODE" PATH="$work/bin:$PATH" HOME="$work/home"
: > "$FAKE_ARGV"
: > "$FAKE_STDIN"
export LATITUDESH_AUTH_TOKEN="$AUTH" LATITUDESH_NODE_TOKEN="$NODE"

# shellcheck source=deploy-latitude.sh
source "$here/deploy-latitude.sh"

# shellcheck disable=SC2034 # read by the sourced functions
NODES=3 IP=192.0.2.10 NODE_NAME=node1
check_node_token >/dev/null
install_key >/dev/null 2>&1

fails=0
for tok in "$AUTH" "$NODE" "${NODE: -5}"; do
    if grep -q "$tok" "$FAKE_ARGV"; then
        echo "FAIL token $tok is in a command's arguments:"
        grep "$tok" "$FAKE_ARGV"
        fails=$((fails + 1))
    else
        echo "ok   $tok is in no argument"
    fi
done
for what in "curl stdin:.*Bearer $NODE" "ssh stdin: $NODE"; do
    if grep -q "$what" "$FAKE_STDIN"; then
        echo "ok   the check can see the token where it is sent ($what)"
    else
        echo "FAIL the token never reached stdin ($what): the test would pass without checking anything"
        fails=$((fails + 1))
    fi
done

# The peer address comes from the same connection that runs the command, whichever source
# address that connection has, and survives sudo's environment reset.
export FAKE_SSH_EVAL=1
for src in 198.51.100.7 203.0.113.99; do
    export FAKE_SSH_CONNECTION="$src 51000 192.0.2.10 22"
    got="$(on 'printf %s "$SSH_PEER"')"
    if [ "$got" = "$src" ]; then echo "ok   peer $src reaches the command"; else echo "FAIL peer for $src was '$got'"; fails=$((fails + 1)); fi
done

# A three-node run needs a build that knows the latitude source: a release would be refused by its
# own config check. The guard needs --distro, and the node is checked before it gets the pool.
check_result() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3' got '$2'"; fails=$((fails + 1)); fi; }
check_result "three nodes without --distro stop" "$( (NODES=3 DISTRO="" require_branch_build) >/dev/null 2>&1 && echo ran || echo stopped)" stopped
check_result "three nodes with --distro go on" "$( (NODES=3 DISTRO=/tmp/x.tar.gz require_branch_build) >/dev/null 2>&1 && echo ran || echo stopped)" ran
check_result "one node without --distro goes on" "$( (NODES=1 DISTRO="" require_branch_build) >/dev/null 2>&1 && echo ran || echo stopped)" ran
check_result "one node with one address goes on" "$( (NODES=1 TF_VAR_elastic_ip_count=1 require_one_address) >/dev/null 2>&1 && echo ran || echo stopped)" ran
check_result "one node with no setting goes on" "$( (NODES=1 TF_VAR_elastic_ip_count="" require_one_address) >/dev/null 2>&1 && echo ran || echo stopped)" ran
check_result "one node with two addresses stops" "$( (NODES=1 TF_VAR_elastic_ip_count=2 require_one_address) >/dev/null 2>&1 && echo ran || echo stopped)" stopped
check_result "three nodes with three addresses go on" "$( (NODES=3 TF_VAR_elastic_ip_count=3 require_one_address) >/dev/null 2>&1 && echo ran || echo stopped)" ran
printf 'binary with latitude_elastic_ip_ids inside\n' > "$work/spx-new"
printf 'binary of a release\n' > "$work/spx-old"
SPX_BIN="$work/spx-new"
check_result "a build that knows latitude is accepted" "$( (node_knows_latitude) >/dev/null 2>&1 && echo ran || echo stopped)" ran
# shellcheck disable=SC2034 # read by the sourced functions
SPX_BIN="$work/spx-old"
check_result "a release is refused" "$( (node_knows_latitude) >/dev/null 2>&1 && echo ran || echo stopped)" stopped
# The profile spx writes under sudo is in the ubuntu user's home, so node-side reads name that
# directory instead of relying on root's HOME.
mkdir "$work/awsdir"
printf '[other]\naws_access_key_id = X\n[spinifex]\naws_access_key_id = AK1\naws_secret_access_key = SK1\n[z]\nq = 1\n' > "$work/awsdir/credentials"
node_ip() { printf 192.0.2.10; }
# shellcheck disable=SC2034 # read by the sourced functions
NODE_AWS_DIR="$work/awsdir"
check_result "credentials come from the node's AWS dir" "$(cmd_credentials | tr '\n' ' ')" "[spinifex] aws_access_key_id = AK1 aws_secret_access_key = SK1 "

# A failed API call logs its status and a documented code, never the response text, which can
# echo the request's headers.
mkdir "$work/bin2"
cat > "$work/bin2/curl" <<'SH'
#!/bin/sh
cat > /dev/null
printf '{"errors":[{"code":"%s","detail":"Authorization: Bearer %s"}]}\n401' "$FAKE_CODE" "$FAKE_LEAK"
SH
chmod +x "$work/bin2/curl"
logged="$(FAKE_CODE=UNAUTHORIZED FAKE_LEAK="$AUTH" PATH="$work/bin2:$PATH" latitude GET /servers 2>&1 || true)"
check_result "a failed call names its status and code" "$(grep -c 'returned 401 (UNAUTHORIZED)' <<<"$logged" || true)" 1
check_result "a failed call never logs the token" "$(grep -c "$AUTH" <<<"$logged" || true)" 0
logged="$(FAKE_CODE="$AUTH" FAKE_LEAK="$AUTH" PATH="$work/bin2:$PATH" latitude GET /servers 2>&1 || true)"
check_result "an undocumented code is not logged" "$(grep -c "$AUTH" <<<"$logged" || true)" 0

# A recorded ID that now names another deployment's object is not ours to delete.
# shellcheck disable=SC2034 # read by the sourced functions
NAME=demo
latitude() { printf '%s' "$FAKE_LAT"; }
expected_project_id() { echo proj_1; }
srv() {
    local frag=',"project":{"id":"proj_1"}'
    [ "$#" -lt 2 ] || frag="$2"
    printf '{"data":{"id":"sv_1","attributes":{"hostname":"%s"%s}}}' "$1" "$frag"
}
FAKE_LAT="$(srv spx-demo-node2)"
check_result "a recorded server with our hostname is ours" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" ours
FAKE_LAT="$(srv spx-other-node1)"
check_result "a recorded server with another hostname is not" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=1
FAKE_LAT="$(srv spx-demo2-node1)"
check_result "a hostname that only starts like ours is not" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=1
FAKE_LAT="$(srv spx-demo-node1 ',"project":{"id":"proj_other"}')"
check_result "our hostname in another project is not ours" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=1
FAKE_LAT="$(srv spx-demo-node1 '')"
check_result "a server with no project is unknown" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=2
FAKE_LAT="$(srv spx-demo-node1 ',"project":{}')"
check_result "a project with no id is unknown" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=2
FAKE_LAT="$(srv spx-demo-node1 ',"project":null')"
check_result "a null project is unknown" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=2
expected_project_id() { return 1; }
FAKE_LAT="$(srv spx-demo-node1)"
check_result "no known project id is unknown" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=2
expected_project_id() { echo proj_1; }
FAKE_LAT='not json'
check_result "an unreadable object is unknown" "$(recorded_identity server sv_1 && echo ours || echo "rc=$?")" rc=2
KNOWN_IDS="$work/ids"
printf 'server sv_1\n' > "$KNOWN_IDS"
lat_exists() { return 0; }
FAKE_LAT="$(srv spx-other-node1)"
check_result "destroy stops before Terraform when a recorded ID was reused" "$( (check_recorded_identity) >/dev/null 2>&1 && echo went || echo stopped)" stopped
FAKE_LAT="$(srv spx-demo-node1 ',"project":{"id":"proj_other"}')"
check_result "destroy stops when the same hostname is in another project" "$( (check_recorded_identity) >/dev/null 2>&1 && echo went || echo stopped)" stopped
FAKE_LAT="$(srv spx-demo-node1 '')"
check_result "destroy stops when the project cannot be read" "$( (check_recorded_identity) >/dev/null 2>&1 && echo went || echo stopped)" stopped
FAKE_LAT="$(srv spx-demo-node1)"
check_result "destroy goes on when the recorded server is ours" "$( (check_recorded_identity) >/dev/null 2>&1 && echo went || echo stopped)" went
leftovers_out="$( (lat_match() { return 0; }; FAKE_LAT="$(srv spx-other-node1)" leftovers) 2>/dev/null || true)"
check_result "a reused ID is not listed as a leftover to delete" "$(grep -c sv_1 <<<"$leftovers_out" || true)" 0

# Cleanup never deletes an object whose ownership could not be read, recorded or not.
deleted="$work/deleted"
: > "$deleted"
latitude() { printf '%s %s\n' "$1" "$2" >> "$deleted"; }
printf 'server sv_1\nserver sv_2\n' > "$KNOWN_IDS"
delete_left $'server sv_1 sv_1 (unknown)\nserver sv_2 spx-demo-node2\n' "" >/dev/null 2>&1
check_result "an unreadable recorded object is not deleted" "$(grep -c sv_1 "$deleted" || true)" 0
check_result "a verified recorded object is deleted" "$(grep -c 'DELETE .*sv_2' "$deleted" || true)" 1

# Both deployment sizes write the metadata remap.
cfg="$work/spinifex.toml"
extract() { sed -n "/^$1()/,/^}/p" "$here/deploy-latitude.sh" | sed -n "/<<'PY'/,/^PY\$/p" | sed '1d;$d' | sed "s|/etc/spinifex/spinifex.toml|$cfg|"; }
printf '[network]\nexternal_mode = "nat"\n\n[bootstrap]\ny = 1\n' > "$cfg"
extract configure_pool | python3 -I - 203.0.113.1 203.0.113.254
check_result "one node gets the metadata remap" "$(grep -c 'imds_host_meta_ip = "169.254.42.254"' "$cfg")" 1
printf '[network]\nexternal_mode = "nat"\n\n[nodes.node1]\nx = 1\n\n[nodes.node2]\nx = 1\n\n[nodes.node3]\nx = 1\n\n[bootstrap]\ny = 1\n' > "$cfg"
extract configure_latitude_pool | python3 -I - proj_1 eip_1,eip_2 node1=sv_1,node2=sv_2,node3=sv_3
check_result "three nodes get the metadata remap" "$(grep -c 'imds_host_dns_ip  = "169.254.42.253"' "$cfg")" 1
check_result "three nodes get the latitude pool" "$(grep -c 'source                  = "latitude"' "$cfg")" 1
extract configure_latitude_pool | python3 -I - proj_1 eip_1,eip_2 node1=sv_1,node2=sv_2,node3=sv_3
check_result "writing the config again changes nothing" "$(grep -c 'imds_host_meta_ip' "$cfg")" 1
unset FAKE_SSH_EVAL

[ "$fails" -eq 0 ]
