// Package agent implements rds-agent, which runs inside a Spinifex RDS
// instance. It registers the VM with the control plane over TLS+SigV4 (never
// NATS), writes the bootstrap handoff the engine's first-boot script consumes,
// then heartbeats health and polls for directives.
//
// It is the only path by which a secret reaches the VM: the master password is
// served once, to an authenticated caller. Static config is read from the
// cloud-init env file /etc/spinifex-rds/agent.env; real env vars override it.
package agent
