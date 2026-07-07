package handlers_eks

import (
	"encoding/json"
	"fmt"
)

// StateSubject returns the NATS subject the control-plane VM publishes its
// periodic health self-report to: "eks.state.{accountID}.{clusterName}.server".
// The CP VM is on the management NATS bus, so this report reaches the host-side
// reconciler without any route into the VPC overlay — unlike an HTTP /healthz
// probe to the apiserver, which k3s binds to the unreachable VPC node-ip.
func StateSubject(accountID, clusterName string) string {
	return fmt.Sprintf("eks.state.%s.%s.server", accountID, clusterName)
}

// ServerStateReport is the JSON payload of a control-plane state report, emitted
// by scripts/images/eks-node/mulga-eks-state-report.sh. Healthz mirrors the
// apiserver's /healthz body ("ok" when serving); NodeCount is the number of
// nodes the apiserver reports (server + joined agents); TS is the publish time
// in unix seconds, used to reject stale reports from a CP that stopped emitting.
type ServerStateReport struct {
	Healthz   string `json:"healthz"`
	NodeCount int    `json:"node_count"`
	TS        int64  `json:"ts"`
	// Reason is a compact in-guest diagnosis emitted only when the apiserver is
	// unhealthy (failing /readyz subchecks, etcd reachability, etcd-disk free).
	// Empty from a healthy CP or an older AMI that predates the field.
	Reason string `json:"reason,omitempty"`
}

// Healthy reports whether the apiserver was serving at publish time.
func (s ServerStateReport) Healthy() bool { return s.Healthz == "ok" }

func unmarshalServerStateReport(data []byte) (ServerStateReport, error) {
	var r ServerStateReport
	if err := json.Unmarshal(data, &r); err != nil {
		return ServerStateReport{}, fmt.Errorf("eks: unmarshal server state report: %w", err)
	}
	return r, nil
}

// AddonStatusSubject returns the NATS subject the control-plane VM publishes a
// per-add-on delivery status to: "eks.addon.{accountID}.{clusterName}.status".
// One subject per cluster carries reports for every add-on (Addon names the
// add-on); the host-side AddonStatusReconciler CASes the matching AddonRecord.
// Distinct from StateSubject because add-on lifecycle is tracked per-resource,
// mirroring AWS EKS (DescribeAddon.status is independent of cluster status).
func AddonStatusSubject(accountID, clusterName string) string {
	return fmt.Sprintf("eks.addon.%s.%s.status", accountID, clusterName)
}

// AddonDeliveryPhase is the VM-observed delivery phase the on-VM addon-sync
// agent reports for one managed add-on. The reconciler maps it onto the
// AWS-visible AddonStatus.
type AddonDeliveryPhase string

const (
	// AddonPhaseApplied: the rendered manifest was written to the K3s
	// auto-deploy dir but pods are not yet Ready. Record stays CREATING.
	AddonPhaseApplied AddonDeliveryPhase = "applied"
	// AddonPhaseReady: the add-on's workloads rolled out successfully. Record
	// flips to ACTIVE.
	AddonPhaseReady AddonDeliveryPhase = "ready"
	// AddonPhaseFailed: render or rollout failed. Record flips to DEGRADED
	// (CREATE_FAILED if it never reached ACTIVE — decided by the reconciler).
	AddonPhaseFailed AddonDeliveryPhase = "failed"
)

// AddonStatusReport is the JSON payload of an add-on delivery report, emitted
// by scripts/images/eks-node/mulga-eks-addon-sync.sh via the "addon" channel of
// eks-gateway-publish. TS is the publish time in unix seconds.
type AddonStatusReport struct {
	Addon   string             `json:"addon"`
	Version string             `json:"version"`
	Phase   AddonDeliveryPhase `json:"phase"`
	Message string             `json:"message,omitempty"`
	TS      int64              `json:"ts"`
}

func unmarshalAddonStatusReport(data []byte) (AddonStatusReport, error) {
	var r AddonStatusReport
	if err := json.Unmarshal(data, &r); err != nil {
		return AddonStatusReport{}, fmt.Errorf("eks: unmarshal addon status report: %w", err)
	}
	return r, nil
}
