#!/bin/sh
set -eu

# mulga-eks-addon-sync — renders Spinifex managed-addon bundles into the K3s
# auto-deploy dir and reports per-addon delivery status back to the spinifex
# control plane. The host-side daemon stages each CreateAddon as a manifest
# descriptor in KV; the VM cannot read KV directly, so this agent pulls the
# staged set through the AWS gateway (eks-gateway-fetch, SigV4) and renders the
# baked bundle for each one. Status flows back via eks-gateway-publish on the
# "addon" channel, where the reconciler CASes the AddonRecord CREATING→ACTIVE.
#
# Pull   (host→VM): GET /clusters/{cluster}/internal-addons/{accountId}
# Report (VM→host): eks.addon.{accountID}.{clusterName}.status
#
# Runs server-role only (it owns the single auto-deploy dir); enabled by
# eks-node-role.sh. Loops every ADDON_SYNC_INTERVAL seconds in service mode,
# one-shot otherwise.

ENVFILE=/etc/spinifex-eks/first-boot.env
[ -f "${ENVFILE}" ] || { logger -t mulga-eks-addon-sync "${ENVFILE} missing — exiting"; exit 0; }
# set -a so the sourced creds are exported to the eks-gateway-{fetch,publish}
# children, which read EKS_ACCOUNT_ID etc. from their environment.
set -a
# shellcheck disable=SC1090
. "${ENVFILE}"
set +a

: "${EKS_GATEWAY_URL:?}"
: "${EKS_ADDON_GATEWAY_URL:?}"
: "${EKS_ACCESS_KEY:?}"
: "${EKS_SECRET_KEY:?}"
: "${EKS_ACCOUNT_ID:?}"
: "${EKS_CLUSTER_NAME:?}"

KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
export KUBECONFIG

# Baked bundles live at BAKED_DIR/<addon>/<version>/*.yaml (air-gapped, shipped
# in the AMI). Rendered output lands in the K3s auto-deploy dir under a stable
# per-addon filename so GC can find orphans this agent owns.
BAKED_DIR=/usr/share/spinifex-eks/addons
DEPLOY_DIR=/var/lib/rancher/k3s/server/manifests
RENDER_PREFIX=spinifex-addon-
# Per-cluster admission-webhook serving certs are minted once and cached here so
# the rendered manifest is byte-stable across sync ticks (a churning cert would
# re-apply the addon every tick).
WEBHOOK_CERT_DIR=/var/lib/spinifex-eks/webhook-certs

# log mirrors to syslog (on-VM /var/log) and, best-effort, to the serial
# console — the host captures ttyS0 to a per-instance log, so this is the only
# delivery diagnostics channel reachable from the host (the VM has no
# host-routable address). Keep messages terse; one line per sync outcome.
log() {
    logger -t mulga-eks-addon-sync "$*"
    echo "[mulga-eks-addon-sync] $*" > /dev/console 2>/dev/null || true
}

# report PHASE ADDON VERSION [MESSAGE] — publish one addon status report.
report() {
    _phase=$1; _addon=$2; _version=$3; _msg=${4:-}
    printf '{"addon":"%s","version":"%s","phase":"%s","message":"%s","ts":%s}' \
        "${_addon}" "${_version}" "${_phase}" "${_msg}" "$(date +%s)" \
        | eks-gateway-publish -channel addon 2>&1 | logger -t mulga-eks-addon-sync || true
}

# render_addon ADDON VERSION ROLE_ARN — render the baked bundle into the
# auto-deploy dir, substituting the IRSA role ARN plus the cluster/gateway
# placeholders an in-cluster AWS-SDK addon (e.g. the LB controller) needs to
# reach the gateway. Returns non-zero (and reports failed) when the baked bundle
# for ADDON/VERSION is absent. Placeholders not present in a bundle are no-ops.
render_addon() {
    _addon=$1; _version=$2; _role=$3
    _src="${BAKED_DIR}/${_addon}/${_version}"
    _dst="${DEPLOY_DIR}/${RENDER_PREFIX}${_addon}.yaml"

    if [ ! -d "${_src}" ]; then
        log "no baked bundle for ${_addon}/${_version} (looked in ${_src})"
        report failed "${_addon}" "${_version}" "no baked bundle for version ${_version}"
        return 1
    fi

    # Gateway CA as single-line base64 for a kube Secret .data entry the addon
    # mounts as AWS_CA_BUNDLE. tr -d so busybox base64 (no -w0) stays one line.
    _ca_b64=""
    if [ -n "${EKS_GATEWAY_CA:-}" ] && [ -f "${EKS_GATEWAY_CA}" ]; then
        _ca_b64=$(base64 < "${EKS_GATEWAY_CA}" | tr -d '\n')
    fi

    # Webhook serving cert (opt-in): a bundle ships webhook.conf with its service
    # SANs when it runs an admission webhook. Mint/reuse a self-signed cert on the
    # node (the AMI has no openssl) and embed it as the Secret + every caBundle.
    _wh_ca=""; _wh_crt=""; _wh_key=""
    if [ -f "${_src}/webhook.conf" ]; then
        WEBHOOK_CN=""; WEBHOOK_DNS=""
        # shellcheck disable=SC1090,SC1091
        . "${_src}/webhook.conf"
        if [ -n "${WEBHOOK_CN}" ] && [ -n "${WEBHOOK_DNS}" ]; then
            _wh_err="${_dst}.whcert.$$"
            if ! _wh_out=$(eks-webhook-cert -dir "${WEBHOOK_CERT_DIR}/${_addon}" \
                    -cn "${WEBHOOK_CN}" -dns "${WEBHOOK_DNS}" 2>"${_wh_err}"); then
                log "addon ${_addon}: webhook cert gen failed: $(tr '\n' ' ' < "${_wh_err}")"
                rm -f "${_wh_err}"
                report failed "${_addon}" "${_version}" "webhook cert generation failed"
                return 1
            fi
            rm -f "${_wh_err}"
            _wh_ca=$(printf '%s' "${_wh_out}" | cut -f1)
            _wh_crt=$(printf '%s' "${_wh_out}" | cut -f2)
            _wh_key=$(printf '%s' "${_wh_out}" | cut -f3)
        fi
    fi

    # Reusable AWS-credential injection. A bundle opts in by placing the
    # {{IRSA_ENV}} / {{IRSA_VOLUME}} / {{IRSA_VOLUME_MOUNT}} markers on their own
    # line; awk swaps each for the standard block, then the sed pass fills the
    # nested role/region/gateway placeholders. Bundles without the markers are
    # untouched. Blocks assume a Deployment pod spec (env 8sp, volumes 6sp,
    # volumeMounts 8sp).
    #
    # Gateway-CA trust is injected in both modes: AWS_CA_BUNDLE covers the
    # standard-config clients, and SSL_CERT_FILE adds the gateway CA to the Go
    # system trust pool. The LBC ec2/elbv2/acm clients install their own
    # per-client BaseEndpoint resolver and build a fresh HTTP client that
    # ignores AWS_CA_BUNDLE, falling back to the system pool — so without
    # SSL_CERT_FILE they reject the gateway cert as "unknown authority".
    #
    # IRSA (web-identity) env is injected ONLY when a service-account role ARN is
    # staged. The token (aud sts.amazonaws.com) + role ARN + gateway STS endpoint
    # are what AssumeRoleWithWebIdentity needs. Without a role ARN, those are
    # omitted entirely: injecting an empty AWS_ROLE_ARN beside the token file
    # half-arms the SDK web-identity provider, which then signs anonymously
    # instead of falling through the default chain to the node instance-profile
    # credentials served over IMDS. Omitting them restores that node-credential
    # fallback.
    if [ -n "${_role}" ]; then
        _irsa_env='        - name: AWS_ROLE_ARN
          value: "{{SERVICE_ACCOUNT_ROLE_ARN}}"
        - name: AWS_WEB_IDENTITY_TOKEN_FILE
          value: /var/run/secrets/eks.amazonaws.com/serviceaccount/token
        - name: AWS_STS_REGIONAL_ENDPOINTS
          value: regional
        - name: AWS_REGION
          value: "{{AWS_REGION}}"
        - name: AWS_DEFAULT_REGION
          value: "{{AWS_REGION}}"
        - name: AWS_ENDPOINT_URL_STS
          value: "{{GATEWAY_ENDPOINT}}"
        - name: AWS_CA_BUNDLE
          value: /etc/spinifex/gateway-ca/ca.pem
        - name: SSL_CERT_FILE
          value: /etc/spinifex/gateway-ca/ca.pem'
        _irsa_vol='      - name: aws-iam-token
        projected:
          defaultMode: 420
          sources:
          - serviceAccountToken:
              audience: sts.amazonaws.com
              expirationSeconds: 86400
              path: token
      - name: spinifex-gateway-ca
        secret:
          defaultMode: 420
          secretName: spinifex-gateway-ca'
        _irsa_mnt='        - name: aws-iam-token
          mountPath: /var/run/secrets/eks.amazonaws.com/serviceaccount
          readOnly: true
        - name: spinifex-gateway-ca
          mountPath: /etc/spinifex/gateway-ca
          readOnly: true'
    else
        _irsa_env='        - name: AWS_REGION
          value: "{{AWS_REGION}}"
        - name: AWS_DEFAULT_REGION
          value: "{{AWS_REGION}}"
        - name: AWS_CA_BUNDLE
          value: /etc/spinifex/gateway-ca/ca.pem
        - name: SSL_CERT_FILE
          value: /etc/spinifex/gateway-ca/ca.pem'
        _irsa_vol='      - name: spinifex-gateway-ca
        secret:
          defaultMode: 420
          secretName: spinifex-gateway-ca'
        _irsa_mnt='        - name: spinifex-gateway-ca
          mountPath: /etc/spinifex/gateway-ca
          readOnly: true'
    fi

    # IngressClassParams spec injection. EKS_ELB_SUBNET_IDS is the cluster's
    # ELB-eligible subnets (CSV, deduped to one per AZ by the daemon). Inject them
    # as explicit .spec.subnets.ids so every Ingress takes LBC's explicit-subnet
    # path, the only one that honors ALBSingleSubnet; a single-AZ cluster otherwise
    # fails reconcile. Empty value drops the marker line, leaving tag auto-discovery.
    _icp_spec=''
    if [ -n "${EKS_ELB_SUBNET_IDS:-}" ]; then
        _icp_spec='  spec:
    subnets:
      ids:'
        _oldifs=$IFS
        IFS=','
        for _sn in ${EKS_ELB_SUBNET_IDS}; do
            [ -n "${_sn}" ] || continue
            _icp_spec="${_icp_spec}
      - ${_sn}"
        done
        IFS=$_oldifs
    fi

    _tmp="${_dst}.tmp.$$"
    : > "${_tmp}"
    for _f in "${_src}"/*.yaml; do
        [ -e "${_f}" ] || continue
        # Each block marker must be the ONLY token on its line (whitespace aside);
        # matching a substring would also fire on a comment that merely names the
        # marker, injecting the block mid-document and producing invalid YAML.
        awk -v env="${_irsa_env}" -v vol="${_irsa_vol}" -v mnt="${_irsa_mnt}" -v icp="${_icp_spec}" '
            $0 ~ /^[[:space:]]*[{][{]IRSA_ENV[}][}][[:space:]]*$/                { print env; next }
            $0 ~ /^[[:space:]]*[{][{]IRSA_VOLUME_MOUNT[}][}][[:space:]]*$/       { print mnt; next }
            $0 ~ /^[[:space:]]*[{][{]IRSA_VOLUME[}][}][[:space:]]*$/             { print vol; next }
            $0 ~ /^[[:space:]]*[{][{]ELB_INGRESS_PARAMS_SPEC[}][}][[:space:]]*$/ { if (icp != "") print icp; next }
            { print }
        ' "${_f}" \
        | sed -e "s|{{SERVICE_ACCOUNT_ROLE_ARN}}|${_role}|g" \
            -e "s|{{CLUSTER_NAME}}|${EKS_CLUSTER_NAME}|g" \
            -e "s|{{AWS_REGION}}|${EKS_REGION:-}|g" \
            -e "s|{{AWS_VPC_ID}}|${EKS_VPC_ID:-}|g" \
            -e "s|{{GATEWAY_ENDPOINT}}|${EKS_ADDON_GATEWAY_URL}|g" \
            -e "s|{{GATEWAY_CA_PEM_B64}}|${_ca_b64}|g" \
            -e "s|{{WEBHOOK_CA_B64}}|${_wh_ca}|g" \
            -e "s|{{WEBHOOK_TLS_CRT_B64}}|${_wh_crt}|g" \
            -e "s|{{WEBHOOK_TLS_KEY_B64}}|${_wh_key}|g" \
            >> "${_tmp}"
        printf '\n---\n' >> "${_tmp}"
    done
    # Idempotent: only replace the auto-deploy file when the rendered content
    # actually changed. An unconditional mv bumps the file mtime every tick,
    # which K3s' auto-deploy controller treats as a change and re-applies —
    # clearing the Addon CR's .status.gvks each time, so the same-tick readiness
    # check never observes a settled apply and the add-on stays CREATING forever.
    if [ -e "${_dst}" ] && cmp -s "${_tmp}" "${_dst}"; then
        rm -f "${_tmp}"
        return 0
    fi
    mv -f "${_tmp}" "${_dst}"
    return 0
}

# addon_gvks ADDON — echoes the GVKs K3s' auto-deploy controller applied for the
# rendered manifest, empty until it has applied the objects. A non-empty value
# means delivery succeeded. Addon-agnostic — confirms delivery, not deep per-pod
# health (a later hardening item).
#
# K3s (v1.32.x) records the applied GVKs in the addon.k3s.cattle.io/gvks
# *annotation* (e.g. "/v1, Kind=Namespace;/v1, Kind=ConfigMap"); the Addon CR
# carries no .status subresource. The CR is matched by .spec.source (the
# absolute path of the rendered file) rather than a guessed CR name, since K3s
# derives the name from the path with its own transform.
addon_gvks() {
    _want="${DEPLOY_DIR}/${RENDER_PREFIX}$1.yaml"
    kubectl -n kube-system get addon -o json 2>/dev/null \
        | jq -r --arg src "${_want}" \
            '.items[] | select(.spec.source==$src)
             | .metadata.annotations["addon.k3s.cattle.io/gvks"] // ""' 2>/dev/null \
        | grep -v '^$' | head -1 || true
}

# diag_addon ADDON — one-shot console dump of the live K3s state for a stuck
# add-on, so a delivery that never reaches ready is diagnosable from the
# host-captured serial console (the VM is otherwise unreachable). Fires once per
# process via a sentinel to avoid spamming every tick.
diag_addon() {
    [ -f /tmp/.addon-diag-"$1" ] && return 0
    : > /tmp/.addon-diag-"$1"
    {
        echo "----- addon-diag ${1} -----"
        echo "[deploy dir]"; ls -l "${DEPLOY_DIR}" 2>&1
        echo "[rendered file head]"; head -40 "${DEPLOY_DIR}/${RENDER_PREFIX}${1}.yaml" 2>&1
        echo "[all addon CRs]"; kubectl get addon -A 2>&1
        echo "[addon CR ${RENDER_PREFIX}${1} -o yaml]"; kubectl -n kube-system get addon "${RENDER_PREFIX}${1}" -o yaml 2>&1
        echo "[namespace ${1}]"; kubectl get ns "${1}" 2>&1
        echo "[configmaps in ${1}]"; kubectl -n "${1}" get cm 2>&1
        echo "----- end addon-diag ${1} -----"
    } > /dev/console 2>&1 || true
}

sync_once() {
    _staged=$(mktemp)
    _ferr=$(mktemp)
    if ! eks-gateway-fetch -resource addons > "${_staged}" 2>"${_ferr}"; then
        log "fetch staged addons failed — keeping current state: $(tr '\n' ' ' < "${_ferr}")"
        rm -f "${_staged}" "${_ferr}"
        return 0
    fi
    rm -f "${_ferr}"

    # Names currently staged, for the GC pass below.
    _names=$(cut -f1 "${_staged}")
    _count=$(grep -c . "${_staged}" 2>/dev/null || echo 0)
    log "fetch ok: ${_count} staged addon(s)"

    # Render/refresh each staged addon and report its phase.
    while IFS=$(printf '\t') read -r _addon _version _role _cfg_b64; do
        [ -n "${_addon}" ] || continue
        : "${_cfg_b64:-}"  # config values reserved for per-addon templating (follow-up)
        if render_addon "${_addon}" "${_version}" "${_role}"; then
            _gvks=$(addon_gvks "${_addon}")
            if [ -n "${_gvks}" ]; then
                log "addon ${_addon}/${_version}: applied + ready (gvks=${_gvks})"
                report ready "${_addon}" "${_version}"
            else
                log "addon ${_addon}/${_version}: applied, not ready yet (no K3s Addon CR for ${RENDER_PREFIX}${_addon}.yaml with populated .status.gvks)"
                diag_addon "${_addon}"
                report applied "${_addon}" "${_version}"
            fi
        fi
    done < "${_staged}"
    rm -f "${_staged}"

    # GC: remove rendered manifests this agent owns whose addon is no longer
    # staged (DeleteAddon removed its descriptor). K3s does NOT delete the applied
    # objects when the manifest file disappears, so delete them explicitly.
    for _file in "${DEPLOY_DIR}/${RENDER_PREFIX}"*.yaml; do
        [ -e "${_file}" ] || continue
        _base=$(basename "${_file}")
        _name=${_base#"${RENDER_PREFIX}"}
        _name=${_name%.yaml}
        if ! printf '%s\n' "${_names}" | grep -qx "${_name}"; then
            log "unstaging addon ${_name} (no longer staged)"
            # Copy the manifest aside, drop the original so K3s stops tracking it
            # (and never re-applies), then delete the objects it rendered.
            # --wait=false so namespace finalizers don't stall the sync loop.
            _gc=$(mktemp)
            cp "${_file}" "${_gc}"
            rm -f "${_file}"
            kubectl delete -f "${_gc}" --ignore-not-found --wait=false 2>&1 \
                | logger -t mulga-eks-addon-sync || true
            rm -f "${_gc}"
        fi
    done
}

mkdir -p "${DEPLOY_DIR}"

if [ -n "${ADDON_SYNC_INTERVAL:-}" ] && [ "${ADDON_SYNC_INTERVAL}" -gt 0 ] 2>/dev/null; then
    log "starting: cluster=${EKS_CLUSTER_NAME} account=${EKS_ACCOUNT_ID} interval=${ADDON_SYNC_INTERVAL}s gateway=${EKS_GATEWAY_URL}"
    while true; do
        sync_once
        sleep "${ADDON_SYNC_INTERVAL}"
    done
else
    sync_once
fi
