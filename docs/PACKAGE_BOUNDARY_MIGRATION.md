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

## Recording rule

For every later slice, add the old and new path, source commit, focused
verification, and any finding exposed by the move. Record a conflict rather
than folding a behavioural repair into a structural commit. When this branch
is merged, the umbrella repository's execution plan records the resulting
Spinifex commit.
