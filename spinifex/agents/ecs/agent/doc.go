// Package agent implements ecs-agent, which runs inside a Spinifex ECS
// container instance (a guest VM booted from the ECS-AMI, with containerd baked
// in). It registers the host with the ECS scheduler through the AWS gateway over
// TLS+SigV4 (never NATS), heartbeats by re-registering, polls the gateway for
// task assignments, runs them through containerd, and reports state back over
// the gateway.
//
// Static config (gateway URL, CA, region, cluster, seeded IAM creds, containerd
// socket) is read from the cloud-init env file /etc/spinifex-ecs/agent.env
// (KEY=value); real env vars override it. Host identity (account, instance, AZ)
// comes from IMDS at boot.
package agent
