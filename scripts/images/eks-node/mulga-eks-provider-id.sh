#!/bin/sh
set -eu

# mulga-eks-provider-id — writes a K3s config drop-in that supplies the node
# metadata the aws-ebs-csi-driver needs, read from IMDS at boot: the kubelet
# provider-id (aws:///<az>/<instance-id>) plus the region/zone topology labels
# and the instance-type label.
#
# The node plugin's metadata init first tries IMDS, whose ENI path the spinifex
# IMDS does not serve, then falls back to Kubernetes: instance ID from
# .spec.providerID, region/zone from the well-known topology labels, and the
# instance type from node.kubernetes.io/instance-type. K3s runs no AWS
# cloud-provider, so none of those are set unless kubelet is told. Without the
# topology labels NodeGetInfo never runs, the CSINode carries no topology key,
# and the provisioner fails "no topology key found"; without a parseable
# instance type NodeGetInfo panics in IsNitroInstanceType ("cannot determine
# family"). This drop-in supplies all four; region is derived from the AZ
# (trailing zone letters stripped).
#
# K3s merges /etc/rancher/k3s/config.yaml.d/*.yaml over config.yaml; list keys
# such as kubelet-arg are concatenated, so this adds the flag without touching
# the cloud-init-written main config. Role-agnostic: runs on server + agent.

IMDS="http://169.254.169.254/latest"
DROPIN_DIR=/etc/rancher/k3s/config.yaml.d
DROPIN="${DROPIN_DIR}/10-provider-id.yaml"
LOGTAG="mulga-eks-provider-id"

log() { echo "[${LOGTAG}] $*"; }

if [ -f "${DROPIN}" ]; then
    log "drop-in ${DROPIN} present — already configured"
    exit 0
fi

# IMDSv2 token if the service enforces it; tokenless (v1) is the fallback.
fetch() {
    if [ -n "${_tok}" ]; then
        curl -fsS --max-time 3 -H "X-aws-ec2-metadata-token: ${_tok}" "${IMDS}/meta-data/$1" 2>/dev/null
    else
        curl -fsS --max-time 3 "${IMDS}/meta-data/$1" 2>/dev/null
    fi
}

# Retry: the IMDS on-link route + networking may settle shortly after boot.
iid=""
az=""
itype=""
i=0
while [ "${i}" -lt 30 ]; do
    _tok=$(curl -fsS --max-time 3 -X PUT "${IMDS}/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 300" 2>/dev/null || true)
    iid=$(fetch instance-id || true)
    az=$(fetch placement/availability-zone || true)
    itype=$(fetch instance-type || true)
    case "${iid}" in
        i-*) [ -n "${az}" ] && break ;;
    esac
    iid=""
    az=""
    i=$((i + 1))
    sleep 2
done

if [ -z "${iid}" ] || [ -z "${az}" ]; then
    log "IMDS instance-id/availability-zone unavailable after retries; provider-id not set"
    exit 0
fi

# Region = AZ with the trailing zone letter(s) removed (ap-southeast-2a ->
# ap-southeast-2). The CSI metadata fallback reads it from the region label.
region=$(printf '%s' "${az}" | sed 's/[a-z]*$//')

# instance-type must parse as family.size or the node plugin panics in
# IsNitroInstanceType. Fall back to a generic type only to keep it parseable;
# the real spinifex type (e.g. sys.medium) already satisfies the format.
case "${itype}" in
    *.*) : ;;
    *) itype="m5.large" ;;
esac

mkdir -p "${DROPIN_DIR}"
cat > "${DROPIN}" <<EOF
kubelet-arg:
  - "provider-id=aws:///${az}/${iid}"
node-label:
  - "topology.kubernetes.io/region=${region}"
  - "topology.kubernetes.io/zone=${az}"
  - "node.kubernetes.io/instance-type=${itype}"
EOF
log "wrote provider-id aws:///${az}/${iid}, region=${region} zone=${az} type=${itype}"
