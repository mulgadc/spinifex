package qmpcollector

// MetricsEC2SubjectPrefix is the NATS subject family for per-instance guest
// metrics: metrics.ec2.<instance-id>. Goanna is the eventual consumer; the
// collector's OTLP bridge taps it for the operator plane meanwhile.
//
// The stream is not a Spinifex-owned versioned telemetry contract yet: its
// eventual consumer lives in Goanna. Keep compatibility with the deployed
// subject and JSON shape, but make a cross-repository contract decision before
// treating this local representation as a public API.
const MetricsEC2SubjectPrefix = "metrics.ec2."

// TelemetrySeries is one CloudWatch-mappable datapoint. Names and labels
// follow the Goanna schema: goanna_ec2_* metrics with
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
