// Package eksv1 defines version 1 of the EKS add-on contract between the
// control-plane guest and the host. v1 is the deployed guest contract; a
// generation-aware delivery protocol needs a successor version, not a v1 edit.
package eksv1

import "fmt"

// AddonStatusSubject returns the per-cluster NATS subject that carries every
// add-on delivery report: "eks.addon.{accountID}.{clusterName}.status".
func AddonStatusSubject(accountID, clusterName string) string {
	return fmt.Sprintf("eks.addon.%s.%s.status", accountID, clusterName)
}

// AddonDeliveryPhase is the guest-observed delivery phase of one add-on. The
// host-side add-on owner maps it onto the AWS-visible add-on status.
type AddonDeliveryPhase string

const (
	// AddonPhaseApplied: the rendered manifest is in the K3s auto-deploy dir
	// but its workloads are not yet Ready.
	AddonPhaseApplied AddonDeliveryPhase = "applied"
	// AddonPhaseReady: the add-on's workloads rolled out successfully.
	AddonPhaseReady AddonDeliveryPhase = "ready"
	// AddonPhaseFailed: render or rollout failed.
	AddonPhaseFailed AddonDeliveryPhase = "failed"
)

// AddonStatusReport is the add-on delivery report payload. The guest builds it
// by hand (always sending "message", even empty) and the gateway relays it
// verbatim onto AddonStatusSubject. TS is the publish time in unix seconds.
type AddonStatusReport struct {
	Addon   string             `json:"addon"`
	Version string             `json:"version"`
	Phase   AddonDeliveryPhase `json:"phase"`
	Message string             `json:"message,omitempty"`
	TS      int64              `json:"ts"`
}
