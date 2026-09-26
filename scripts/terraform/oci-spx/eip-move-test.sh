#!/usr/bin/env bash
# eip-move-test.sh — prove an EIP follows its guest across an OCI node move.
#
# Drives the three-node Terraform OCI cluster through the exact sequence that
# strands an address today: associate an EIP, stop the guest, force it to start
# on a different node, and assert the address still answers. The decisive check
# is not the ping — it is the OCI API's own record of which VNIC carries the
# private half, because a reachable address on the wrong VNIC is luck.
#
# Step 0b first proves the other half of the distinction on the same guest: the
# auto-assigned address it launched with is gone from the OCI API while it is
# stopped, and a different one comes back. That is the billing claim, asserted
# against OCI rather than against our own record of what we think we released.
#
# Read-write on dev-oci only. Never point this at prod or dev-prod.
set -euo pipefail

KEY=${KEY:-$HOME/.ssh/oci-poc.pem}
OCI_BIN=${OCI_BIN:-$HOME/bin/oci}
OCI_PROFILE=${OCI_PROFILE:-apacanzset03child03}
SUBNET=${SUBNET:-ocid1.subnet.oc1.ap-sydney-1.aaaaaaaaimrhjgsqgrfg7bnka2d3nbx2n5ss7kurcwneon22rkclqf3ypfqa}

# Public address -> node name. The control plane is reached through node01.
declare -A NODE_IP=(
    [spinifexnode01]=149.118.68.212
    [spinifexnode02]=137.23.19.167
    [spinifexnode03]=137.23.14.3
)
API_HOST=${API_HOST:-${NODE_IP[spinifexnode01]}}

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; FAILED=1; }
ok()   { printf '\033[32mok: %s\033[0m\n' "$*"; }
FAILED=0

# Assertions take values rather than an exit status: `cond && ok || fail` runs
# the failure branch whenever ok's own status is non-zero, and passing $? through
# a helper only moves the same trap one line down.
eq() { # eq <actual> <expected> <what>
    if [[ $1 == "$2" ]]; then ok "$3 ($1)"; else fail "$3: got '$1', wanted '$2'"; fi
}
unset_or_none() { # unset_or_none <actual> <what>
    if [[ -z $1 || $1 == None ]]; then ok "$2"; else fail "$2: got '$1'"; fi
}
is_set() { # is_set <actual> <what>
    if [[ -n $1 && $1 != None ]]; then ok "$2"; else fail "$2: got '$1'"; fi
}

on() { # on <ip> <cmd...>
    local ip=$1; shift
    timeout 60 ssh -i "$KEY" -o StrictHostKeyChecking=no -o BatchMode=yes "ubuntu@$ip" "$@"
}

aws_spx() { # aws_spx <args...>  — run the AWS CLI from node01 against the cluster
    on "$API_HOST" "export AWS_PROFILE=spinifex AWS_CA_BUNDLE=/etc/spinifex/ca.pem; \
        aws --endpoint-url https://localhost:9999 --region ap-southeast-2 $*"
}

# node_of <instance-id> — which node is the guest on right now
node_of() {
    on "$API_HOST" "sudo spx get instances --config /etc/spinifex/spinifex.toml 2>/dev/null" \
        | awk -v id="$1" '$0 ~ id {print}' | grep -oE 'spinifexnode0[0-9]' | head -1
}

# private_half_of <public-addr> — the OCI private address this EIP rides the
# wire as. Read off the OVN row that stamps the public address, which is the
# one place the pairing is recorded outside the bindings bucket. Guessing from
# the host's /32 routes would be ambiguous: a node hosting the guest carries the
# auto-assigned address's private half on the same VNIC.
private_half_of() {
    on "$API_HOST" "NB=\$(sudo grep -oP '^ovn_nb_addr\\s*=\\s*\"\\K[^\"]+' /etc/spinifex/spinifex.toml); \
        sudo ovn-nbctl --db=\"\$NB\" --bare --columns=external_ip \
        find NAT 'external_ids:spinifex\\:public_ip=\"$1\"'" 2>/dev/null | tr -d '\r' | head -1
}

# reserved_ip_ocid <public-addr> — the OCI public IP object behind an address,
# empty when OCI holds none. This is the object the tenancy is billed for, so
# it — not our own KV — is what answers whether a stop really gave it back.
reserved_ip_ocid() {
    "$OCI_BIN" --profile "$OCI_PROFILE" network public-ip get \
        --public-ip-address "$1" --query 'data.id' --raw-output 2>/dev/null || true
}

# state_of <instance-id> — the EC2 state name, for polling
state_of() {
    aws_spx "ec2 describe-instances --instance-ids $1 \
        --query 'Reservations[0].Instances[0].State.Name' --output text" 2>/dev/null | tr -d '\r'
}

# public_ip_of <instance-id> — what DescribeInstances reports, "None" when none
public_ip_of() {
    aws_spx "ec2 describe-instances --instance-ids $1 \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text" 2>/dev/null | tr -d '\r'
}

# is_state <instance-id> <state> — a predicate for wait_for
is_state() { [[ $(state_of "$1") == "$2" ]]; }

# vnic_of_private <addr> — the OCI VNIC OCID currently carrying a private IP
vnic_of_private() {
    "$OCI_BIN" --profile "$OCI_PROFILE" network private-ip list \
        --subnet-id "$SUBNET" --ip-address "$1" --query 'data[0]."vnic-id"' --raw-output 2>/dev/null
}

# external_vnic_of <node-name> — that node's br-wan (secondary) VNIC OCID
external_vnic_of() {
    on "${NODE_IP[$1]}" 'curl -s -H "Authorization: Bearer Oracle" -L \
        http://169.254.169.254/opc/v2/vnics/ | python3 -c "
import sys,json
v=json.load(sys.stdin)
print(v[1][\"vnicId\"] if len(v)>1 else \"\")"'
}

reachable() { ping -c4 -W2 "$1" >/dev/null 2>&1; }

wait_for() { # wait_for <seconds> <description> <cmd...>
    local secs=$1 desc=$2; shift 2
    local deadline=$(( SECONDS + secs ))
    while (( SECONDS < deadline )); do
        if "$@"; then ok "$desc"; return 0; fi
        sleep 5
    done
    fail "$desc (timed out after ${secs}s)"
    return 1
}

INSTANCE=${INSTANCE:-}
ALLOC=""
cleanup() {
    [[ -n $ALLOC ]] || return 0
    say "cleanup: releasing $ALLOC"
    local assoc
    assoc=$(aws_spx "ec2 describe-addresses --query \
        \"Addresses[?AllocationId=='$ALLOC'].AssociationId\" --output text" 2>/dev/null | tr -d '\r')
    [[ -n $assoc && $assoc != None ]] && aws_spx "ec2 disassociate-address --association-id $assoc" >/dev/null 2>&1
    sleep 5
    aws_spx "ec2 release-address --allocation-id $ALLOC" >/dev/null 2>&1 || true
}
trap cleanup EXIT

[[ -n $INSTANCE ]] || { echo "set INSTANCE=i-... (a running guest with an ENI)" >&2; exit 2; }

say "0. starting state"
ENI=$(aws_spx "ec2 describe-instances --instance-ids $INSTANCE \
    --query 'Reservations[0].Instances[0].NetworkInterfaces[0].NetworkInterfaceId' --output text" | tr -d '\r')
AUTO=$(public_ip_of "$INSTANCE")
echo "instance $INSTANCE on $(node_of "$INSTANCE"), eni $ENI, auto-assigned ${AUTO:-<none>}"

# An auto-assigned address is borrowed for as long as the guest runs. On OCI it
# is a reserved public IP object billed to the tenancy, so "released on stop" is
# a billing claim and OCI's own API is the only place it can be proved.
say "0b. an auto-assigned address goes back to OCI on stop"
if [[ -z $AUTO || $AUTO == None ]]; then
    echo "skipped: guest has no auto-assigned address"
else
    AUTO_OCID=$(reserved_ip_ocid "$AUTO")
    is_set "$AUTO_OCID" "OCI holds a public IP object for $AUTO while running"

    aws_spx "ec2 stop-instances --instance-ids $INSTANCE" >/dev/null
    wait_for 180 "instance stopped" is_state "$INSTANCE" stopped
    unset_or_none "$(public_ip_of "$INSTANCE")" "stopped instance reports no PublicIpAddress"
    unset_or_none "$(reserved_ip_ocid "$AUTO")" "OCI no longer bills a public IP for $AUTO"

    aws_spx "ec2 start-instances --instance-ids $INSTANCE" >/dev/null
    wait_for 300 "instance running again" is_state "$INSTANCE" running
    RESTARTED=$(public_ip_of "$INSTANCE")
    is_set "$RESTARTED" "restarted instance has an auto-assigned address"
    if [[ $RESTARTED == "$AUTO" ]]; then
        fail "restart returned the same address $AUTO — the stop did not release it"
    else
        ok "restart returned a different address ($AUTO -> $RESTARTED)"
    fi
    is_set "$(reserved_ip_ocid "$RESTARTED")" "OCI holds a public IP object for $RESTARTED"
fi

# After the restart above, so the move in step 4 is measured from where the
# guest actually is rather than from where it started the script.
START_NODE=$(node_of "$INSTANCE")
echo "instance now on $START_NODE"

say "1. allocate and associate an EIP"
ALLOC=$(aws_spx "ec2 allocate-address --query AllocationId --output text" | tr -d '\r')
EIP=$(aws_spx "ec2 describe-addresses --allocation-ids $ALLOC --query 'Addresses[0].PublicIp' --output text" | tr -d '\r')
aws_spx "ec2 associate-address --allocation-id $ALLOC --network-interface-id $ENI" >/dev/null
echo "allocated $ALLOC = $EIP"
wait_for 300 "EIP $EIP answers on $START_NODE" reachable "$EIP"

PRIVATE=$(private_half_of "$EIP")
echo "private half: ${PRIVATE:-<unresolved>}"
is_set "$PRIVATE" "the EIP resolves to an OCI private address"

say "2. DescribeInstances must report the EIP, not the auto-assigned address"
eq "$(public_ip_of "$INSTANCE")" "$EIP" "DescribeInstances reports the EIP"

say "3. stop the guest"
aws_spx "ec2 stop-instances --instance-ids $INSTANCE" >/dev/null
wait_for 180 "instance stopped" is_state "$INSTANCE" stopped

unset_or_none "$(public_ip_of "$INSTANCE")" "stopped instance reports no PublicIpAddress"

# The EIP's own object is the customer's and must outlive the stop — the exact
# opposite of what step 0b asserted for the borrowed address.
is_set "$(reserved_ip_ocid "$EIP")" "OCI still holds the reserved public IP for $EIP"

STILL=$(aws_spx "ec2 describe-addresses --allocation-ids $ALLOC --query 'Addresses[0].AssociationId' --output text" | tr -d '\r')
is_set "$STILL" "EIP stays associated across the stop"

say "4. force a start on a different node"
on "${NODE_IP[$START_NODE]}" 'sudo systemctl stop spinifex-daemon'
restore_daemon() { on "${NODE_IP[$START_NODE]}" 'sudo systemctl start spinifex-daemon' || true; }
trap 'restore_daemon; cleanup' EXIT

aws_spx "ec2 start-instances --instance-ids $INSTANCE" >/dev/null
sleep 20
NEW_NODE=$(node_of "$INSTANCE")
echo "now on $NEW_NODE (was $START_NODE)"
[[ $NEW_NODE != "$START_NODE" ]] || { fail "guest did not move; nothing to prove"; exit 1; }

say "5. the decisive checks"
wait_for 240 "EIP $EIP answers after the move to $NEW_NODE" reachable "$EIP"

if [[ -n ${PRIVATE:-} ]]; then
    WANT=$(external_vnic_of "$NEW_NODE"); GOT=$(vnic_of_private "$PRIVATE")
    eq "$GOT" "$WANT" "OCI carries $PRIVATE on $NEW_NODE's VNIC"
fi

eq "$(public_ip_of "$INSTANCE")" "$EIP" "still reports the EIP after the move"

say "6. restore"
restore_daemon
wait_for 120 "$START_NODE prunes its stale route for $PRIVATE" bash -c \
    "! timeout 60 ssh -i $KEY -o StrictHostKeyChecking=no ubuntu@${NODE_IP[$START_NODE]} \
      'ip route show dev spx-nat-host' | grep -q '${PRIVATE:-__none__}'"

(( FAILED )) && { echo; echo "RESULT: FAILED"; exit 1; }
echo; echo "RESULT: PASSED"
