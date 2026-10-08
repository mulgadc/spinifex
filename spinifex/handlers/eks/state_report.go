package handlers_eks

import (
	"encoding/json"
	"fmt"

	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
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
	// NodegroupReady is the Ready-node count grouped by the
	// eks.amazonaws.com/nodegroup label value, so each nodegroup's readiness can
	// be gated on its OWN workers rather than the cluster-wide total. Nil from an
	// older AMI that predates per-nodegroup reporting.
	NodegroupReady map[string]int `json:"nodegroup_ready,omitempty"`
	// FsyncMs is the guest's mean etcd WAL fsync in milliseconds, reported on
	// every state report rather than only unhealthy ones so a stalled control
	// plane has a healthy baseline to be read against. Nil when the guest had
	// too few fsyncs to average, when etcd served no metrics, or from an older
	// AMI that predates the field — none of which is the same as "fast".
	FsyncMs *float64 `json:"fsync_ms,omitempty"`
}

// slowFsyncMs is the mean WAL fsync above which the control plane's disk is
// reported as the suspect regardless of whether the apiserver still answers.
// etcd's own guidance puts a healthy p99 in single-digit milliseconds; a *mean*
// this high means the datastore is being served slowly enough that an apiserver
// timeout is a consequence rather than a cause.
const slowFsyncMs = 25.0

// SlowFsync reports whether the guest's mean etcd fsync is high enough to
// explain control-plane trouble on its own. False when no sample was taken.
func (s ServerStateReport) SlowFsync() bool {
	return s.FsyncMs != nil && *s.FsyncMs >= slowFsyncMs
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

func unmarshalAddonStatusReport(data []byte) (eksv1.AddonStatusReport, error) {
	var r eksv1.AddonStatusReport
	if err := json.Unmarshal(data, &r); err != nil {
		return eksv1.AddonStatusReport{}, fmt.Errorf("eks: unmarshal addon status report: %w", err)
	}
	return r, nil
}
