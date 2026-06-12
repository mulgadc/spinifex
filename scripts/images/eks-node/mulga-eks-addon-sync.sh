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
# auto-deploy dir, substituting the IRSA role ARN. Returns non-zero (and reports
# failed) when the baked bundle for ADDON/VERSION is absent.
render_addon() {
    _addon=$1; _version=$2; _role=$3
    _src="${BAKED_DIR}/${_addon}/${_version}"
    _dst="${DEPLOY_DIR}/${RENDER_PREFIX}${_addon}.yaml"

    if [ ! -d "${_src}" ]; then
        log "no baked bundle for ${_addon}/${_version} (looked in ${_src})"
        report failed "${_addon}" "${_version}" "no baked bundle for version ${_version}"
        return 1
    fi

    _tmp="${_dst}.tmp.$$"
    : > "${_tmp}"
    for _f in "${_src}"/*.yaml; do
        [ -e "${_f}" ] || continue
        sed -e "s|{{SERVICE_ACCOUNT_ROLE_ARN}}|${_role}|g" "${_f}" >> "${_tmp}"
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
