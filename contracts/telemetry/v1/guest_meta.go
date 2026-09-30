// Package telemetryv1 defines version 1 of cross-process telemetry contracts.
package telemetryv1

// GuestTelemetryMeta is the qmp-collector's per-VM discovery record. The VM
// runtime writes qmp-telemetry-<instance-id>.json next to the dedicated
// telemetry QMP socket; qmp-collector reads and refreshes it after VM or ENI
// lifecycle changes.
type GuestTelemetryMeta struct {
	InstanceID string `json:"instance_id"`
	AccountID  string `json:"account_id,omitempty"`
	VCPUs      int    `json:"vcpus"`
	// PeriodSeconds is the collection interval: 60 for detailed monitoring,
	// 300 for basic — the EC2 monitoring tiers.
	PeriodSeconds int `json:"period_seconds"`
	// Taps are the VPC data-plane tap devices whose host-side counters yield
	// NetworkIn/Out. Control-plane NICs (mgmt, dev hostfwd) are excluded.
	Taps   []string `json:"taps,omitempty"`
	Socket string   `json:"socket"`
}
