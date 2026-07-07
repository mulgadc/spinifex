package types

// GuestTelemetryMeta is the qmp-collector's per-VM discovery record: written
// as qmp-telemetry-<instance-id>.json in the runtime dir next to the VM's
// telemetry QMP socket, rewritten on ENI hot-plug, removed on stop/terminate.
type GuestTelemetryMeta struct {
	InstanceID string `json:"instance_id"`
	AccountID  string `json:"account_id,omitempty"`
	VCPUs      int    `json:"vcpus"`
	// PeriodSeconds is the collection interval: 60 for detailed monitoring,
	// 300 for basic — the EC2 monitoring tiers.
	PeriodSeconds int `json:"period_seconds"`
	// Taps are the VPC data-plane tap devices whose host-side counters
	// yield NetworkIn/Out. Control-plane NICs (mgmt, dev hostfwd) excluded.
	Taps   []string `json:"taps,omitempty"`
	Socket string   `json:"socket"`
}

// MetricsEC2SubjectPrefix is the NATS subject family for per-instance guest
// metrics: metrics.ec2.<instance-id>. Goanna is the eventual consumer; the
// collector's OTLP bridge taps it for the operator plane meanwhile.
const MetricsEC2SubjectPrefix = "metrics.ec2."

// TelemetrySeries is one CloudWatch-mappable datapoint. Names and labels
// follow the schema locked in the Goanna plan: goanna_ec2_* metrics with
// namespace/instance_id/account_id labels.
type TelemetrySeries struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
	Unit   string            `json:"unit,omitempty"`
}

// TelemetryBatch is one collection tick for one instance on metrics.ec2.<id>.
type TelemetryBatch struct {
	TS            int64             `json:"ts"`
	PeriodSeconds int               `json:"period_seconds"`
	Node          string            `json:"node,omitempty"`
	Series        []TelemetrySeries `json:"series"`
}
