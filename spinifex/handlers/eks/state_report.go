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
