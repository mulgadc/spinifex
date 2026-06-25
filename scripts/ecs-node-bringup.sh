#!/usr/bin/env bash
# ecs-node-bringup.sh — launch a Spinifex ECS container-instance VM.
#
# ECS container instances are plain EC2 instances booted from the spinifex-ecs-node
# AMI (Alpine + containerd + ecs-agent), AWS-faithful: there is no ECS-specific
# launch API. This script renders the cloud-init user-data that seeds the agent's
# control-plane config + IAM credentials into /etc/spinifex-ecs/agent.env and the
# gateway TLS CA into /etc/spinifex-ecs/gateway-ca.pem, then runs the instance.
#
# The agent then registers, heartbeats, polls assignments, and reports task state
# entirely through the AWS gateway over TLS+SigV4 — it never touches NATS.
#
# Usage:
#   AWS_PROFILE=spinifex scripts/ecs-node-bringup.sh <cluster-name>
#
# Env overrides:
#   GATEWAY_IP   gateway IP the guest dials        (default: host WAN-bridge IP)
#   GATEWAY_CA   host path to the gateway CA PEM   (default /etc/spinifex/ca.pem)
#   AMI_ID       ECS-node AMI                      (default: latest spinifex-ecs-node)
#   SUBNET_ID / SG_ID / KEY_NAME / INSTANCE_TYPE   (default: first available / t3.small)
set -euo pipefail

CLUSTER="${1:?usage: ecs-node-bringup.sh <cluster-name>}"
PROFILE="${AWS_PROFILE:-spinifex}"
REGION="$(aws configure get region --profile "$PROFILE")"

# VPC guests NAT out through the WAN and reach host-run services on the host's
# WAN-bridge IP; the mgmt bridge (10.15.8.x) is not routable from a guest netns.
# Default to the br-wan address, falling back to the mgmt IP if it can't be read.
default_gateway_ip() {
  ip -4 addr show br-wan 2>/dev/null | awk '/inet /{print $2; exit}' | cut -d/ -f1
}
GATEWAY_IP="${GATEWAY_IP:-$(default_gateway_ip)}"
GATEWAY_IP="${GATEWAY_IP:-10.15.8.1}"
GATEWAY_CA="${GATEWAY_CA:-/etc/spinifex/ca.pem}"
INSTANCE_TYPE="${INSTANCE_TYPE:-t3.small}"

awscli() { aws --profile "$PROFILE" "$@"; }

AMI_ID="${AMI_ID:-$(awscli ec2 describe-images \
  --filters 'Name=name,Values=spinifex-ecs-node' \
  --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)}"
SUBNET_ID="${SUBNET_ID:-$(awscli ec2 describe-subnets \
  --query 'Subnets[0].SubnetId' --output text)}"
SG_ID="${SG_ID:-$(awscli ec2 describe-security-groups \
  --query 'SecurityGroups[0].GroupId' --output text)}"
KEY_NAME="${KEY_NAME:-$(awscli ec2 describe-key-pairs \
  --query 'KeyPairs[0].KeyName' --output text)}"

ACCESS_KEY="$(aws configure get aws_access_key_id --profile "$PROFILE")"
SECRET_KEY="$(aws configure get aws_secret_access_key --profile "$PROFILE")"

echo "cluster=$CLUSTER ami=$AMI_ID subnet=$SUBNET_ID sg=$SG_ID key=$KEY_NAME gw=https://$GATEWAY_IP:9999" >&2

# Indent a file's content by six spaces for a cloud-init "content: |" block.
indent6() { sed 's/^/      /' "$1"; }

USERDATA="$(cat <<EOF
#cloud-config
bootcmd:
  - rm -f /etc/resolv.conf
  - printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
write_files:
  - path: /etc/spinifex-ecs/agent.env
    permissions: '0600'
    content: |
      ECS_GATEWAY_URL=https://$GATEWAY_IP:9999
      ECS_GATEWAY_CA=/etc/spinifex-ecs/gateway-ca.pem
      ECS_REGION=$REGION
      ECS_CLUSTER=$CLUSTER
      ECS_ACCESS_KEY=$ACCESS_KEY
      ECS_SECRET_KEY=$SECRET_KEY
  - path: /etc/spinifex-ecs/gateway-ca.pem
    permissions: '0644'
    content: |
$(indent6 "$GATEWAY_CA")
EOF
)"

INSTANCE_ID="$(awscli ec2 run-instances \
  --image-id "$AMI_ID" --instance-type "$INSTANCE_TYPE" \
  --key-name "$KEY_NAME" --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
  --user-data "$USERDATA" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=ecs-node-$CLUSTER}]" \
  --query 'Instances[0].InstanceId' --output text)"

echo "$INSTANCE_ID"
