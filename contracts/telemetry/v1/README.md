# Telemetry v1 contracts

`GuestTelemetryMeta` is the JSON sidecar contract between the VM runtime and
the qmp-collector role. The writer creates and refreshes
`qmp-telemetry-<instance-id>.json`; the collector reads it to locate the
dedicated QMP socket, determine the monitoring interval, and select VPC data
plane taps. Do not change this JSON shape incompatibly; add a new version when
required.

The `metrics.ec2.*` NATS batch schema is deliberately not owned here yet.
Goanna consumes that stream in another repository, so its ownership requires a
cross-repository contract decision rather than a Spinifex-only move.
