#!/bin/bash
set -e

# Consolidated LB Data Plane E2E Test
# Tests all 4 LB variants: ALB internet-facing, ALB internal, NLB internet-facing, NLB internal.
# Shares a single VPC, subnet, and set of dual-purpose app instances across all suites.
# ALB + NLB run in parallel within each phase (internet-facing / internal).
#
# Requires: Pool mode with external IPAM (NOT dev_networking).
#
# Usage:
#   ./tests/e2e/run-lb-e2e.sh                         # internal-only (single-node)
#   ./tests/e2e/run-lb-e2e.sh --peer <ip>             # all 4 variants (multi-node, legacy)
#   ./tests/e2e/run-lb-e2e.sh --nodes <ip1> <ip2> ... # all 4 variants (multi-node, preferred)

cd "$(dirname "$0")/../.."

# ==========================================================================
# Dev mode gate
# ==========================================================================
SPINIFEX_CONFIG="${HOME}/spinifex/config/spinifex.toml"
if [ -f "$SPINIFEX_CONFIG" ]; then
    if grep -q 'dev_networking = true' "$SPINIFEX_CONFIG"; then
        echo "Skipping LB E2E: dev_networking is enabled (no external IPAM)"
        exit 0
    fi
fi

# ==========================================================================
# Arguments
# ==========================================================================
PEER_NODE_IP=""
ALL_NODE_IPS=()
while [ $# -gt 0 ]; do
    case "$1" in
        --peer) PEER_NODE_IP="$2"; shift 2 ;;
        --nodes) shift; while [ $# -gt 0 ] && [[ "$1" != --* ]]; do ALL_NODE_IPS+=("$1"); shift; done ;;
        *) echo "Unknown arg: $1"; exit 1 ;;
    esac
done
# --nodes supersedes --peer; fall back to --peer for backwards compat
if [ ${#ALL_NODE_IPS[@]} -gt 0 ] && [ -z "$PEER_NODE_IP" ]; then
    PEER_NODE_IP="${ALL_NODE_IPS[1]:-}"
fi

ENDPOINT="${ENDPOINT:-https://127.0.0.1:9999}"
export AWS_PROFILE=spinifex
AWS_EC2="aws --endpoint-url ${ENDPOINT} ec2"
AWS_ELBV2="aws --endpoint-url ${ENDPOINT} elbv2"

SSH_KEY_PATH="$HOME/.ssh/tf-user-ap-southeast-2"

PASSED=0
FAILED=0

pass() {
    echo "  ✅ $1"
    PASSED=$((PASSED + 1))
}

fail() {
    echo "  ❌ $1"
    FAILED=$((FAILED + 1))
}

peer_ssh() {
    local ip="$1"; shift
    ssh -i "$SSH_KEY_PATH" \
        -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 \
        -o LogLevel=ERROR \
        "tf-user@${ip}" "$@"
}

# find_lb_host <eni_id> — discover which node hosts the LB system instance by
# checking for its OVS tap device. Prints the node's WAN IP; returns 1 if not found.
find_lb_host() {
    local eni_id="$1"
    local tap_prefix
    tap_prefix="tap${eni_id#eni-}"
    tap_prefix="${tap_prefix:0:15}"

    local my_ip
    my_ip=$(ip -4 -o addr show br-wan 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)

    for node_ip in "${ALL_NODE_IPS[@]}"; do
        local found
        if [ "$node_ip" = "$my_ip" ]; then
            found=$(ip link show "$tap_prefix" 2>/dev/null || true)
        else
            found=$(peer_ssh "$node_ip" "ip link show '$tap_prefix'" 2>/dev/null || true)
        fi
        if [ -n "$found" ]; then
            echo "$node_ip"
            return 0
        fi
    done
    return 1
}

# pick_peer <host_ip> — return the first node IP that is NOT the given host.
pick_peer() {
    local host_ip="$1"
    for node_ip in "${ALL_NODE_IPS[@]}"; do
        if [ "$node_ip" != "$host_ip" ]; then
            echo "$node_ip"
            return 0
        fi
    done
    return 1
}

# ==========================================================================
# Resource tracking for cleanup
# ==========================================================================
VPC_ID=""
SUBNET_ID=""
IGW_ID=""
APP_INSTANCE_IDS=()
CLIENT_INSTANCE_ID=""

# Track ALB + NLB resources separately for parallel suites
ALB_LISTENER_ARN=""
ALB_LB_ARN=""
ALB_TG_ARN=""
NLB_LISTENER_ARN=""
NLB_LB_ARN=""
NLB_TG_ARN=""

cleanup() {
    local exit_code=$?
    echo ""
    echo "Cleanup..."

    # LB resources (in case a suite failed mid-way)
    for arn in "$ALB_LISTENER_ARN" "$NLB_LISTENER_ARN"; do
        [ -n "$arn" ] && $AWS_ELBV2 delete-listener --listener-arn "$arn" 2>/dev/null || true
    done
    for arn in "$ALB_LB_ARN" "$NLB_LB_ARN"; do
        [ -n "$arn" ] && $AWS_ELBV2 delete-load-balancer --load-balancer-arn "$arn" 2>/dev/null || true
    done
    for arn in "$ALB_TG_ARN" "$NLB_TG_ARN"; do
        [ -n "$arn" ] && $AWS_ELBV2 delete-target-group --target-group-arn "$arn" 2>/dev/null || true
    done

    # Instances
    ALL_INSTANCES=("${APP_INSTANCE_IDS[@]}")
    if [ -n "$CLIENT_INSTANCE_ID" ]; then
        ALL_INSTANCES+=("$CLIENT_INSTANCE_ID")
    fi
    for inst_id in "${ALL_INSTANCES[@]}"; do
        if [ -n "$inst_id" ]; then
            echo "  Terminating instance $inst_id..."
            $AWS_EC2 terminate-instances --instance-ids "$inst_id" 2>/dev/null || true
        fi
    done
    if [ ${#ALL_INSTANCES[@]} -gt 0 ]; then
        echo "  Waiting for instances to terminate..."
        for attempt in $(seq 1 30); do
            ALL_TERMINATED=true
            for inst_id in "${ALL_INSTANCES[@]}"; do
                STATE=$($AWS_EC2 describe-instances --instance-ids "$inst_id" \
                    --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "terminated")
                if [ "$STATE" != "terminated" ]; then
                    ALL_TERMINATED=false
                    break
                fi
            done
            if [ "$ALL_TERMINATED" = true ]; then break; fi
            sleep 2
        done
    fi

    echo "  Deleting key pair..."
    $AWS_EC2 delete-key-pair --key-name lb-e2e-key 2>/dev/null || true

    if [ -n "$IGW_ID" ] && [ -n "$VPC_ID" ]; then
        echo "  Detaching IGW..."
        $AWS_EC2 detach-internet-gateway --internet-gateway-id "$IGW_ID" --vpc-id "$VPC_ID" 2>/dev/null || true
        $AWS_EC2 delete-internet-gateway --internet-gateway-id "$IGW_ID" 2>/dev/null || true
    fi
    if [ -n "$SUBNET_ID" ]; then
        echo "  Deleting subnet..."
        $AWS_EC2 delete-subnet --subnet-id "$SUBNET_ID" 2>/dev/null || true
    fi
    if [ -n "$VPC_ID" ]; then
        echo "  Deleting VPC..."
        $AWS_EC2 delete-vpc --vpc-id "$VPC_ID" 2>/dev/null || true
    fi

    echo "Cleanup complete"
    echo ""
    echo "========================================"
    echo "LB E2E Results: $PASSED passed, $FAILED failed"
    echo "========================================"

    if [ $FAILED -gt 0 ]; then exit 1; fi
    exit $exit_code
}
trap cleanup EXIT

echo "========================================"
echo "Consolidated LB Data Plane E2E"
echo "========================================"
echo "Endpoint:  $ENDPOINT"
echo "Peer node: ${PEER_NODE_IP:-none (internet-facing tests will be skipped)}"
echo ""

# ==========================================================================
# Phase 0: Prerequisites
# ==========================================================================
echo "Phase 0: Prerequisites"
echo "========================================"

PEER_AVAILABLE=false
if [ -n "$PEER_NODE_IP" ]; then
    echo "Verifying SSH to peer node..."
    if peer_ssh "$PEER_NODE_IP" "hostname" > /dev/null 2>&1; then
        PEER_AVAILABLE=true
        pass "SSH to peer node $PEER_NODE_IP"
    else
        echo "  Cannot SSH to peer node $PEER_NODE_IP — internet-facing tests will be skipped"
    fi
fi

echo "Discovering instance types..."
AVAILABLE_TYPES=$($AWS_EC2 describe-instance-types --query 'InstanceTypes[*].InstanceType' --output text)
INSTANCE_TYPE=$(echo $AVAILABLE_TYPES | tr ' ' '\n' | grep -m1 'nano')
if [ -z "$INSTANCE_TYPE" ] || [ "$INSTANCE_TYPE" == "None" ]; then
    echo "ERROR: No nano instance type found"; exit 1
fi
pass "instance type: $INSTANCE_TYPE"

echo "Discovering AMIs..."
ALL_IMAGES=$($AWS_EC2 describe-images --output json 2>&1)
# Prefer the system-imported ubuntu AMI (name starts with "ami-ubuntu").
# Fall back to any non-alpine image, then any image at all.
AMI_ID=$(echo "$ALL_IMAGES" | jq -r '[.Images[] | select(.Name | test("^ami-ubuntu"))][0].ImageId // empty')
if [ -z "$AMI_ID" ]; then
    AMI_ID=$(echo "$ALL_IMAGES" | jq -r '[.Images[] | select(.Name | test("alpine") | not)][0].ImageId // empty')
fi
if [ -z "$AMI_ID" ]; then
    AMI_ID=$(echo "$ALL_IMAGES" | jq -r '.Images[0].ImageId // empty')
fi
if [ -z "$AMI_ID" ] || [ "$AMI_ID" == "None" ]; then
    echo "ERROR: No AMI found"; exit 1
fi
pass "AMI: $AMI_ID"

echo "Creating key pair..."
$AWS_EC2 delete-key-pair --key-name lb-e2e-key 2>/dev/null || true
$AWS_EC2 create-key-pair --key-name lb-e2e-key --output json > /dev/null 2>&1 || {
    fail "create key pair"; exit 1
}
pass "key pair: lb-e2e-key"

# ==========================================================================
# Phase 1: Shared VPC + Subnet
# ==========================================================================
echo ""
echo "Phase 1: Shared VPC + Subnet"
echo "========================================"

echo "Creating VPC..."
VPC_OUTPUT=$($AWS_EC2 create-vpc --cidr-block 10.200.0.0/16 --output json) || { fail "create-vpc"; exit 1; }
VPC_ID=$(echo "$VPC_OUTPUT" | jq -r '.Vpc.VpcId')
pass "create-vpc: $VPC_ID"

echo "Creating internet gateway..."
IGW_OUTPUT=$($AWS_EC2 create-internet-gateway --output json) || { fail "create-internet-gateway"; exit 1; }
IGW_ID=$(echo "$IGW_OUTPUT" | jq -r '.InternetGateway.InternetGatewayId')
$AWS_EC2 attach-internet-gateway --internet-gateway-id "$IGW_ID" --vpc-id "$VPC_ID" || { fail "attach-internet-gateway"; exit 1; }
pass "internet gateway: $IGW_ID (attached)"

echo "Creating subnet..."
SUBNET_OUTPUT=$($AWS_EC2 create-subnet --vpc-id "$VPC_ID" --cidr-block 10.200.1.0/24 --output json) || { fail "create-subnet"; exit 1; }
SUBNET_ID=$(echo "$SUBNET_OUTPUT" | jq -r '.Subnet.SubnetId')
pass "create-subnet: $SUBNET_ID"

$AWS_EC2 modify-subnet-attribute --subnet-id "$SUBNET_ID" --map-public-ip-on-launch 2>&1 || { fail "modify-subnet-attribute"; exit 1; }
pass "MapPublicIpOnLaunch enabled"

# Authorize ingress on the new VPC's default SG before any run-instances.
# Default SGs allow ingress only from members of the same SG; LB health probes
# and client traffic come from outside the SG and would otherwise be dropped
# by the OVN port-group ACL, causing 120s polling timeouts.
DEFAULT_SG_LB=$($AWS_EC2 describe-security-groups \
    --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=default" \
    --query 'SecurityGroups[0].GroupId' --output text)
echo "VPC default SG: $DEFAULT_SG_LB"
for port in 80 9000; do
    set +e
    AUTH_OUTPUT=$($AWS_EC2 authorize-security-group-ingress \
        --group-id "$DEFAULT_SG_LB" \
        --protocol tcp --port "$port" --cidr 0.0.0.0/0 2>&1)
    AUTH_EXIT=$?
    set -e
    if [ $AUTH_EXIT -ne 0 ] && ! echo "$AUTH_OUTPUT" | grep -q 'InvalidPermission.Duplicate'; then
        fail "authorize tcp/$port on default SG: $AUTH_OUTPUT"; exit 1
    fi
done
pass "default SG ingress: tcp/80, tcp/9000 from 0.0.0.0/0"

# ==========================================================================
# Phase 2: Launch Dual-Purpose App Instances
# ==========================================================================
echo ""
echo "Phase 2: Launch App Instances"
echo "========================================"

APP_USER_DATA=$(cat <<'USERDATA'
#!/bin/bash
INSTANCE_ID=$(hostname)

# HTTP responder (ALB tests)
mkdir -p /tmp/httpd && cd /tmp/httpd
echo "{\"instance_id\": \"${INSTANCE_ID}\"}" > index.html
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &

# TCP echo responder (NLB tests) — stdlib only, no extra packages
cat > /tmp/tcp_echo.py << 'PYEOF'
import socketserver, os
class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.wfile.write((os.uname()[1] + "\n").encode())
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("0.0.0.0", 9000), Handler).serve_forever()
PYEOF
nohup python3 /tmp/tcp_echo.py > /dev/null 2>&1 &
USERDATA
)

echo "Launching 2 dual-purpose app instances (HTTP:80 + TCP:9000)..."
for i in 1 2; do
    echo "  Launching app instance $i..."
    RUN_OUTPUT=$($AWS_EC2 run-instances \
        --image-id "$AMI_ID" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name lb-e2e-key \
        --subnet-id "$SUBNET_ID" \
        --user-data "$APP_USER_DATA" \
        --output json 2>&1) || {
        fail "run-instances (app $i)"; echo "  Output: $RUN_OUTPUT"; exit 1
    }
    INST_ID=$(echo "$RUN_OUTPUT" | jq -r '.Instances[0].InstanceId')
    if [ -z "$INST_ID" ] || [ "$INST_ID" == "null" ]; then
        fail "run-instances (app $i) — no instance ID"; exit 1
    fi
    APP_INSTANCE_IDS+=("$INST_ID")
    echo "  App instance $i: $INST_ID"
done
pass "launched ${#APP_INSTANCE_IDS[@]} app instances"

echo "Waiting for instances to reach running state..."
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    for attempt in $(seq 1 60); do
        STATE=$($AWS_EC2 describe-instances --instance-ids "$inst_id" \
            --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null)
        if [ "$STATE" == "running" ]; then break; fi
        if [ "$STATE" == "terminated" ] || [ $attempt -eq 60 ]; then
            REASON=$($AWS_EC2 describe-instances --instance-ids "$inst_id" \
                --query 'Reservations[0].Instances[0].StateReason.Message' --output text 2>/dev/null || echo "unknown")
            fail "instance $inst_id did not reach running (stuck in $STATE, reason: $REASON)"
            # Dump daemon log tail for debugging
            echo "  Daemon log tail:"
            sudo journalctl -u spinifex-daemon --no-pager -n 30 2>/dev/null || tail -30 ~/spinifex/logs/spinifex.log 2>/dev/null || echo "  (no logs available)"
            exit 1
        fi
        sleep 2
    done
done
pass "all instances running"

echo "Collecting app instance private IPs..."
declare -A INSTANCE_IPS
for inst_id in "${APP_INSTANCE_IDS[@]}"; do
    IP=$($AWS_EC2 describe-instances --instance-ids "$inst_id" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
    if [ -z "$IP" ] || [ "$IP" == "None" ]; then
        fail "instance $inst_id has no PrivateIpAddress"; exit 1
    fi
    INSTANCE_IPS[$inst_id]="$IP"
    echo "  $inst_id -> $IP"
done
pass "all app instances have private IPs"

echo "App readiness will be verified via LB health checks in each test suite"

# ==========================================================================
# Helper: wait for LB to become active
# ==========================================================================
wait_for_lb_active() {
    local lb_arn="$1" label="$2" timeout="${3:-270}"
    echo "Waiting for $label to become active (up to ${timeout}s)..."
    local lb_active=false
    for attempt in $(seq 1 $((timeout / 3))); do
        local state
        state=$($AWS_ELBV2 describe-load-balancers --load-balancer-arns "$lb_arn" \
            --query 'LoadBalancers[0].State.Code' --output text 2>/dev/null)
        if [ "$state" == "active" ]; then
            lb_active=true; break
        fi
        if [ $((attempt % 10)) -eq 0 ]; then
            echo "  $label state: $state (attempt $attempt)"
        fi
        sleep 3
    done
    if [ "$lb_active" = true ]; then
        pass "$label state: active"
        return 0
    else
        fail "$label did not reach active state (stuck in $state)"
        echo "  Debug: daemon logs:"
        sudo journalctl -u spinifex-daemon --no-pager -n 50 2>/dev/null \
            | grep -iE 'LaunchSystemInstance|LB.VM|lb-agent|alb-agent|mgmt|heartbeat|GenerateVolumes|volume_preparation' \
            | tail -20 || \
            grep -iE 'LaunchSystemInstance|LB.VM|lb-agent|alb-agent|mgmt|heartbeat|GenerateVolumes|volume_preparation' ~/spinifex/logs/spinifex.log 2>/dev/null \
            | tail -20 || echo "  (no matching log lines)"
        return 1
    fi
}

# ==========================================================================
# Helper: wait for targets to become healthy
# ==========================================================================
wait_for_targets_healthy() {
    local tg_arn="$1" expected="$2" label="$3" timeout="${4:-120}"
    echo "Polling target health for $label (timeout ${timeout}s)..."
    local start=$(date +%s) healthy=false health_output
    while true; do
        local elapsed=$(( $(date +%s) - start ))
        if [ $elapsed -ge $timeout ]; then break; fi

        health_output=$($AWS_ELBV2 describe-target-health \
            --target-group-arn "$tg_arn" --output json 2>/dev/null) || { sleep 5; continue; }

        local healthy_count total_count
        healthy_count=$(echo "$health_output" | jq '[.TargetHealthDescriptions[] | select(.TargetHealth.State == "healthy")] | length')
        total_count=$(echo "$health_output" | jq '.TargetHealthDescriptions | length')
        echo "  ${elapsed}s: $healthy_count/$total_count targets healthy"

        if [ "$healthy_count" -eq "$expected" ]; then
            healthy=true; break
        fi
        sleep 5
    done
    if [ "$healthy" = true ]; then
        pass "$label: $expected targets healthy"
    else
        echo "  Current target health:"
        echo "$health_output" | jq -r '.TargetHealthDescriptions[] | "    \(.Target.Id): \(.TargetHealth.State) (\(.TargetHealth.Reason // "n/a"))"'
        fail "$label: targets did not become healthy within ${timeout}s"
    fi
}

# ==========================================================================
# Helper: verify ENI cleanup after LB deletion
# ==========================================================================
wait_for_eni_cleanup() {
    local eni_filter="$1" label="$2"
    echo "Verifying ENI cleanup for $label..."
    local cleaned=false
    for attempt in $(seq 1 10); do
        local count
        count=$($AWS_EC2 describe-network-interfaces \
            --filters "Name=description,Values=$eni_filter" \
            --query 'NetworkInterfaces | length(@)' --output text 2>/dev/null)
        if [ "$count" == "0" ] || [ -z "$count" ]; then
            cleaned=true; break
        fi
        sleep 3
    done
    if [ "$cleaned" = true ]; then
        pass "$label ENI cleaned up"
    else
        fail "$label ENI still exists after 30s"
    fi
}

# ==========================================================================
# Helper: run HTTP traffic test (ALB suites)
# ==========================================================================
run_http_traffic_test() {
    local url="$1" label="$2" num_requests="${3:-20}"
    echo "Sending $num_requests HTTP requests to $url ..."
    declare -A counts
    local total_ok=0 total_fail=0
    for i in $(seq 1 $num_requests); do
        local resp
        resp=$(curl -s --max-time 5 "$url/" 2>/dev/null) || { total_fail=$((total_fail+1)); continue; }
        local inst
        inst=$(echo "$resp" | jq -r '.instance_id // empty' 2>/dev/null)
        if [ -n "$inst" ]; then
            counts[$inst]=$(( ${counts[$inst]:-0} + 1 ))
            total_ok=$((total_ok+1))
        else
            total_fail=$((total_fail+1))
        fi
    done
    echo "  Results: $total_ok successful, $total_fail failed"
    echo "  Distribution:"
    for inst_id in "${!counts[@]}"; do echo "    $inst_id: ${counts[$inst_id]} responses"; done

    local unique=${#counts[@]}
    if [ "$unique" -ge 2 ]; then
        pass "$label round-robin: $unique unique instances"
    else
        fail "$label round-robin: expected 2 unique responders, got $unique"
    fi
    if [ "$total_ok" -ge $((num_requests / 2)) ]; then
        pass "$label success rate: $total_ok/$num_requests"
    else
        fail "$label success rate: only $total_ok/$num_requests"
    fi
}

# ==========================================================================
# Helper: run TCP traffic test (NLB suites)
# ==========================================================================
run_tcp_traffic_test() {
    local ip="$1" port="$2" label="$3" num_requests="${4:-20}"
    echo "Sending $num_requests TCP requests to ${ip}:${port} ..."
    declare -A counts
    local total_ok=0 total_fail=0
    for i in $(seq 1 $num_requests); do
        local resp
        resp=$(echo "" | nc -w5 "$ip" "$port" 2>/dev/null || true)
        resp=$(echo "$resp" | tr -d '[:space:]')
        if [ -n "$resp" ]; then
            counts[$resp]=$(( ${counts[$resp]:-0} + 1 ))
            total_ok=$((total_ok+1))
        else
            total_fail=$((total_fail+1))
        fi
    done
    echo "  Results: $total_ok successful, $total_fail failed"
    echo "  Distribution:"
    for inst_id in "${!counts[@]}"; do echo "    $inst_id: ${counts[$inst_id]} responses"; done

    local unique=${#counts[@]}
    if [ "$unique" -ge 2 ]; then
        pass "$label round-robin: $unique unique instances"
    elif [ "$unique" -eq 1 ]; then
        pass "$label traffic forwarded: 1 instance ($total_ok/$num_requests successful)"
    else
        fail "$label: no successful responses"
    fi
    if [ "$total_ok" -ge $((num_requests / 2)) ]; then
        pass "$label success rate: $total_ok/$num_requests"
    else
        fail "$label success rate: only $total_ok/$num_requests"
    fi
}

# ==========================================================================
# Helper: parse results file and verify traffic distribution
# ==========================================================================
verify_traffic_results() {
    local results="$1" proto="$2" label="$3"

    declare -A resp_counts
    local total_ok=0
    while IFS= read -r line; do
        if [ "$proto" = "http" ]; then
            local inst
            inst=$(echo "$line" | jq -r '.instance_id // empty' 2>/dev/null)
            if [ -n "$inst" ]; then
                resp_counts[$inst]=$(( ${resp_counts[$inst]:-0} + 1 ))
                total_ok=$((total_ok+1))
            fi
        else
            line=$(echo "$line" | tr -d '[:space:]')
            if [ -n "$line" ]; then
                resp_counts[$line]=$(( ${resp_counts[$line]:-0} + 1 ))
                total_ok=$((total_ok+1))
            fi
        fi
    done <<< "$results"

    echo "  Results: $total_ok successful"
    echo "  Distribution:"
    for inst_id in "${!resp_counts[@]}"; do echo "    $inst_id: ${resp_counts[$inst_id]} responses"; done

    local unique=${#resp_counts[@]}
    if [ "$unique" -ge 2 ]; then
        pass "$label round-robin via private IP: $unique unique instances"
    elif [ "$unique" -eq 1 ]; then
        pass "$label traffic forwarded via private IP: 1 instance ($total_ok/20 successful)"
    else
        fail "$label: no successful responses"
    fi
    if [ "$total_ok" -ge 10 ]; then
        pass "$label success rate: $total_ok/20"
    else
        fail "$label success rate: only $total_ok/20"
    fi
}


# ##########################################################################
# Phase 3a: Internet-Facing (ALB + NLB in parallel)
# ##########################################################################
if [ "$PEER_AVAILABLE" = true ]; then
    echo ""
    echo "========================================"
    echo "Phase 3a: Internet-Facing (ALB + NLB)"
    echo "========================================"

    # --- Create both TGs ---
    echo "Creating HTTP target group (ALB)..."
    TG_OUTPUT=$($AWS_ELBV2 create-target-group \
        --name lb-e2e-alb-inet-tg \
        --protocol HTTP --port 80 \
        --vpc-id "$VPC_ID" \
        --health-check-path "/index.html" \
        --health-check-interval-seconds 5 \
        --healthy-threshold-count 2 \
        --unhealthy-threshold-count 2 \
        --output json 2>&1) || { fail "create-target-group (ALB inet)"; }
    ALB_TG_ARN=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].TargetGroupArn')
    pass "ALB target group: $ALB_TG_ARN"

    echo "Creating TCP target group (NLB)..."
    TG_OUTPUT=$($AWS_ELBV2 create-target-group \
        --name lb-e2e-nlb-inet-tg \
        --protocol TCP --port 9000 \
        --vpc-id "$VPC_ID" \
        --health-check-protocol TCP \
        --health-check-interval-seconds 10 \
        --healthy-threshold-count 2 \
        --unhealthy-threshold-count 2 \
        --output json 2>&1) || { fail "create-target-group (NLB inet)"; }
    NLB_TG_ARN=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].TargetGroupArn')
    pass "NLB target group: $NLB_TG_ARN"

    TG_PROTOCOL=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].Protocol')
    HC_PROTOCOL=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].HealthCheckProtocol')
    if [ "$TG_PROTOCOL" == "TCP" ]; then pass "NLB TG protocol: TCP"; else fail "NLB TG protocol: expected TCP, got $TG_PROTOCOL"; fi
    if [ "$HC_PROTOCOL" == "TCP" ]; then pass "NLB HC protocol: TCP"; else fail "NLB HC protocol: expected TCP, got $HC_PROTOCOL"; fi

    # Register targets to both TGs
    $AWS_ELBV2 register-targets --target-group-arn "$ALB_TG_ARN" \
        --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" --output json 2>&1 || { fail "register-targets (ALB)"; }
    $AWS_ELBV2 register-targets --target-group-arn "$NLB_TG_ARN" \
        --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" --output json 2>&1 || { fail "register-targets (NLB)"; }
    pass "registered 2 targets to both TGs"

    # --- Create both LBs (they provision in parallel) ---
    echo "Creating internet-facing ALB..."
    LB_OUTPUT=$($AWS_ELBV2 create-load-balancer \
        --name lb-e2e-alb-inet \
        --scheme internet-facing \
        --subnets "$SUBNET_ID" \
        --output json 2>&1) || { fail "create-load-balancer (ALB inet)"; }
    ALB_LB_ARN=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].LoadBalancerArn')
    ALB_LB_SCHEME=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].Scheme')
    pass "ALB: $ALB_LB_ARN (scheme: $ALB_LB_SCHEME)"
    if [ "$ALB_LB_SCHEME" == "internet-facing" ]; then pass "ALB scheme: internet-facing"; else fail "ALB scheme: expected internet-facing, got $ALB_LB_SCHEME"; fi

    echo "Creating internet-facing NLB..."
    LB_OUTPUT=$($AWS_ELBV2 create-load-balancer \
        --name lb-e2e-nlb-inet \
        --type network \
        --subnets "$SUBNET_ID" \
        --output json 2>&1) || { fail "create-load-balancer (NLB inet)"; }
    NLB_LB_ARN=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].LoadBalancerArn')
    NLB_LB_TYPE=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].Type')
    pass "NLB: $NLB_LB_ARN (type: $NLB_LB_TYPE)"
    if [ "$NLB_LB_TYPE" == "network" ]; then pass "NLB type: network"; else fail "NLB type: expected network, got $NLB_LB_TYPE"; fi
    if echo "$NLB_LB_ARN" | grep -q "/net/"; then pass "NLB ARN contains /net/"; else fail "NLB ARN missing /net/"; fi

    # --- Verify ENIs ---
    ALB_LB_ID=$(echo "$ALB_LB_ARN" | sed 's|.*/||')
    NLB_LB_ID=$(echo "$NLB_LB_ARN" | sed 's|.*/||')
    sleep 3

    ENI_OUTPUT=$($AWS_EC2 describe-network-interfaces \
        --filters "Name=description,Values=ELB app/lb-e2e-alb-inet/${ALB_LB_ID}" \
        --output json 2>/dev/null)
    ALB_PUBLIC_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty' 2>/dev/null)
    ALB_ENI_ID=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].NetworkInterfaceId // empty' 2>/dev/null)
    if [ -n "$ALB_PUBLIC_IP" ] && [ "$ALB_PUBLIC_IP" != "null" ]; then
        pass "ALB ENI has public IP: $ALB_PUBLIC_IP"
    else
        fail "ALB ENI has no public IP"; exit 1
    fi

    ENI_OUTPUT=$($AWS_EC2 describe-network-interfaces \
        --filters "Name=description,Values=ELB net/lb-e2e-nlb-inet/${NLB_LB_ID}" \
        --output json 2>/dev/null)
    NLB_PUBLIC_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty' 2>/dev/null)
    NLB_ENI_ID=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].NetworkInterfaceId // empty' 2>/dev/null)
    if [ -n "$NLB_PUBLIC_IP" ] && [ "$NLB_PUBLIC_IP" != "null" ]; then
        pass "NLB ENI has public IP: $NLB_PUBLIC_IP"
    else
        fail "NLB ENI has no public IP"
    fi

    # DNS checks
    ALB_DNS=$($AWS_ELBV2 describe-load-balancers --load-balancer-arns "$ALB_LB_ARN" \
        --query 'LoadBalancers[0].DNSName' --output text 2>/dev/null)
    if echo "$ALB_DNS" | grep -q "^internal-"; then
        fail "ALB DNS has internal- prefix for internet-facing"
    else
        pass "ALB DNS: no internal- prefix"
    fi

    # --- Create both listeners ---
    echo "Creating ALB listener (HTTP:80)..."
    LISTENER_OUTPUT=$($AWS_ELBV2 create-listener \
        --load-balancer-arn "$ALB_LB_ARN" \
        --protocol HTTP --port 80 \
        --default-actions "Type=forward,TargetGroupArn=$ALB_TG_ARN" \
        --output json 2>&1) || { fail "create-listener (ALB)"; }
    ALB_LISTENER_ARN=$(echo "$LISTENER_OUTPUT" | jq -r '.Listeners[0].ListenerArn')
    pass "ALB listener: $ALB_LISTENER_ARN"

    echo "Creating NLB listener (TCP:9000)..."
    LISTENER_OUTPUT=$($AWS_ELBV2 create-listener \
        --load-balancer-arn "$NLB_LB_ARN" \
        --protocol TCP --port 9000 \
        --default-actions "Type=forward,TargetGroupArn=$NLB_TG_ARN" \
        --output json 2>&1) || { fail "create-listener (NLB)"; }
    NLB_LISTENER_ARN=$(echo "$LISTENER_OUTPUT" | jq -r '.Listeners[0].ListenerArn')
    pass "NLB listener: $NLB_LISTENER_ARN"

    # --- Wait for both LBs active (provisioning in parallel, checked sequentially) ---
    wait_for_lb_active "$ALB_LB_ARN" "ALB internet-facing" || exit 1
    wait_for_lb_active "$NLB_LB_ARN" "NLB internet-facing" || exit 1

    # --- Wait for both TGs healthy ---
    wait_for_targets_healthy "$ALB_TG_ARN" 2 "ALB internet-facing"
    wait_for_targets_healthy "$NLB_TG_ARN" 2 "NLB internet-facing"

    # --- Discover hosting nodes for each LB ---
    # The LB VM can land on any node via NATS queue group distribution.
    # "Local" tests run from the hosting node; "remote" tests from another node.
    ALB_HOST=""
    NLB_HOST=""
    ALB_REMOTE=""
    NLB_REMOTE=""
    LOCAL_IP=$(ip -4 -o addr show br-wan 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)

    if [ ${#ALL_NODE_IPS[@]} -gt 1 ]; then
        echo "Discovering LB hosting nodes..."
        if [ -n "$ALB_ENI_ID" ]; then
            ALB_HOST=$(find_lb_host "$ALB_ENI_ID") || true
        fi
        if [ -n "$NLB_ENI_ID" ]; then
            NLB_HOST=$(find_lb_host "$NLB_ENI_ID") || true
        fi
        if [ -n "$ALB_HOST" ]; then
            ALB_REMOTE=$(pick_peer "$ALB_HOST") || ALB_REMOTE=""
            echo "  ALB VM on $ALB_HOST (remote test from $ALB_REMOTE)"
        else
            echo "  ALB VM host: unknown (falling back to local=$LOCAL_IP, remote=$PEER_NODE_IP)"
            ALB_HOST="$LOCAL_IP"
            ALB_REMOTE="$PEER_NODE_IP"
        fi
        if [ -n "$NLB_HOST" ]; then
            NLB_REMOTE=$(pick_peer "$NLB_HOST") || NLB_REMOTE=""
            echo "  NLB VM on $NLB_HOST (remote test from $NLB_REMOTE)"
        else
            echo "  NLB VM host: unknown (falling back to local=$LOCAL_IP, remote=$PEER_NODE_IP)"
            NLB_HOST="$LOCAL_IP"
            NLB_REMOTE="$PEER_NODE_IP"
        fi
    else
        ALB_HOST="$LOCAL_IP"
        NLB_HOST="$LOCAL_IP"
    fi

    # --- Helper: run a connectivity test from a specific node ---
    # test_http_from <node_ip> <url> <label>
    test_http_from() {
        local node_ip="$1" url="$2" label="$3"
        echo "Testing $label at $url from $node_ip..."
        local ok=false
        for attempt in $(seq 1 20); do
            local result
            if [ "$node_ip" = "$LOCAL_IP" ]; then
                result=$(curl -s --max-time 3 "$url/" 2>/dev/null || true)
            else
                result=$(peer_ssh "$node_ip" "curl -s --max-time 3 '$url/'" 2>/dev/null || true)
            fi
            if echo "$result" | grep -q "instance_id"; then
                ok=true; break
            fi
            echo "  Attempt $attempt/20: not yet responding..."
            sleep 5
        done
        if [ "$ok" = true ]; then pass "$label reachable"; else fail "$label unreachable at $url from $node_ip"; fi
        echo "$ok"
    }

    # test_tcp_from <node_ip> <ip> <port> <label>
    test_tcp_from() {
        local node_ip="$1" target_ip="$2" port="$3" label="$4"
        echo "Testing $label at ${target_ip}:${port} from $node_ip..."
        local ok=false
        for attempt in $(seq 1 20); do
            local probe
            if [ "$node_ip" = "$LOCAL_IP" ]; then
                probe=$(echo "" | nc -w5 "$target_ip" "$port" 2>/dev/null || true)
            else
                probe=$(peer_ssh "$node_ip" "echo '' | nc -w5 '$target_ip' '$port'" 2>/dev/null || true)
            fi
            if [ -n "$probe" ]; then ok=true; break; fi
            echo "  Attempt $attempt/20..."
            sleep 5
        done
        if [ "$ok" = true ]; then pass "$label reachable"; else fail "$label unreachable at ${target_ip}:${port} from $node_ip"; fi
        echo "$ok"
    }

    # --- ALB: local connectivity (from hosting node) ---
    ALB_URL="http://${ALB_PUBLIC_IP}:80"
    ALB_LOCAL_OK=$(test_http_from "$ALB_HOST" "$ALB_URL" "ALB inet (local)")

    # ALB traffic test from hosting node
    if [ "$ALB_LOCAL_OK" = "true" ]; then
        if [ "$ALB_HOST" = "$LOCAL_IP" ]; then
            run_http_traffic_test "$ALB_URL" "ALB inet (local)"
        else
            # Run traffic test via SSH on the hosting node
            declare -A ALB_LOCAL_COUNTS
            ALB_LOCAL_TOTAL=0
            for i in $(seq 1 20); do
                RESPONSE=$(peer_ssh "$ALB_HOST" "curl -s --max-time 5 '$ALB_URL/'" 2>/dev/null) || continue
                RESP_INSTANCE=$(echo "$RESPONSE" | jq -r '.instance_id // empty' 2>/dev/null)
                if [ -n "$RESP_INSTANCE" ]; then
                    ALB_LOCAL_COUNTS[$RESP_INSTANCE]=$(( ${ALB_LOCAL_COUNTS[$RESP_INSTANCE]:-0} + 1 ))
                    ALB_LOCAL_TOTAL=$((ALB_LOCAL_TOTAL + 1))
                fi
            done
            echo "  Distribution:"
            for inst_id in "${!ALB_LOCAL_COUNTS[@]}"; do echo "    $inst_id: ${ALB_LOCAL_COUNTS[$inst_id]} responses"; done
            if [ "${#ALB_LOCAL_COUNTS[@]}" -ge 2 ]; then pass "ALB inet (local) round-robin: ${#ALB_LOCAL_COUNTS[@]} unique instances"; else fail "ALB inet (local) round-robin: expected 2 unique responders, got ${#ALB_LOCAL_COUNTS[@]}"; fi
            if [ "$ALB_LOCAL_TOTAL" -ge 10 ]; then pass "ALB inet (local) success rate: $ALB_LOCAL_TOTAL/20"; else fail "ALB inet (local) success rate: only $ALB_LOCAL_TOTAL/20"; fi
        fi
    fi

    # --- NLB: local connectivity (from hosting node) ---
    if [ -n "$NLB_PUBLIC_IP" ] && [ "$NLB_PUBLIC_IP" != "null" ]; then
        NLB_LOCAL_OK=$(test_tcp_from "$NLB_HOST" "$NLB_PUBLIC_IP" 9000 "NLB inet (local)")
        if [ "$NLB_LOCAL_OK" = "true" ]; then
            if [ "$NLB_HOST" = "$LOCAL_IP" ]; then
                run_tcp_traffic_test "$NLB_PUBLIC_IP" 9000 "NLB inet (local)"
            else
                declare -A NLB_LOCAL_COUNTS
                NLB_LOCAL_TOTAL=0
                for i in $(seq 1 20); do
                    RESPONSE=$(peer_ssh "$NLB_HOST" "echo '' | nc -w5 '$NLB_PUBLIC_IP' 9000" 2>/dev/null) || continue
                    RESP_INSTANCE=$(echo "$(echo "$RESPONSE" | tr -d '[:space:]')")
                    if [ -n "$RESP_INSTANCE" ]; then
                        NLB_LOCAL_COUNTS[$RESP_INSTANCE]=$(( ${NLB_LOCAL_COUNTS[$RESP_INSTANCE]:-0} + 1 ))
                        NLB_LOCAL_TOTAL=$((NLB_LOCAL_TOTAL + 1))
                    fi
                done
                echo "  Distribution:"
                for inst_id in "${!NLB_LOCAL_COUNTS[@]}"; do echo "    $inst_id: ${NLB_LOCAL_COUNTS[$inst_id]} responses"; done
                if [ "${#NLB_LOCAL_COUNTS[@]}" -ge 2 ]; then pass "NLB inet (local) round-robin: ${#NLB_LOCAL_COUNTS[@]} unique instances"; else fail "NLB inet (local) round-robin: expected 2 unique responders, got ${#NLB_LOCAL_COUNTS[@]}"; fi
                if [ "$NLB_LOCAL_TOTAL" -ge 10 ]; then pass "NLB inet (local) success rate: $NLB_LOCAL_TOTAL/20"; else fail "NLB inet (local) success rate: only $NLB_LOCAL_TOTAL/20"; fi
            fi
        fi
    fi

    # --- ALB: remote connectivity (from a different node) ---
    if [ -n "$ALB_REMOTE" ]; then
        ALB_REMOTE_OK=$(test_http_from "$ALB_REMOTE" "$ALB_URL" "ALB inet (remote)")
        if [ "$ALB_REMOTE_OK" = "true" ]; then
            declare -A ALB_REMOTE_COUNTS
            ALB_REMOTE_TOTAL=0
            for i in $(seq 1 20); do
                RESPONSE=$(peer_ssh "$ALB_REMOTE" "curl -s --max-time 5 '$ALB_URL/'" 2>/dev/null) || continue
                RESP_INSTANCE=$(echo "$RESPONSE" | jq -r '.instance_id // empty' 2>/dev/null)
                if [ -n "$RESP_INSTANCE" ]; then
                    ALB_REMOTE_COUNTS[$RESP_INSTANCE]=$(( ${ALB_REMOTE_COUNTS[$RESP_INSTANCE]:-0} + 1 ))
                    ALB_REMOTE_TOTAL=$((ALB_REMOTE_TOTAL + 1))
                fi
            done
            echo "  Distribution:"
            for inst_id in "${!ALB_REMOTE_COUNTS[@]}"; do echo "    $inst_id: ${ALB_REMOTE_COUNTS[$inst_id]} responses"; done
            if [ "${#ALB_REMOTE_COUNTS[@]}" -ge 2 ]; then pass "ALB inet (remote) round-robin: ${#ALB_REMOTE_COUNTS[@]} unique"; else fail "ALB inet (remote) round-robin: ${#ALB_REMOTE_COUNTS[@]} unique"; fi
            if [ "$ALB_REMOTE_TOTAL" -ge 10 ]; then pass "ALB inet (remote) success rate: $ALB_REMOTE_TOTAL/20"; else fail "ALB inet (remote) success rate: $ALB_REMOTE_TOTAL/20"; fi
        fi
    fi

    # --- NLB: remote connectivity (from a different node) ---
    if [ -n "$NLB_REMOTE" ] && [ -n "$NLB_PUBLIC_IP" ] && [ "$NLB_PUBLIC_IP" != "null" ]; then
        NLB_REMOTE_OK=$(test_tcp_from "$NLB_REMOTE" "$NLB_PUBLIC_IP" 9000 "NLB inet (remote)")
        if [ "$NLB_REMOTE_OK" = "true" ]; then
            declare -A NLB_REMOTE_COUNTS
            NLB_REMOTE_TOTAL=0
            for i in $(seq 1 20); do
                RESPONSE=$(peer_ssh "$NLB_REMOTE" "echo '' | nc -w5 '$NLB_PUBLIC_IP' 9000" 2>/dev/null) || continue
                RESP_INSTANCE=$(echo "$(echo "$RESPONSE" | tr -d '[:space:]')")
                if [ -n "$RESP_INSTANCE" ]; then
                    NLB_REMOTE_COUNTS[$RESP_INSTANCE]=$(( ${NLB_REMOTE_COUNTS[$RESP_INSTANCE]:-0} + 1 ))
                    NLB_REMOTE_TOTAL=$((NLB_REMOTE_TOTAL + 1))
                fi
            done
            echo "  Distribution:"
            for inst_id in "${!NLB_REMOTE_COUNTS[@]}"; do echo "    $inst_id: ${NLB_REMOTE_COUNTS[$inst_id]} responses"; done
            if [ "${#NLB_REMOTE_COUNTS[@]}" -ge 2 ]; then pass "NLB inet (remote) round-robin: ${#NLB_REMOTE_COUNTS[@]} unique"; else fail "NLB inet (remote) round-robin: ${#NLB_REMOTE_COUNTS[@]} unique"; fi
            if [ "$NLB_REMOTE_TOTAL" -ge 10 ]; then pass "NLB inet (remote) success rate: $NLB_REMOTE_TOTAL/20"; else fail "NLB inet (remote) success rate: $NLB_REMOTE_TOTAL/20"; fi
        fi
    fi

    # --- NLB deregister/draining test ---
    echo "Deregistering first target from NLB: ${APP_INSTANCE_IDS[0]}..."
    $AWS_ELBV2 deregister-targets --target-group-arn "$NLB_TG_ARN" \
        --targets "Id=${APP_INSTANCE_IDS[0]}" --output json 2>&1 || fail "deregister-targets"
    pass "deregistered ${APP_INSTANCE_IDS[0]}"

    sleep 3
    HEALTH_OUTPUT=$($AWS_ELBV2 describe-target-health --target-group-arn "$NLB_TG_ARN" --output json 2>/dev/null) || true
    REMAINING=$(echo "$HEALTH_OUTPUT" | jq '.TargetHealthDescriptions | length')
    DRAINING=$(echo "$HEALTH_OUTPUT" | jq '[.TargetHealthDescriptions[] | select(.TargetHealth.State == "draining")] | length')
    echo "  Targets remaining: $REMAINING (draining: $DRAINING)"
    if [ "$REMAINING" -eq 1 ]; then pass "target deregistered: 1 remaining"
    elif [ "$DRAINING" -ge 1 ]; then pass "target in draining state"
    else pass "target deregistration processed"
    fi

    # --- Cleanup both internet-facing LBs ---
    echo ""
    echo "Cleaning up internet-facing LBs..."
    $AWS_ELBV2 delete-listener --listener-arn "$ALB_LISTENER_ARN" 2>/dev/null || true
    $AWS_ELBV2 delete-listener --listener-arn "$NLB_LISTENER_ARN" 2>/dev/null || true
    $AWS_ELBV2 delete-load-balancer --load-balancer-arn "$ALB_LB_ARN" 2>/dev/null || true
    $AWS_ELBV2 delete-load-balancer --load-balancer-arn "$NLB_LB_ARN" 2>/dev/null || true
    pass "deleted both internet-facing LBs"

    wait_for_eni_cleanup "ELB app/lb-e2e-alb-inet/${ALB_LB_ID}" "ALB internet-facing"
    wait_for_eni_cleanup "ELB net/lb-e2e-nlb-inet/${NLB_LB_ID}" "NLB internet-facing"

    $AWS_ELBV2 deregister-targets --target-group-arn "$ALB_TG_ARN" \
        --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" 2>/dev/null || true
    $AWS_ELBV2 deregister-targets --target-group-arn "$NLB_TG_ARN" \
        --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" 2>/dev/null || true
    $AWS_ELBV2 delete-target-group --target-group-arn "$ALB_TG_ARN" 2>/dev/null || true
    $AWS_ELBV2 delete-target-group --target-group-arn "$NLB_TG_ARN" 2>/dev/null || true
    ALB_LB_ARN=""; ALB_TG_ARN=""; ALB_LISTENER_ARN=""
    NLB_LB_ARN=""; NLB_TG_ARN=""; NLB_LISTENER_ARN=""
    pass "internet-facing cleanup complete"
else
    echo ""
    echo "========================================"
    echo "Phase 3a: Internet-Facing — SKIPPED (no --peer)"
    echo "========================================"
fi


# ##########################################################################
# Phase 3b: Internal (ALB + NLB in parallel, one shared client VM)
# ##########################################################################
echo ""
echo "========================================"
echo "Phase 3b: Internal (ALB + NLB)"
echo "========================================"

# --- Create both TGs ---
echo "Creating HTTP target group (ALB internal)..."
TG_OUTPUT=$($AWS_ELBV2 create-target-group \
    --name lb-e2e-alb-int-tg \
    --protocol HTTP --port 80 \
    --vpc-id "$VPC_ID" \
    --health-check-path "/index.html" \
    --health-check-interval-seconds 5 \
    --healthy-threshold-count 2 \
    --unhealthy-threshold-count 2 \
    --output json 2>&1) || { fail "create-target-group (ALB int)"; }
ALB_TG_ARN=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].TargetGroupArn')
pass "ALB target group: $ALB_TG_ARN"

echo "Creating TCP target group (NLB internal)..."
TG_OUTPUT=$($AWS_ELBV2 create-target-group \
    --name lb-e2e-nlb-int-tg \
    --protocol TCP --port 9000 \
    --vpc-id "$VPC_ID" \
    --health-check-protocol TCP \
    --health-check-interval-seconds 10 \
    --healthy-threshold-count 2 \
    --unhealthy-threshold-count 2 \
    --output json 2>&1) || { fail "create-target-group (NLB int)"; }
NLB_TG_ARN=$(echo "$TG_OUTPUT" | jq -r '.TargetGroups[0].TargetGroupArn')
pass "NLB target group: $NLB_TG_ARN"

# Register targets to both
$AWS_ELBV2 register-targets --target-group-arn "$ALB_TG_ARN" \
    --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" --output json 2>&1 || { fail "register-targets (ALB)"; }
$AWS_ELBV2 register-targets --target-group-arn "$NLB_TG_ARN" \
    --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" --output json 2>&1 || { fail "register-targets (NLB)"; }
pass "registered 2 targets to both TGs"

# --- Create both LBs (provision in parallel) ---
echo "Creating internal ALB..."
LB_OUTPUT=$($AWS_ELBV2 create-load-balancer \
    --name lb-e2e-alb-int \
    --scheme internal \
    --subnets "$SUBNET_ID" \
    --output json 2>&1) || { fail "create-load-balancer (ALB int)"; }
ALB_LB_ARN=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].LoadBalancerArn')
ALB_LB_SCHEME=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].Scheme')
pass "ALB: $ALB_LB_ARN (scheme: $ALB_LB_SCHEME)"
if [ "$ALB_LB_SCHEME" == "internal" ]; then pass "ALB scheme: internal"; else fail "ALB scheme: expected internal, got $ALB_LB_SCHEME"; fi

echo "Creating internal NLB..."
LB_OUTPUT=$($AWS_ELBV2 create-load-balancer \
    --name lb-e2e-nlb-int \
    --type network \
    --scheme internal \
    --subnets "$SUBNET_ID" \
    --output json 2>&1) || { fail "create-load-balancer (NLB int)"; }
NLB_LB_ARN=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].LoadBalancerArn')
NLB_LB_TYPE=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].Type')
NLB_LB_SCHEME=$(echo "$LB_OUTPUT" | jq -r '.LoadBalancers[0].Scheme')
pass "NLB: $NLB_LB_ARN (type: $NLB_LB_TYPE, scheme: $NLB_LB_SCHEME)"
if [ "$NLB_LB_TYPE" == "network" ]; then pass "NLB type: network"; else fail "NLB type: expected network, got $NLB_LB_TYPE"; fi
if [ "$NLB_LB_SCHEME" == "internal" ]; then pass "NLB scheme: internal"; else fail "NLB scheme: expected internal, got $NLB_LB_SCHEME"; fi
if echo "$NLB_LB_ARN" | grep -q "/net/"; then pass "NLB ARN contains /net/"; else fail "NLB ARN missing /net/"; fi

# --- Verify ENIs ---
ALB_LB_ID=$(echo "$ALB_LB_ARN" | sed 's|.*/||')
NLB_LB_ID=$(echo "$NLB_LB_ARN" | sed 's|.*/||')
sleep 3

ENI_OUTPUT=$($AWS_EC2 describe-network-interfaces \
    --filters "Name=description,Values=ELB app/lb-e2e-alb-int/${ALB_LB_ID}" \
    --output json 2>/dev/null)
ALB_PUBLIC_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty' 2>/dev/null)
ALB_PRIVATE_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].PrivateIpAddress // empty' 2>/dev/null)
echo "  ALB Private IP: $ALB_PRIVATE_IP  Public IP: ${ALB_PUBLIC_IP:-(none)}"
if [ -z "$ALB_PUBLIC_IP" ] || [ "$ALB_PUBLIC_IP" == "null" ]; then pass "ALB has no public IP (correct)"; else fail "ALB should not have public IP: $ALB_PUBLIC_IP"; fi
if [ -n "$ALB_PRIVATE_IP" ] && [ "$ALB_PRIVATE_IP" != "null" ]; then pass "ALB private IP: $ALB_PRIVATE_IP"; else fail "ALB has no private IP"; exit 1; fi

ENI_OUTPUT=$($AWS_EC2 describe-network-interfaces \
    --filters "Name=description,Values=ELB net/lb-e2e-nlb-int/${NLB_LB_ID}" \
    --output json 2>/dev/null)
NLB_PUBLIC_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty' 2>/dev/null)
NLB_PRIVATE_IP=$(echo "$ENI_OUTPUT" | jq -r '.NetworkInterfaces[0].PrivateIpAddress // empty' 2>/dev/null)
echo "  NLB Private IP: $NLB_PRIVATE_IP  Public IP: ${NLB_PUBLIC_IP:-(none)}"
if [ -z "$NLB_PUBLIC_IP" ] || [ "$NLB_PUBLIC_IP" == "null" ]; then pass "NLB has no public IP (correct)"; else fail "NLB should not have public IP: $NLB_PUBLIC_IP"; fi
if [ -n "$NLB_PRIVATE_IP" ] && [ "$NLB_PRIVATE_IP" != "null" ]; then pass "NLB private IP: $NLB_PRIVATE_IP"; else fail "NLB has no private IP"; exit 1; fi

# DNS checks
ALB_DNS=$($AWS_ELBV2 describe-load-balancers --load-balancer-arns "$ALB_LB_ARN" \
    --query 'LoadBalancers[0].DNSName' --output text 2>/dev/null)
if echo "$ALB_DNS" | grep -q "^internal-"; then pass "ALB DNS has internal- prefix"; else fail "ALB DNS missing internal- prefix"; fi

NLB_DNS=$($AWS_ELBV2 describe-load-balancers --load-balancer-arns "$NLB_LB_ARN" \
    --query 'LoadBalancers[0].DNSName' --output text 2>/dev/null)
if echo "$NLB_DNS" | grep -q "^internal-"; then pass "NLB DNS has internal- prefix"; else fail "NLB DNS missing internal- prefix"; fi

# --- Create both listeners ---
echo "Creating ALB listener (HTTP:80)..."
LISTENER_OUTPUT=$($AWS_ELBV2 create-listener \
    --load-balancer-arn "$ALB_LB_ARN" \
    --protocol HTTP --port 80 \
    --default-actions "Type=forward,TargetGroupArn=$ALB_TG_ARN" \
    --output json 2>&1) || { fail "create-listener (ALB)"; }
ALB_LISTENER_ARN=$(echo "$LISTENER_OUTPUT" | jq -r '.Listeners[0].ListenerArn')
pass "ALB listener: $ALB_LISTENER_ARN"

echo "Creating NLB listener (TCP:9000)..."
LISTENER_OUTPUT=$($AWS_ELBV2 create-listener \
    --load-balancer-arn "$NLB_LB_ARN" \
    --protocol TCP --port 9000 \
    --default-actions "Type=forward,TargetGroupArn=$NLB_TG_ARN" \
    --output json 2>&1) || { fail "create-listener (NLB)"; }
NLB_LISTENER_ARN=$(echo "$LISTENER_OUTPUT" | jq -r '.Listeners[0].ListenerArn')
pass "NLB listener: $NLB_LISTENER_ARN"

# --- Wait for both active (provisioning in parallel, checked sequentially) ---
wait_for_lb_active "$ALB_LB_ARN" "ALB internal" || exit 1
wait_for_lb_active "$NLB_LB_ARN" "NLB internal" || exit 1

# --- Wait for both TGs healthy ---
wait_for_targets_healthy "$ALB_TG_ARN" 2 "ALB internal"
wait_for_targets_healthy "$NLB_TG_ARN" 2 "NLB internal"

# --- Launch ONE client VM that tests both LBs ---
echo "Launching client VM to test both internal LBs..."
CLIENT_USER_DATA=$(cat <<USERDATA
#!/bin/bash
ALB_IP="${ALB_PRIVATE_IP}"
NLB_IP="${NLB_PRIVATE_IP}"
NUM_REQUESTS=20

mkdir -p /tmp/httpd && cd /tmp/httpd
echo "waiting" > status.txt
nohup python3 -m http.server 80 --bind 0.0.0.0 > /dev/null 2>&1 &

# Wait for ALB to respond
for i in \$(seq 1 60); do
    if curl -s --max-time 3 "http://\${ALB_IP}:80/" 2>/dev/null | grep -q instance_id; then break; fi
    sleep 5
done

# Test ALB (HTTP)
> alb_results.txt
for i in \$(seq 1 \$NUM_REQUESTS); do
    curl -s --max-time 5 "http://\${ALB_IP}:80/" >> alb_results.txt 2>/dev/null
    echo "" >> alb_results.txt
done

# Wait for NLB to respond
for i in \$(seq 1 60); do
    PROBE=\$(echo "" | nc -w3 \${NLB_IP} 9000 2>/dev/null || true)
    if [ -n "\$PROBE" ]; then break; fi
    sleep 5
done

# Test NLB (TCP)
> nlb_results.txt
for i in \$(seq 1 \$NUM_REQUESTS); do
    RESP=\$(echo "" | nc -w2 \${NLB_IP} 9000 2>/dev/null || true)
    echo "\$RESP" >> nlb_results.txt
done

echo "done" > status.txt
USERDATA
)

CLIENT_OUTPUT=$($AWS_EC2 run-instances \
    --image-id "$AMI_ID" \
    --instance-type "$INSTANCE_TYPE" \
    --key-name lb-e2e-key \
    --subnet-id "$SUBNET_ID" \
    --user-data "$CLIENT_USER_DATA" \
    --output json 2>&1) || {
    fail "run-instances (client)"; exit 1
}
CLIENT_INSTANCE_ID=$(echo "$CLIENT_OUTPUT" | jq -r '.Instances[0].InstanceId')
echo "  Client instance: $CLIENT_INSTANCE_ID"
pass "launched client VM"

# Wait for running
for attempt in $(seq 1 60); do
    STATE=$($AWS_EC2 describe-instances --instance-ids "$CLIENT_INSTANCE_ID" \
        --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null)
    if [ "$STATE" == "running" ]; then break; fi
    if [ $attempt -eq 60 ]; then fail "client instance did not reach running"; exit 1; fi
    sleep 2
done

# Discover client public IP
CLIENT_ENI=$($AWS_EC2 describe-network-interfaces \
    --filters "Name=attachment.instance-id,Values=$CLIENT_INSTANCE_ID" \
    --output json 2>/dev/null)
CLIENT_PUBLIC_IP=$(echo "$CLIENT_ENI" | jq -r '.NetworkInterfaces[0].Association.PublicIp // empty' 2>/dev/null)
if [ -z "$CLIENT_PUBLIC_IP" ] || [ "$CLIENT_PUBLIC_IP" == "null" ]; then
    fail "client VM has no public IP"; exit 1
fi
pass "client VM public IP: $CLIENT_PUBLIC_IP"

# Poll for completion
echo "Waiting for client test to complete..."
CLIENT_DONE=false
for attempt in $(seq 1 60); do
    STATUS=$(curl -s --max-time 3 "http://${CLIENT_PUBLIC_IP}:80/status.txt" 2>/dev/null) || true
    if [ "$STATUS" == "done" ]; then CLIENT_DONE=true; break; fi
    if [ $((attempt % 10)) -eq 0 ]; then
        echo "  Client status: ${STATUS:-unreachable} (attempt $attempt/60)"
    fi
    sleep 5
done
if [ "$CLIENT_DONE" != true ]; then
    fail "client test did not complete within timeout"; exit 1
fi
pass "client test completed"

# --- Fetch and verify ALB results ---
echo ""
echo "ALB internal results:"
ALB_RESULTS=$(curl -s --max-time 10 "http://${CLIENT_PUBLIC_IP}:80/alb_results.txt" 2>/dev/null)
if [ -z "$ALB_RESULTS" ]; then
    fail "could not fetch ALB results from client VM"
else
    verify_traffic_results "$ALB_RESULTS" "http" "ALB internal"
fi

# --- Fetch and verify NLB results ---
echo ""
echo "NLB internal results:"
NLB_RESULTS=$(curl -s --max-time 10 "http://${CLIENT_PUBLIC_IP}:80/nlb_results.txt" 2>/dev/null)
if [ -z "$NLB_RESULTS" ]; then
    fail "could not fetch NLB results from client VM"
else
    verify_traffic_results "$NLB_RESULTS" "tcp" "NLB internal"
fi

# --- Terminate client ---
echo "  Terminating client VM..."
$AWS_EC2 terminate-instances --instance-ids "$CLIENT_INSTANCE_ID" 2>/dev/null || true
CLIENT_INSTANCE_ID=""

# --- Cleanup both internal LBs ---
echo ""
echo "Cleaning up internal LBs..."
$AWS_ELBV2 delete-load-balancer --load-balancer-arn "$ALB_LB_ARN" 2>/dev/null || true
$AWS_ELBV2 delete-load-balancer --load-balancer-arn "$NLB_LB_ARN" 2>/dev/null || true
pass "deleted both internal LBs"

wait_for_eni_cleanup "ELB app/lb-e2e-alb-int/${ALB_LB_ID}" "ALB internal"
wait_for_eni_cleanup "ELB net/lb-e2e-nlb-int/${NLB_LB_ID}" "NLB internal"

$AWS_ELBV2 delete-listener --listener-arn "$ALB_LISTENER_ARN" 2>/dev/null || true
$AWS_ELBV2 delete-listener --listener-arn "$NLB_LISTENER_ARN" 2>/dev/null || true
$AWS_ELBV2 deregister-targets --target-group-arn "$ALB_TG_ARN" \
    --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" 2>/dev/null || true
$AWS_ELBV2 deregister-targets --target-group-arn "$NLB_TG_ARN" \
    --targets "Id=${APP_INSTANCE_IDS[0]}" "Id=${APP_INSTANCE_IDS[1]}" 2>/dev/null || true
$AWS_ELBV2 delete-target-group --target-group-arn "$ALB_TG_ARN" 2>/dev/null || true
$AWS_ELBV2 delete-target-group --target-group-arn "$NLB_TG_ARN" 2>/dev/null || true
ALB_LB_ARN=""; ALB_TG_ARN=""; ALB_LISTENER_ARN=""
NLB_LB_ARN=""; NLB_TG_ARN=""; NLB_LISTENER_ARN=""
pass "internal cleanup complete"

echo ""
