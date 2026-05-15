#!/bin/bash
# run-reboot-e2e.sh — single-node host reboot resilience driver (cell #18).
#
# Runs from the GitHub Actions runner (NOT via ssh-and-run) so the script
# survives the systemctl-reboot-induced ssh disconnect mid-run. Each step that
# touches the cluster opens its own ssh session into the spinifex node.
#
# Required env:
#   WAN_IP   — single-node cluster's WAN IP
#   SSH_KEY  — path to ssh key the runner can use to reach tf-user@WAN_IP
#
# Sequence:
#   1. Pre-reboot: VPC + 2 app EC2s + internal ALB; verify round-robin.
#   2. Snapshot pre-reboot state (instance IPs, ALB ENI MAC, ovn-nbctl show).
#   3. systemctl reboot the host; poll TCP/22 from the runner until SSH returns.
#   4. Wait for spinifex services + AWS gateway to respond.
#   5. Post-reboot assertions (each a separate pass/fail line).
#   6. Cleanup via EXIT trap.

set -u

WAN_IP="${WAN_IP:?WAN_IP is required}"
SSH_KEY="${SSH_KEY:?SSH_KEY is required}"
SSH_KEY="${SSH_KEY/#\~/$HOME}"

REBOOT_WAIT_SECS="${REBOOT_WAIT_SECS:-300}"
DAEMON_READY_SECS="${DAEMON_READY_SECS:-180}"
INSTANCE_RUNNING_SECS="${INSTANCE_RUNNING_SECS:-120}"
LB_RECOVER_SECS="${LB_RECOVER_SECS:-180}"
LAUNCH_TIME_SECS="${LAUNCH_TIME_SECS:-30}"

NODE_KEY_PATH="\$HOME/reboot-e2e-key"
NODE_USERDATA_PATH="\$HOME/reboot-e2e-userdata.txt"

SSH_OPTS=(
    -i "$SSH_KEY"
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o ConnectTimeout=10
    -o LogLevel=ERROR
)

PASSED=0
FAILED=0

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { echo "  ✅ $1"; PASSED=$((PASSED + 1)); }
fail() { echo "  ❌ $1"; FAILED=$((FAILED + 1)); }

node_ssh() { ssh "${SSH_OPTS[@]}" "tf-user@${WAN_IP}" "$@"; }

# Pipe a file's contents into a path on the node (tf-user-owned).
put_to_node() {
    local dest="$1"
    node_ssh "cat > $dest"
}

# Run an aws cli call on the node with the spinifex profile already configured.
node_aws_ec2()   { node_ssh "AWS_PROFILE=spinifex aws ec2 $*"; }
node_aws_elbv2() { node_ssh "AWS_PROFILE=spinifex aws elbv2 $*"; }

# ==========================================================================
# Resource tracking for cleanup
# ==========================================================================
VPC_ID=""
SUBNET_ID=""
IGW_ID=""
APP_INSTANCE_IDS=()
ALB_LISTENER_ARN=""
ALB_LB_ARN=""
ALB_TG_ARN=""
E2E_SG_ID=""

cleanup() {
    local exit_code=$?
    echo ""
    echo "=== Cleanup ==="

    if ! node_ssh "true" 2>/dev/null; then
        log "Cleanup skipped: node unreachable (exit=$exit_code)"
        echo "Reboot E2E: $PASSED passed, $FAILED failed"
        if [ $FAILED -gt 0 ] || [ $exit_code -ne 0 ]; then exit 1; fi
        exit 0
    fi

    [ -n "$ALB_LISTENER_ARN" ] && node_aws_elbv2 "delete-listener --listener-arn $ALB_LISTENER_ARN" >/dev/null 2>&1 || true
    [ -n "$ALB_LB_ARN" ] && node_aws_elbv2 "delete-load-balancer --load-balancer-arn $ALB_LB_ARN" >/dev/null 2>&1 || true
    [ -n "$ALB_TG_ARN" ] && node_aws_elbv2 "delete-target-group --target-group-arn $ALB_TG_ARN" >/dev/null 2>&1 || true
    # SG can't be deleted while ENIs reference it; delete after instances + ALB are gone (below).

    for inst_id in "${APP_INSTANCE_IDS[@]}"; do
        [ -n "$inst_id" ] && node_aws_ec2 "terminate-instances --instance-ids $inst_id" >/dev/null 2>&1 || true
    done
    if [ ${#APP_INSTANCE_IDS[@]} -gt 0 ]; then
        for attempt in $(seq 1 30); do
            local all_terminated=true
            for inst_id in "${APP_INSTANCE_IDS[@]}"; do
                [ -z "$inst_id" ] && continue
                local state
                state=$(node_aws_ec2 "describe-instances --instance-ids $inst_id --query 'Reservations[0].Instances[0].State.Name' --output text" 2>/dev/null || echo "terminated")
                if [ "$state" != "terminated" ]; then all_terminated=false; break; fi
            done
            if [ "$all_terminated" = true ]; then break; fi
            sleep 2
        done
    fi

    node_aws_ec2 "delete-key-pair --key-name reboot-e2e-key" >/dev/null 2>&1 || true
    node_ssh "rm -f $NODE_KEY_PATH $NODE_USERDATA_PATH" >/dev/null 2>&1 || true

    # SG must come after instances + ALB ENIs are gone (it can't be deleted while referenced).
    [ -n "$E2E_SG_ID" ] && node_aws_ec2 "delete-security-group --group-id $E2E_SG_ID" >/dev/null 2>&1 || true

    if [ -n "$IGW_ID" ] && [ -n "$VPC_ID" ]; then
        node_aws_ec2 "detach-internet-gateway --internet-gateway-id $IGW_ID --vpc-id $VPC_ID" >/dev/null 2>&1 || true
        node_aws_ec2 "delete-internet-gateway --internet-gateway-id $IGW_ID" >/dev/null 2>&1 || true
    fi
    [ -n "$SUBNET_ID" ] && node_aws_ec2 "delete-subnet --subnet-id $SUBNET_ID" >/dev/null 2>&1 || true
    [ -n "$VPC_ID" ] && node_aws_ec2 "delete-vpc --vpc-id $VPC_ID" >/dev/null 2>&1 || true

    echo ""
    echo "================================="
    echo "Reboot E2E: $PASSED passed, $FAILED failed"
    echo "================================="

    if [ $FAILED -gt 0 ]; then exit 1; fi
    exit $exit_code
}
trap cleanup EXIT

dump_diagnostics() {
    # Window from reboot onwards so recovery failure logs aren't scrolled out
    # by NATS warm-up / per-instance state-transition spam.
    local mode label
    if [ -n "${REBOOT_START:-}" ]; then
        mode="--since=@${REBOOT_START}"
        label="since reboot (@${REBOOT_START})"
    else
        mode="-n 200"
        label="last 200 lines each"
    fi
    log "--- spinifex service journals (${label}) ---"
    for svc in spinifex-nats spinifex-predastore spinifex-viperblock \
               spinifex-daemon spinifex-awsgw spinifex-vpcd \
               ovn-controller ovs-vswitchd; do
        echo "=== ${svc} ==="
        node_ssh "sudo journalctl -u ${svc} --no-pager ${mode} 2>/dev/null || true"
    done
}

# ==========================================================================
# HTTP traffic burst from the spinifex node against an ALB URL.
# ==========================================================================
run_http_burst() {
    local url="$1" label="$2" num="${3:-20}"
    log "Sending $num HTTP requests to $url ($label)..."
    local results
    results=$(node_ssh "for i in \$(seq 1 $num); do curl -s --max-time 5 '$url/' 2>/dev/null; echo; done" 2>/dev/null || true)

    declare -A counts=()
    local total_ok=0
    while IFS= read -r line; do
        local inst
        inst=$(echo "$line" | jq -r '.instance_id // empty' 2>/dev/null)
        if [ -n "$inst" ]; then
            counts[$inst]=$(( ${counts[$inst]:-0} + 1 ))
            total_ok=$((total_ok + 1))
        fi
    done <<< "$results"

    local unique=${#counts[@]}
    echo "  Distribution:"
    for inst_id in "${!counts[@]}"; do echo "    $inst_id: ${counts[$inst_id]} responses"; done

    if [ "$unique" -ge 2 ]; then
        pass "$label round-robin: $unique unique instances"
    else
        fail "$label round-robin: expected >=2 unique responders, got $unique"
    fi
    if [ "$total_ok" -ge $((num / 2)) ]; then
        pass "$label success rate: $total_ok/$num"
    else
        fail "$label success rate: only $total_ok/$num"
    fi
}

wait_for_lb_active() {
    local lb_arn="$1" label="$2" timeout="${3:-270}"
    log "Waiting for $label active (up to ${timeout}s)..."
    local state=""
    for attempt in $(seq 1 $((timeout / 3))); do
        state=$(node_aws_elbv2 "describe-load-balancers --load-balancer-arns $lb_arn --query 'LoadBalancers[0].State.Code' --output text" 2>/dev/null || true)
        if [ "$state" = "active" ]; then
            pass "$label active"
            return 0
        fi
        sleep 3
    done
    fail "$label did not reach active (last state: ${state:-unknown})"
    return 1
}

wait_for_targets_healthy() {
    local tg_arn="$1" expected="$2" label="$3" timeout="${4:-180}"
    log "Waiting for $label targets healthy (up to ${timeout}s)..."
    local healthy_count=0 total_count=0
    for attempt in $(seq 1 $((timeout / 5))); do
        local out
        out=$(node_aws_elbv2 "describe-target-health --target-group-arn $tg_arn --output json" 2>/dev/null || echo '{}')
        healthy_count=$(echo "$out" | jq '[.TargetHealthDescriptions[] | select(.TargetHealth.State == "healthy")] | length' 2>/dev/null || echo 0)
        total_count=$(echo "$out" | jq '.TargetHealthDescriptions | length' 2>/dev/null || echo 0)
        if [ "$healthy_count" -eq "$expected" ]; then
            pass "$label: $expected/$expected targets healthy"
            return 0
        fi
        sleep 5
    done
    fail "$label: only $healthy_count/$total_count healthy after ${timeout}s"
    return 1
}

# ==========================================================================
# Phase 0: Prerequisites
# ==========================================================================
echo "================================="
echo "Reboot Resilience E2E (cell 18)"
echo "================================="
log "WAN_IP: $WAN_IP"

log "Verifying SSH to node..."
node_ssh "hostname" >/dev/null || { fail "ssh to $WAN_IP"; exit 1; }
pass "ssh to $WAN_IP"

log "Discovering instance type + AMI..."
INSTANCE_TYPE=$(node_aws_ec2 "describe-instance-types --query 'InstanceTypes[*].InstanceType' --output text" \
    | tr ' \t' '\n\n' | grep -m1 'nano' || true)
if [ -z "$INSTANCE_TYPE" ]; then fail "no nano instance type available"; exit 1; fi
pass "instance type: $INSTANCE_TYPE"

ALL_IMAGES=$(node_aws_ec2 "describe-images --output json" 2>/dev/null)
AMI_ID=$(echo "$ALL_IMAGES" | jq -r '[.Images[] | select(.Name | test("^ami-ubuntu"))][0].ImageId // empty')
if [ -z "$AMI_ID" ]; then
    AMI_ID=$(echo "$ALL_IMAGES" | jq -r '[.Images[] | select(.Name | test("alpine") | not)][0].ImageId // empty')
fi
if [ -z "$AMI_ID" ]; then AMI_ID=$(echo "$ALL_IMAGES" | jq -r '.Images[0].ImageId // empty'); fi
if [ -z "$AMI_ID" ]; then fail "no AMI found"; exit 1; fi
pass "AMI: $AMI_ID"

# Create key pair, capturing the private key material so we can ssh into guests
node_aws_ec2 "delete-key-pair --key-name reboot-e2e-key" >/dev/null 2>&1 || true
KEY_RESPONSE=$(node_aws_ec2 "create-key-pair --key-name reboot-e2e-key --output json" 2>/dev/null) \
    || { fail "create-key-pair"; exit 1; }
KEY_MATERIAL=$(echo "$KEY_RESPONSE" | jq -r '.KeyMaterial')
if [ -z "$KEY_MATERIAL" ] || [ "$KEY_MATERIAL" = "null" ]; then
    fail "create-key-pair returned no KeyMaterial"; exit 1
fi
printf '%s' "$KEY_MATERIAL" | put_to_node "$NODE_KEY_PATH"
node_ssh "chmod 600 $NODE_KEY_PATH" || { fail "chmod key"; exit 1; }
pass "key pair: reboot-e2e-key"

# Stage user-data on the node (HTTP responder for round-robin verification)
APP_USER_DATA=$(cat <<'USERDATA'
#!/bin/bash
INSTANCE_ID=$(hostname)
mkdir -p /tmp/httpd && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &
USERDATA
)
printf '%s' "$APP_USER_DATA" | put_to_node "$NODE_USERDATA_PATH"

# ==========================================================================
# Phase 1: VPC + Subnet
# ==========================================================================
log "Creating VPC..."
VPC_ID=$(node_aws_ec2 "create-vpc --cidr-block 10.210.0.0/16 --output json" | jq -r '.Vpc.VpcId')
if [ -z "$VPC_ID" ] || [ "$VPC_ID" = "null" ]; then fail "create-vpc"; exit 1; fi
pass "create-vpc: $VPC_ID"

IGW_ID=$(node_aws_ec2 "create-internet-gateway --output json" | jq -r '.InternetGateway.InternetGatewayId')
node_aws_ec2 "attach-internet-gateway --internet-gateway-id $IGW_ID --vpc-id $VPC_ID" >/dev/null \
    || { fail "attach-internet-gateway"; exit 1; }
pass "internet gateway: $IGW_ID (attached)"

SUBNET_ID=$(node_aws_ec2 "create-subnet --vpc-id $VPC_ID --cidr-block 10.210.1.0/24 --output json" | jq -r '.Subnet.SubnetId')
if [ -z "$SUBNET_ID" ] || [ "$SUBNET_ID" = "null" ]; then fail "create-subnet"; exit 1; fi
pass "create-subnet: $SUBNET_ID"
node_aws_ec2 "modify-subnet-attribute --subnet-id $SUBNET_ID --map-public-ip-on-launch" >/dev/null || true

log "Creating shared security group (port 80 inbound)..."
E2E_SG_ID=$(node_aws_ec2 "create-security-group \
    --group-name reboot-e2e-sg \
    --description 'Reboot E2E shared SG (ALB + app instances)' \
    --vpc-id $VPC_ID \
    --output json" | jq -r '.GroupId')
if [ -z "$E2E_SG_ID" ] || [ "$E2E_SG_ID" = "null" ]; then fail "create-security-group"; exit 1; fi
node_aws_ec2 "authorize-security-group-ingress \
    --group-id $E2E_SG_ID \
    --protocol tcp --port 80 --cidr 0.0.0.0/0" >/dev/null \
    || { fail "authorize-security-group-ingress port 80"; exit 1; }
pass "security group: $E2E_SG_ID"

# ==========================================================================
# Phase 2: Launch app EC2 instances
# ==========================================================================
log "Launching 2 app instances..."
for i in 1 2; do
    INST=$(node_aws_ec2 "run-instances \
        --image-id $AMI_ID \
        --instance-type $INSTANCE_TYPE \
        --key-name reboot-e2e-key \
        --subnet-id $SUBNET_ID \
        --security-group-ids $E2E_SG_ID \
        --user-data file://$NODE_USERDATA_PATH \
        --output json" | jq -r '.Instances[0].InstanceId')
    if [ -z "$INST" ] || [ "$INST" = "null" ]; then fail "run-instances (app $i)"; exit 1; fi
    APP_INSTANCE_IDS+=("$INST")
    log "  app$i: $INST"
done
pass "launched ${#APP_INSTANCE_IDS[@]} app instances"

log "Waiting for instances to reach running..."
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    for attempt in $(seq 1 60); do
        STATE=$(node_aws_ec2 "describe-instances --instance-ids $inst_id --query 'Reservations[0].Instances[0].State.Name' --output text" 2>/dev/null)
        if [ "$STATE" = "running" ]; then break; fi
        if [ $attempt -eq 60 ]; then fail "instance $inst_id stuck in $STATE"; exit 1; fi
        sleep 2
    done
done
pass "all instances running"

declare -A PRE_INSTANCE_IPS
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    IP=$(node_aws_ec2 "describe-instances --instance-ids $inst_id --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text" 2>/dev/null)
    if [ -z "$IP" ] || [ "$IP" = "None" ]; then fail "$inst_id has no PrivateIpAddress"; exit 1; fi
    PRE_INSTANCE_IPS[$inst_id]="$IP"
    log "  $inst_id -> $IP"
done
pass "all instances have private IPs"

# ==========================================================================
# Phase 3: ALB (internet-facing)
# ==========================================================================
log "Creating HTTP target group..."
ALB_TG_ARN=$(node_aws_elbv2 "create-target-group \
    --name reboot-e2e-tg \
    --protocol HTTP --port 80 \
    --vpc-id $VPC_ID \
    --health-check-path /index.html \
    --health-check-interval-seconds 5 \
    --healthy-threshold-count 2 \
    --unhealthy-threshold-count 2 \
    --output json" | jq -r '.TargetGroups[0].TargetGroupArn')
if [ -z "$ALB_TG_ARN" ] || [ "$ALB_TG_ARN" = "null" ]; then fail "create-target-group"; exit 1; fi
pass "target group: $ALB_TG_ARN"

node_aws_elbv2 "register-targets --target-group-arn $ALB_TG_ARN \
    --targets Id=${APP_INSTANCE_IDS[0]} Id=${APP_INSTANCE_IDS[1]}" >/dev/null \
    || { fail "register-targets"; exit 1; }
pass "registered 2 targets"

log "Creating internet-facing ALB..."
ALB_LB_ARN=$(node_aws_elbv2 "create-load-balancer \
    --name reboot-e2e-alb \
    --scheme internet-facing \
    --subnets $SUBNET_ID \
    --security-groups $E2E_SG_ID \
    --output json" | jq -r '.LoadBalancers[0].LoadBalancerArn')
if [ -z "$ALB_LB_ARN" ] || [ "$ALB_LB_ARN" = "null" ]; then fail "create-load-balancer"; exit 1; fi
pass "ALB: $ALB_LB_ARN"

ALB_LB_ID=$(echo "$ALB_LB_ARN" | sed 's|.*/||')

ALB_LISTENER_ARN=$(node_aws_elbv2 "create-listener \
    --load-balancer-arn $ALB_LB_ARN \
    --protocol HTTP --port 80 \
    --default-actions Type=forward,TargetGroupArn=$ALB_TG_ARN \
    --output json" | jq -r '.Listeners[0].ListenerArn')
if [ -z "$ALB_LISTENER_ARN" ] || [ "$ALB_LISTENER_ARN" = "null" ]; then fail "create-listener"; exit 1; fi
pass "listener: $ALB_LISTENER_ARN"

wait_for_lb_active "$ALB_LB_ARN" "ALB" || { dump_diagnostics; exit 1; }

ENI_OUT=$(node_aws_ec2 "describe-network-interfaces \
    --filters Name=description,Values='ELB app/reboot-e2e-alb/${ALB_LB_ID}' \
    --output json" 2>/dev/null)
ALB_PUBLIC_IP=$(echo "$ENI_OUT" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty')
ALB_ENI_ID=$(echo "$ENI_OUT" | jq -r '.NetworkInterfaces[0].NetworkInterfaceId // empty')
ALB_ENI_MAC=$(echo "$ENI_OUT" | jq -r '.NetworkInterfaces[0].MacAddress // empty')
if [ -z "$ALB_PUBLIC_IP" ] || [ "$ALB_PUBLIC_IP" = "null" ]; then fail "ALB has no public IP"; exit 1; fi
pass "ALB ENI $ALB_ENI_ID public IP: $ALB_PUBLIC_IP"

wait_for_targets_healthy "$ALB_TG_ARN" 2 "ALB" || { dump_diagnostics; exit 1; }

# ==========================================================================
# Phase 4: Pre-reboot traffic burst
# ==========================================================================
echo ""
echo "=== Phase 4: Pre-reboot traffic ==="
run_http_burst "http://${ALB_PUBLIC_IP}:80" "ALB pre-reboot"

# ==========================================================================
# Phase 5: Snapshot pre-reboot state
# ==========================================================================
echo ""
echo "=== Phase 5: Snapshot pre-reboot state ==="
PRE_OVN=$(node_ssh "sudo ovn-nbctl show 2>/dev/null" || echo "")
PRE_OVN_LS_COUNT=$(echo "$PRE_OVN" | grep -c '^switch ' || true)
PRE_OVN_PORT_COUNT=$(echo "$PRE_OVN" | grep -c '^    port ' || true)
log "ovn-nbctl: ${PRE_OVN_LS_COUNT} logical switches, ${PRE_OVN_PORT_COUNT} ports"
log "ALB ENI MAC: ${ALB_ENI_MAC:-<unknown>}"
pass "pre-reboot state snapshot captured"

# ==========================================================================
# Phase 6: Reboot the host
# ==========================================================================
echo ""
echo "=== Phase 6: systemctl reboot ==="
REBOOT_START=$(date +%s)
log "Issuing reboot via ssh (connection will drop)..."
node_ssh "sudo systemctl reboot" >/dev/null 2>&1 || true

# Give the host a moment to actually start shutting down before we begin polling
sleep 5

log "Polling TCP/22 (timeout ${REBOOT_WAIT_SECS}s)..."
HOST_UP=false
for attempt in $(seq 1 $((REBOOT_WAIT_SECS / 5))); do
    if timeout 5 bash -c "</dev/tcp/${WAN_IP}/22" 2>/dev/null; then
        if node_ssh "true" 2>/dev/null; then
            HOST_UP=true
            break
        fi
    fi
    sleep 5
done
REBOOT_ELAPSED=$(( $(date +%s) - REBOOT_START ))
if [ "$HOST_UP" = true ]; then
    pass "host SSH back after ${REBOOT_ELAPSED}s"
else
    fail "host SSH did not come back within ${REBOOT_WAIT_SECS}s"
    exit 1
fi

# ==========================================================================
# Phase 7: Wait for spinifex services + AWS gateway readiness
# ==========================================================================
echo ""
echo "=== Phase 7: Wait for spinifex readiness ==="
log "Service status:"
for svc in spinifex-daemon spinifex-predastore spinifex-viperblock \
           spinifex-nats spinifex-awsgw spinifex-vpcd \
           ovn-controller ovs-vswitchd ovn-northd; do
    state=$(node_ssh "systemctl is-active $svc" 2>/dev/null || echo "n/a")
    log "  $svc: $state"
done

log "Polling describe-instance-types (timeout ${DAEMON_READY_SECS}s)..."
DAEMON_OK=false
for attempt in $(seq 1 $((DAEMON_READY_SECS / 5))); do
    if node_aws_ec2 "describe-instance-types --query 'InstanceTypes[0].InstanceType' --output text" >/dev/null 2>&1; then
        DAEMON_OK=true
        break
    fi
    sleep 5
done
if [ "$DAEMON_OK" = true ]; then
    pass "AWS gateway responding post-reboot"
else
    fail "AWS gateway did not respond within ${DAEMON_READY_SECS}s"
    dump_diagnostics
    exit 1
fi

# ==========================================================================
# Phase 8: Post-reboot assertions
# ==========================================================================
echo ""
echo "=== Phase 8: Post-reboot assertions ==="

# 8.1: instances back to running with same IDs
log "Polling instance state (timeout ${INSTANCE_RUNNING_SECS}s)..."
ALL_RUNNING=false
for attempt in $(seq 1 $((INSTANCE_RUNNING_SECS / 5))); do
    all=true
    for inst_id in "${APP_INSTANCE_IDS[@]}"; do
        STATE=$(node_aws_ec2 "describe-instances --instance-ids $inst_id --query 'Reservations[0].Instances[0].State.Name' --output text" 2>/dev/null || echo "missing")
        if [ "$STATE" != "running" ]; then all=false; break; fi
    done
    if [ "$all" = true ]; then ALL_RUNNING=true; break; fi
    sleep 5
done
if [ "$ALL_RUNNING" = true ]; then
    pass "all instances back to running"
else
    fail "instances did not all reach running within ${INSTANCE_RUNNING_SECS}s"
fi

# 8.2: private IPs preserved (IPAM persistence)
log "Verifying private IP persistence..."
IPS_PRESERVED=true
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    POST_IP=$(node_aws_ec2 "describe-instances --instance-ids $inst_id --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text" 2>/dev/null)
    if [ "$POST_IP" != "${PRE_INSTANCE_IPS[$inst_id]}" ]; then
        fail "$inst_id IP drift: pre=${PRE_INSTANCE_IPS[$inst_id]} post=$POST_IP"
        IPS_PRESERVED=false
    fi
done
if [ "$IPS_PRESERVED" = true ]; then
    pass "private IPs preserved across reboot"
fi

# 8.3: guest VMs relaunched post-reboot (LaunchTime > reboot time via describe-instances)
# Two-hop SSH to VPC private IPs is not possible in single-node macvlan mode —
# the host has no route to VPC subnets. LaunchTime from the daemon's recovery
# path (reset to time.Now() on relaunch) is a reliable reboot signal.
log "Verifying guest VMs relaunched post-reboot (LaunchTime check)..."
GUESTS_RELAUNCHED=true
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    LAUNCH_TS=$(node_aws_ec2 "describe-instances --instance-ids $inst_id \
        --query 'Reservations[0].Instances[0].LaunchTime' \
        --output text" 2>/dev/null || echo "")
    if [ -z "$LAUNCH_TS" ] || [ "$LAUNCH_TS" = "None" ]; then
        fail "$inst_id: could not retrieve LaunchTime"
        GUESTS_RELAUNCHED=false
        continue
    fi
    LAUNCH_EPOCH=$(date -d "$LAUNCH_TS" +%s 2>/dev/null || echo "0")
    if [ "$LAUNCH_EPOCH" -ge "$REBOOT_START" ]; then
        log "  $inst_id: LaunchTime=${LAUNCH_TS} ✓"
    else
        fail "$inst_id: LaunchTime=${LAUNCH_TS} predates reboot — VM not relaunched"
        GUESTS_RELAUNCHED=false
    fi
done
if [ "$GUESTS_RELAUNCHED" = true ]; then
    pass "all instances relaunched post-reboot"
fi

# 8.4: ALB back to active
wait_for_lb_active "$ALB_LB_ARN" "ALB post-reboot" "$LB_RECOVER_SECS" || true

# 8.5: targets healthy again
wait_for_targets_healthy "$ALB_TG_ARN" 2 "ALB post-reboot" "$LB_RECOVER_SECS" || true

# 8.6: re-run the traffic burst
# Diagnose the post-reboot 0/20 burst BEFORE running it. run_http_burst parses
# instance_id from a JSON body and counts anything else (502/503, timeout,
# refused) as 0, so we can't tell L2-fail from L7-fail. Probe the EIP with
# verbose curl + a brief br-wan tcpdump + ip neigh, then run a histogram of
# HTTP status codes alongside the burst.
echo ""
log "Pre-burst connectivity probe for ALB EIP ${ALB_PUBLIC_IP}..."
node_ssh "sudo ip neigh flush ${ALB_PUBLIC_IP} 2>/dev/null || true"
TCPDUMP_PID_OUT=$(node_ssh "sudo timeout 6 tcpdump -i br-wan -nn -c 30 'host ${ALB_PUBLIC_IP} or (arp and host ${ALB_PUBLIC_IP})' >/tmp/probe-tcpdump.log 2>&1 & echo \$!" || true)
sleep 1
PROBE_OUT=$(node_ssh "curl -sv --connect-timeout 3 --max-time 5 -o /tmp/probe-body.txt -w 'HTTP=%{http_code} CONNECT=%{time_connect} TOTAL=%{time_total} EXIT_AFTER\n' 'http://${ALB_PUBLIC_IP}:80/' 2>&1; echo CURL_EXIT=\$?" || true)
log "curl -v probe result:"
printf '%s\n' "$PROBE_OUT" | sed 's/^/    /'
log "probe response body (head):"
node_ssh "head -c 400 /tmp/probe-body.txt 2>/dev/null; echo" | sed 's/^/    /' || true
sleep 5
log "tcpdump on br-wan during probe:"
node_ssh "cat /tmp/probe-tcpdump.log 2>/dev/null" | sed 's/^/    /' || true
log "ip neigh for EIP after probe:"
node_ssh "ip neigh show ${ALB_PUBLIC_IP} 2>/dev/null; ip neigh show dev br-wan 2>/dev/null | head -10" | sed 's/^/    /' || true

log "Re-running traffic burst against same ALB (with HTTP status histogram)..."
# Run a parallel curl that captures just status codes so we can distinguish
# "ALB returned 5xx" from "connect timeout / refused".
STATUS_HIST=$(node_ssh "for i in \$(seq 1 20); do curl -s -o /dev/null -w '%{http_code} %{exitcode}\n' --connect-timeout 3 --max-time 5 'http://${ALB_PUBLIC_IP}:80/' 2>/dev/null; done | sort | uniq -c" 2>/dev/null || true)
log "HTTP status / curl exit histogram (20 reqs):"
printf '%s\n' "$STATUS_HIST" | sed 's/^/    /'

PRE_BURST_FAILED=$FAILED
run_http_burst "http://${ALB_PUBLIC_IP}:80" "ALB post-reboot"
if [ "$FAILED" -gt "$PRE_BURST_FAILED" ]; then
    log "Post-reboot traffic failed — dumping journals from reboot onwards before cleanup overwrites them."
    dump_diagnostics
fi

# 8.7: ovn-nbctl diff (this is where the known persistence bug lands)
log "Diffing ovn-nbctl show against pre-reboot snapshot..."
POST_OVN=$(node_ssh "sudo ovn-nbctl show 2>/dev/null" || echo "")
POST_OVN_LS_COUNT=$(echo "$POST_OVN" | grep -c '^switch ' || true)
POST_OVN_PORT_COUNT=$(echo "$POST_OVN" | grep -c '^    port ' || true)
log "ovn-nbctl post: ${POST_OVN_LS_COUNT} logical switches, ${POST_OVN_PORT_COUNT} ports (pre: ${PRE_OVN_LS_COUNT} / ${PRE_OVN_PORT_COUNT})"
if [ "$POST_OVN_LS_COUNT" -eq "$PRE_OVN_LS_COUNT" ] && [ "$POST_OVN_PORT_COUNT" -eq "$PRE_OVN_PORT_COUNT" ]; then
    pass "ovn-nbctl logical-switch + port counts match pre-reboot"
else
    fail "ovn-nbctl drift: switches ${PRE_OVN_LS_COUNT}->${POST_OVN_LS_COUNT}, ports ${PRE_OVN_PORT_COUNT}->${POST_OVN_PORT_COUNT}"
fi

# Cleanup runs via the EXIT trap.
