# DDIL E2E Test Harness

Go-based E2E harness that exercises Spinifex and Predastore against DDIL (Denied, Disrupted, Intermittent, Limited) failure modes — NATS kill, daemon-without-NATS restart, cluster partition, degraded links, Predastore writes under partition, Raft under SATCOM latency.

## Layout

```
tests/e2e/ddil/
├── doc.go                  # package doc
├── README.md               # this file
├── harness/                # typed primitives (cluster, SSH, daemon client, ...)
└── scenarios/              # TestScenarioA..F, build tag e2e
```

All test and helper files use `//go:build e2e` so `go build ./...` skips them. Run with `-tags=e2e`.

## Running

Against a pre-provisioned 3-node Proxmox cluster:

```bash
DDIL_NODES=10.0.0.1,10.0.0.2,10.0.0.3 \
DDIL_SSH_USER=ubuntu \
DDIL_SSH_KEY=$HOME/.ssh/tf-user-ap-southeast-2 \
AWS_REGION=ap-southeast-2 \
go test -tags=e2e -timeout 45m ./tests/e2e/ddil/scenarios/...
```

Run a single scenario:

```bash
go test -tags=e2e -run TestScenarioA ./tests/e2e/ddil/scenarios/...
```

Dry-run mode (no cluster ops; verifies SSH reachability, tool availability on remote nodes, and runs `TestCoverageDrift` only):

```bash
DDIL_DRY_RUN=1 go test -tags=e2e ./tests/e2e/ddil/scenarios/...
```

Quarantine listed scenarios (failures downgrade to `t.Skip` with a tagged reason):

```bash
DDIL_QUARANTINED=D,F go test -tags=e2e ./tests/e2e/ddil/scenarios/...
```

## Environment variables

| Var | Required | Purpose |
|-----|----------|---------|
| `DDIL_NODES` | yes (unless dry-run) | Comma-separated cluster node IPs, order matches scenario references (node1, node2, node3). |
| `DDIL_SSH_USER` | yes (unless dry-run) | SSH login user on each node. |
| `DDIL_SSH_KEY` | yes (unless dry-run) | Path to SSH private key. |
| `DDIL_DRY_RUN` | no | `1` skips cluster ops, runs `TestCoverageDrift` only. |
| `DDIL_QUARANTINED` | no | Comma-separated scenario letters (A..F) to quarantine. |
| `AWS_REGION` | yes for witness scenarios | Region used by the AWS SDK when launching witness VMs via the Spinifex AWS gateway. |
| `DDIL_WITNESS_AMI` | no | Specific AMI ID for witness VMs. If unset, the harness picks the first `ami-ubuntu-*` image registered in the cluster. |
| `DDIL_WITNESS_INSTANCE_TYPE` | no (default `t2.micro`) | EC2 instance type for witness VMs. |
| `DDIL_WITNESS_KEY_NAME` | no (default `spinifex-key`) | EC2 key pair name attached to witness VMs; must match an existing registered key. |
| `DDIL_GUEST_SSH_USER` | no (default `ubuntu`) | SSH login user inside the witness guest. |
| `DDIL_GUEST_SSH_KEY` | no (defaults to `DDIL_SSH_KEY`) | Private key for guest SSH; override when the guest image uses a different key than the host. |

## Scenario status

Scenario-level status (SKIPPED / ENABLED / QUARANTINED) plus profile validation tracking lives in [`tests/e2e/TEST_COVERAGE.md`](../TEST_COVERAGE.md). Scenarios begin as `t.Skip("requires <dep>")` and are flipped to real assertions by the hardening epics they exercise.
