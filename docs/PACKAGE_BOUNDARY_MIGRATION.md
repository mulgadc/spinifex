# Package-boundary migration record

**Status:** Active implementation record.
**Decision:** [Mulga ADR-0001 — Spinifex Package Boundary and Layout](https://github.com/mulgadc/mulga/blob/main/docs/adr/0001-spinifex-package-boundary-layout.md).
**Scope:** Spinifex source layout only. This record does not establish a new
architecture decision, expand a customer-facing interface, or claim that the
current implementation conforms to the target layout.

## Purpose

This document records the implementation sequence for ADR-0001 in this
repository. The ADR owns the target layout and dependency rules; this record
captures the source-level entry-point inventory and the evidence needed for
each narrow structural slice.

The migration proceeds on one long-lived refactor branch. Each commit remains
small, independently buildable and reviewable; that does not require one pull
request per package move.

## Baseline

The inventory below was taken from Spinifex commit
`ecef5e4d2f793b4cf2c96d0a43d0a1ab37be3eed`, the revision pinned by Mulga when
ADR-0001 was accepted. It records ownership standing, not a promise that a
binary or script is AWS-compatible or supported beyond its documented scope.

## Entry-point and automation inventory

### Product operator and delivery entry points

| Current path | Standing | Target ownership | Migration rule |
|---|---|---|---|
| `cmd/spinifex` (`spx`) | Supported operator process | Thin main; command assembly in `spinifex/operator/cli` and individual supported use cases in `spinifex/operator/*` | Preserve binary and supported command compatibility. The CLI must reach a domain through an explicit capability, never its private state. |
| `cmd/installer` | Delivery executable | Thin main; interactive, unattended and host-installation work in `spinifex/operator/installer` | Preserve installation behaviour while separating executable startup from delivery logic. It is not, merely by existing, the permanent administration interface. |

`cmd/spinifex/cmd` and `cmd/installer/*` are presently implementation-bearing
trees, not thin entry points. They are not moved until their individual
receiving owners and test seams are selected.

### Guest and named-role entry points

| Current path | Standing | Target ownership | Evidence of role boundary |
|---|---|---|---|
| `cmd/ecs-agent` | ECS guest agent | Thin main; `spinifex/agents/ecs` | Baked into ECS node images; communicates with the control plane through the AWS gateway, not NATS. |
| `cmd/rds-agent` | RDS guest agent | Thin main; `spinifex/agents/rds` | Runs in an RDS guest, handles bootstrap and engine directives through the gateway. |
| `cmd/lb-agent` | ELBv2 guest agent | Thin main; `spinifex/agents/elbv2` | Included in the load-balancer microVM and controls its local data plane. |
| `cmd/eks-token-webhook` | EKS control-plane guest role | Thin main; `spinifex/agents/eks/tokenwebhook` | Baked into the EKS node image as the Kubernetes TokenReview webhook. |
| `cmd/eks-gateway-fetch` | EKS control-plane guest helper | Thin main; `spinifex/agents/eks/gatewayfetch` | Fetches recovery and addon state through the AWS gateway. |
| `cmd/eks-gateway-publish` | EKS control-plane guest helper | Thin main; `spinifex/agents/eks/gatewaypublish` | Relays bootstrap and state reports through the AWS gateway. |
| `cmd/eks-webhook-cert` | EKS image-resident certificate helper | Thin main; `spinifex/agents/eks/webhookcert` | Invoked by EKS image scripts to create/reuse webhook serving certificates. |
| `cmd/eks-konnectivity-cert` | EKS image-resident certificate helper | Thin main; `spinifex/agents/eks/konnectivitycert` | Invoked by EKS image scripts to create/reuse Konnectivity serving certificates. |
| `cmd/ecr-credential-provider` | EKS node integration | Thin main; `spinifex/agents/eks/credentialprovider` | Kubelet exec credential provider baked into EKS images; it is not ECR control-plane ownership. |

No common agent package is introduced by this inventory. Shared mechanisms are
considered only when the existing role and least-privilege boundaries remain
intact; ADR-0001 explicitly defers that design.

### Development and qualification entry points

| Current path | Standing | Target ownership | Rule |
|---|---|---|---|
| `cmd/aws-model-coverage` | Qualification generator | Thin main; reusable code in `internal/tooling` | Produces the dispatch-versus-model inventory; never imported by product packages. |
| `cmd/dhcptest` | Development diagnostic | Thin main; reusable code in `internal/tooling` if extraction is useful | It is a DHCP probe, not a supported `spx admin` command. |
| `cmd/spx-loadgen` | Qualification/load tool | Thin main; reusable code in `internal/testkit` | It drives test load and is not shipped to nodes. |
| `tests/e2e/*/cmd/*`, `tests/bench/*/cmd/*` | Test and benchmark-private tools | Remain test-private | They are invoked by a test or benchmark harness, not a product process. |
| `docs/terraform-workbooks/demo-app/main.go` | Demonstration workload | Remain with the workbook | It is neither a Spinifex executable nor a product package. |

### Automation standing

| Path class | Standing | Rule during migration |
|---|---|---|
| `.github/scripts/` | CI-private qualification automation | Keep at the module root; the invoking workflow owns the evidence scope. |
| `build/`, `images/`, `scripts/images/`, image build and publish scripts | Delivery and guest-image inputs | Keep at the module root. In-guest helpers stay with their image; do not relocate shell sources into Go packages. |
| `scripts/setup.sh`, `setup-ovn.sh`, `install-node.sh`, `node-reset.sh`, `uninstall-spx.sh` | Transitional delivery and documented host-operation mechanics | Keep at the module root. They may implement a published procedure but are not an enduring public administration API solely because they are installed on a node. |
| `scripts/dev-env/`, `scripts/demo/`, benchmarks, diagnostics and Terraform experiments | Development or exploratory automation | Keep at the module root; they are not customer-facing contracts. |
| `scripts/check-coverage.sh`, `diff-coverage.sh`, `run-gate.sh`, `sync-aws-models.sh` and test harness scripts | Repository and qualification automation | Keep at the module root; their caller defines the contract and evidence. |

## First source move — ARN

The first source move is the cohesive ARN leaf package:

- old import path: `github.com/mulgadc/spinifex/spinifex/arn`
- target import path: `github.com/mulgadc/spinifex/spinifex/foundation/aws/arn`

Source commit `91ac70e29` moves the ten package files and updates all forty-four
Go importers. It changes no behaviour and leaves no forwarding package.

Verification passed with an isolated Go module/build cache:

- `go test ./spinifex/foundation/aws/arn`
- `go test` for the fourteen packages directly importing ARN
- a Go-source search for the old import path returned no matches
- `git diff --check`

The inventory was intentionally committed before this move so a later
thin-main refactor cannot silently choose an owner by convenience.

## Second source move — AWS filters

The second source move removes the generic `filterutil` location and name:

- old import path: `github.com/mulgadc/spinifex/spinifex/filterutil`
- target import path: `github.com/mulgadc/spinifex/spinifex/foundation/aws/filters`

Source commit `e707be942` moves the two package files, renames the Go package
to `filters`, and updates twenty-eight importing files. Callers use the
explicit import alias `awsfilters`: many filter implementations already hold
a local `filters` value, and the alias prevents a package-name collision
without changing that local state or any filter behaviour.

Verification passed with an isolated Go module/build cache:

- `go test ./spinifex/foundation/aws/filters`
- `go test` for the seventeen direct caller packages
- a Go-source search for the old import path returned no matches
- `git diff --check`

## Third source move — AWS Query parser

The third source move names the protocol concern rather than the first service
that happened to use it:

- old import path: `github.com/mulgadc/spinifex/spinifex/awsec2query`
- target import path: `github.com/mulgadc/spinifex/spinifex/ingress/aws/query`

Source commit `caaf98d9e` moves the two parser files, renames the Go package to
`query`, and updates thirteen importing files. The parser is used across EC2,
IAM, STS, ECS, ECR, ELBv2 and RDS, but owns only AWS Query wire decoding and no
service action or resource semantics.

Verification passed with an isolated Go module/build cache:

- `go test ./spinifex/ingress/aws/query`
- `go test -run '^$'` for six direct caller packages
- a Go-source search for the old import path returned no matches
- `git diff --check`

A broad `go test` run over those caller packages was stopped after two minutes
without output. It is not recorded as a passing test; the normal full suite
remains CI evidence for the branch.

## Subsequent source moves

The following slices follow the same rule as the first three: each is a
structural move with import rewrites, no compatibility shim, focused package
and direct-caller validation, and `git diff --check`. They are recorded here
while the branch is in flight; they do not describe the umbrella repository's
`main` until its Spinifex gitlink is updated.

| Source commit | Old path | New path | Finding or boundary recorded |
|---|---|---|---|
| `04b41a62c` | `spinifex/idempotency` | `spinifex/foundation/lifecycle/idempotency` | Generic request coalescing is lifecycle infrastructure, not a domain owner. |
| `25fa01a3d` | `spinifex/preflight` | `spinifex/bootstrap/preflight` | Startup validation is bootstrap input, not a generic service. |
| `681fccc61` | `spinifex/systemd` | `spinifex/operator/host/systemd` | Host unit generation and reconciliation are operator-host work. |
| `23ca5d8aa` | `spinifex/resource` | `spinifex/foundation/lifecycle/resource` | Resource metadata is a generic lifecycle primitive. |
| `7a3c8de07` | `spinifex/nbd` | `spinifex/runtime/compute/nbd` | NBD process control is node-local compute runtime. |
| `ea65616be` | `spinifex/hostdns` | `spinifex/runtime/host/dns` | Host resolver integration is runtime host work. |
| `d80803807` | `spinifex/qmp` | `spinifex/runtime/compute/qmp` | QMP is local VM runtime control. |
| `41e2c1f10` | `spinifex/gpu` | `spinifex/runtime/compute/gpu` | GPU discovery and attachment are local compute runtime work. |
| `d78246d05` | `spinifex/instancetypes` | `spinifex/domains/ec2/instancetypes` | AWS-visible instance-type semantics belong to EC2. |
| `b2ce46124` | `spinifex/service` | `spinifex/runtime/service` | The named-role catalogue is runtime composition. |
| `573ee6761` | `spinifex/loadgen` | `internal/testkit/loadgen` | Load generation is qualification support, not product code. |
| `afdb6618c` | `spinifex/otelsetup` | `spinifex/foundation/telemetry` | Spinifex instrumentation primitives are shared telemetry foundation. |
| `956fe1490` | `spinifex/awserrors` | `spinifex/foundation/aws/errors` | AWS error vocabulary is shared AWS foundation. |
| `cbbf252e6` | `spinifex/testutil` | `internal/testkit` | Test doubles remain compiler-private and cannot become application dependencies. |
| `0c2d64413` | `spinifex/vm` | `spinifex/runtime/compute/vm` | VM lifecycle is node-local compute runtime, not EC2 API ownership. |
| `a74adc48b` | `spinifex/ebsmetadata` | `spinifex/domains/ec2/ebs/metadata` | EBS API metadata remains EC2-domain state. |
| `94ddc91a1` | `spinifex/ebsprovider` | `spinifex/providers/ebs` | The provider contract and conformance suite are replaceable storage adapters. |
| `3c1bce1eb` | `spinifex/cloud/oci` | `spinifex/providers/cloud/oci` | OCI access is a replaceable cloud-provider adapter. |
| `56970d079` | `spinifex/services/nats` | `spinifex/runtime/roles/nats` | NATS process startup is a named runtime role. |
| `d87326f64` | `spinifex/network/listenerinventory` | `internal/testkit/listenerinventory` | Listener inventory is test-only qualification support. |
| `4d4875b13` | `spinifex/services/spinifexui` | `spinifex/runtime/roles/spinifexui` | The web UI is a named runtime role, not a logical AWS domain. |
| `f39bc64c9` | `spinifex/admin` listener helpers | `spinifex/foundation/netaddr` | Listener-address parsing is a small shared network primitive. |
| `9e8e889fb` | `spinifex/services/qemunbdd` | `spinifex/runtime/roles/qemunbdd` | QEMU-NBD provider process startup is a runtime role. |
| `3494ae131` | `spinifex/services/qmpcollector` | `spinifex/runtime/roles/qmpcollector` | QMP collection is a named runtime role. |
| `f64931546` | `spinifex/services/spinifex` | `spinifex/runtime/roles/spinifex` | The core daemon executable role is runtime composition. |
| `57f238a0c` | `spinifex/tags` | `spinifex/foundation/aws/tags` | System-tag vocabulary is shared AWS foundation. |
| `7a6b00728`, `186ef7ebc` | `spinifex/kvutil` | `spinifex/foundation/state/kvutil` | Replica policy was made explicit before moving generic KV utilities to state foundation. |
| `57f9d2ccc` | `spinifex/kvstore` | `spinifex/foundation/state/kvstore` | Generic KV storage mechanics are state foundation. |
| `247e52ba3` | `spinifex/reconciler` | `spinifex/foundation/lifecycle/reconciler` | Generic reconciliation mechanics are lifecycle foundation. |
| `b1d0eba89` | inline KV migration mechanics | `spinifex/foundation/state/migrate` | KV migration is a reusable state concern. |
| `967f6904a` | `spinifex/kvlease` | `spinifex/foundation/state/kvlease` | Leases are state foundation, not a resource-domain owner. |
| `248f0e005`, `8c4af3030` | `spinifex/formation` | `spinifex/runtime/formation` | Formation owns its protocol payloads and remains runtime membership/bootstrap work. |
| `b8b97279c`, `c0c64cd53` | `spinifex/instancecache` | `spinifex/runtime/compute/cache` | Cache policy is decoupled from callers before becoming node-local compute runtime. |
| `aa0c36ff4` | `spinifex/types/ec2.go` | `contracts/ec2/v1` | The EC2 instance-command subject and JSON payload are an explicit versioned cross-process contract; `commands_test.go` is its compatibility evidence. |
| `c239fd897` | `spinifex/types/eni.go` | `spinifex/runtime/compute/vm/eni_requests.go` | PCIe hot-plug slot allocation is mutex-bearing, per-VM runtime state rather than a shared type or wire contract; `record_test.go` preserves its restart semantics. |

The EC2 contract row is intentionally different from the directory moves: it creates
a compatibility boundary. `ec2.cmd.*` retains its deployed one-token NATS
subscription and permission shape, while callers use
`InstanceCommandSubject` rather than reconstructing the subject. The
contract's README states the route, scope and change rule.

## Recording rule

For every later slice, add the old and new path, source commit, focused
verification, and any finding exposed by the move. Record a conflict rather
than folding a behavioural repair into a structural commit. When this branch
is merged, the umbrella repository's execution plan records the resulting
Spinifex commit.
