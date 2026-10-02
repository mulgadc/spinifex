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
| `6d6275e78` | `spinifex/types/cluster.go` (`NodeHealthResponse`) | `contracts/cluster/v1/health.go` | Node health is a versioned cluster control-plane representation shared by the per-node NATS route and daemon HTTPS `/health`; `health_test.go` pins both wire forms. |
| `53a614717` | `spinifex/types/cluster.go` (`SharedClusterData`) | `spinifex/daemon/config_hash.go` | Shared-config hash input is daemon-local implementation data; focused config-hash tests preserve deterministic, shared-only semantics. |
| `78fd017e0` | `spinifex/types/telemetry.go` (`GuestTelemetryMeta`) | `contracts/telemetry/v1/guest_meta.go` | QMP collector sidecar JSON is a versioned file contract between VM runtime and telemetry role; `guest_meta_test.go` pins its filename and payload. |
| `4b35a2a33` | `spinifex/types/node.go` (`NodeVMsResponse`, `VMInfo`, `VMGPUInfo`) | `contracts/cluster/v1/node_vms.go` | VM inventory is a versioned cluster fan-out payload used by SPX CLI and EKS host lookup; `node_vms_test.go` pins its subject and JSON shape. |
| `08bd335f0` | `spinifex/types/node.go` (`NodeStatusResponse`, capacity and GPU inventory) | `contracts/cluster/v1/status.go` | Node status is a versioned fan-out observation contract for operator inspection and EC2/EKS scheduling consumers; `status_test.go` pins its subject and JSON shape. |
| `f4cb550c9` | `spinifex/types/ebs.go` (GP3 policy constants) | `spinifex/domains/ec2/ebs/policy/gp3.go` | The supported GP3 product envelope is EC2 EBS domain policy, not generic runtime or Viperblockd wire state; `gp3_test.go` pins the deployed values. |
| `adb55de33` | `spinifex/types/ebs.go` (`NBDTransport`) | `spinifex/services/viperblockd/nbd_transport.go` | NBD endpoint transport selection is Viperblockd-local delivery configuration, not generic EBS state or a provider contract; `nbd_transport_test.go` pins the socket and TCP forms. |
| `94a6e7af2` | `spinifex/types/ebs.go` (`EBSConfigUpdateRequest`, `EBSConfigUpdateResponse`) | `contracts/viperblockd/legacy/v1/config_update.go` | Legacy configuration updates have deployed queue and volume-addressed NATS routes; the versioned compatibility contract is expressly transitional and separate from `providers/ebs`. |
| `b975f1ba9` | Remaining `spinifex/types/ebs.go` payloads | `runtime/compute/vm/ebs_requests.go` and `contracts/viperblockd/legacy/v1/volume.go` | The mutex-bearing attachment collection is VM-owned; deployed mount, unmount and delete routes plus their JSON payloads are versioned Viperblockd legacy compatibility. Contract tests pin every route and JSON shape. |
| `61cf833aa` | `spinifex/cloud/exoscale` | `spinifex/providers/cloud/exoscale` | Exoscale CLI access is a replaceable external-cloud adapter. The source move rewrites its four Exonet callers and preserves its adapter tests and CLI boundary. |
| `dbc62562c` | `spinifex/objectstore` | `spinifex/providers/objectstore` | The generic S3-compatible client is a replaceable object-storage adapter, not S3 resource authority. The mechanical move rewrites all direct callers; its focused suite and caller compilation passed. |
| `f11922986` | `spinifex/config` | `spinifex/bootstrap/config` | Root configuration parsing belongs to bootstrap. Its package API is unchanged; package tests and regular, integration, E2E and benchmark caller compilation passed. |
| `aa4b0b1bb` | `spinifex/paging` | `spinifex/foundation/aws/paging` | Opaque listing tokens, deterministic slicing and EC2 pagination validation are shared AWS protocol infrastructure rather than EC2 resource authority. The move rewrites all five EC2 callers; focused package tests, direct caller compilation and tagged integration compilation passed. |
| `6ac02a886` | `spinifex/clustersize` | `spinifex/foundation/state/clustersize` | The declared cluster size and its fail-closed JetStream/KV replica policy are shared state-tier infrastructure, not a resource-domain owner. Focused package tests, all direct caller compilation, integration and E2E-tagged harness compilation passed. |
| `93084043a` | `spinifex/handlers/ecr` | `spinifex/domains/ecr` | ECR repository metadata, blob semantics and lifecycle rules are ECR domain ownership. The full domain suite, direct callers and tagged integration callers passed. |
| `51b5c55b9` | `spinifex/gateway/ecrapi` | `spinifex/domains/ecr/awsapi` | AWS JSON 1.1 action inventory, validation, resource-scope derivation and NATS-backed action handlers are ECR action-adapter ownership. Focused, gateway and tagged integration validation passed. |
| `5e7311eb2` | `spinifex/gateway/ecr` | `spinifex/domains/ecr/registry` | The OCI Distribution v2 protocol adapter is ECR data-plane ownership. Focused registry, gateway/AWS-gateway and tagged integration validation passed. |
| `474c07316` | `spinifex/gateway/ecrauth` | `spinifex/domains/ecr/auth` | ECR token claims, issuer/verifier, encrypted signing-key persistence and rotation are ECR credential ownership. Focused, gateway/AWS-gateway and tagged integration validation passed. |
| `f59b40ad1` | Gateway `DescribeRepositories` action semantics | `spinifex/domains/ecr/awsapi.DescribeRepositories` | Request validation, account scope, repository metadata lookup and AWS response projection are ECR AWS-action ownership. Gateway retains authenticated HTTP adaptation and response writing. Focused domain, gateway dispatch and tagged integration compilation passed. |
| `c558872e8` | Gateway `CreateRepository` action semantics | `spinifex/domains/ecr/awsapi.CreateRepository` | Request decoding, input defaults and validation, duplicate detection, account-scoped metadata persistence and AWS response projection are ECR AWS-action ownership. Gateway retains authenticated HTTP adaptation and response writing. Focused domain, gateway lifecycle and tagged integration validation passed. |
| `a6b209b7b` | Gateway `DeleteRepository` action semantics | `spinifex/domains/ecr/awsapi.DeleteRepository` | Request validation, account scope, non-empty protection, forced deletion, metadata cascade and AWS response projection are ECR AWS-action ownership. Blob reclamation remains the separately documented GC concern. Focused domain, gateway lifecycle and tagged integration validation passed. |
| `7a5159366` | Gateway `PutImageTagMutability` action | `spinifex/domains/ecr/awsapi.PutImageTagMutability` | This endpoint-free action is registered directly in the ECR action table rather than retained as a gateway wrapper. Request validation, account-scoped metadata update and AWS response construction are domain-adapter ownership; generic gateway auth and policy gating remain before dispatch. Focused domain, gateway and tagged integration validation passed. |
| `a2f6005d2` | Gateway `GetAuthorizationToken` action response | `spinifex/domains/ecr/awsapi.GetAuthorizationToken` | Gateway retains canonical IAM/STS principal construction from authenticated context. The ECR adapter consumes a narrow token-issuer capability and owns mint-result handling, Docker `AWS:<jwt>` encoding and AWS response projection. Focused domain, real issuer/verifier gateway round-trip and tagged integration validation passed. |
| `ee4610a75` | Gateway `ListImages` action semantics | `spinifex/domains/ecr/awsapi.ListImages` | The ECR AWS adapter consumes a narrow OCI image-catalog capability and owns request validation, account scope, tag-status projection and AWS response/error mapping. Gateway retains authenticated HTTP adaptation and supplies the configured registry. Focused domain, registry-backed gateway and tagged integration validation passed. |
| `b22b723a4` | Gateway `DescribeImages` action semantics | `spinifex/domains/ecr/awsapi.DescribeImages` | The ECR AWS adapter now shares the image-catalog capability and catalog error mapping with ListImages, and owns image selection/detail projection. The accepted-but-unapplied filter field is explicitly retained as an unchanged compatibility gap. Focused domain, registry-backed gateway and tagged integration validation passed. |
| `deabaa300` | Gateway `BatchGetImage` action semantics | `spinifex/domains/ecr/awsapi.BatchGetImage` | The ECR AWS adapter consumes a narrow manifest-reader capability and owns request validation, the 100-image cap, digest-over-tag selection and partial-failure response projection. Repository/account scope validation is now shared with ListImages and DescribeImages. Focused domain, registry-backed gateway and tagged integration validation passed. |
| `2542ce98e` | Gateway `PutImage` action semantics | `spinifex/domains/ecr/awsapi.PutImage` | The ECR AWS adapter consumes a narrow manifest-writer capability and owns request validation, tag/digest selection, AWS response projection and OCI manifest-store error translation. Gateway retains authenticated HTTP adaptation and supplies the configured registry. Focused domain, registry-backed gateway and tagged integration validation passed. |
| `7666ed208` | Gateway `BatchDeleteImage` action semantics and residual ECR helpers | `spinifex/domains/ecr/awsapi.BatchDeleteImage`, `awsapi.ValidateRepositoryScope`, gateway `ecr_request.go` | The ECR adapter consumes a narrow image-deleter capability and owns batch deletion/partial-failure projection. Completion exposed scope validation still needed by lifecycle-preview adapters, which now calls the exported ECR adapter utility; generic HTTP JSON decoding moved to a gateway ECR adapter helper. Focused domain, image/lifecycle gateway and tagged integration validation passed. |
| `72c20bf78` | Gateway lifecycle-preview evaluation and `StartLifecyclePolicyPreview` | `spinifex/domains/ecr/awsapi.EvaluateLifecyclePreview`, `StartLifecyclePolicyPreview` | Shared preview evaluation now consumes explicit policy-store and image-catalog capabilities, owns request/stored-policy resolution and lifecycle evaluation, and returns a reusable expiry set. Start preview is fully domain-owned; Get preview consumes the evaluator and retains only its response projection pending the next slice. Focused domain, lifecycle gateway and tagged integration validation passed. |
| `68945bbb1` | Gateway `GetLifecyclePolicyPreview` response projection | `spinifex/domains/ecr/awsapi.GetLifecyclePolicyPreview` | Get preview now joins Start preview in the ECR adapter, sharing the synchronous evaluator and explicit policy-store/image-catalog capabilities. Gateway retains only authenticated HTTP/body adaptation and capability supply. Focused domain, lifecycle gateway and tagged integration validation passed. |
| `4c9f3d681` | Gateway ECR image-action wrappers and residual JSON adapter | `spinifex/domains/ecr/awsapi.RegistryActionService` | ListImages, DescribeImages, BatchGetImage, PutImage and BatchDeleteImage now dispatch through one composed registry-action capability after the generic gateway's authentication and policy gate. The composition root supplies narrow OCI capabilities; the operation inventory derives their implemented status from that capability. The obsolete gateway wrappers and JSON helper are removed. Focused domain, gateway, AWS-gateway and tagged integration validation passed. |
| `40e7b14ad` | Gateway lifecycle-preview wrappers | `spinifex/domains/ecr/awsapi.LifecyclePreviewActionService` | StartLifecyclePolicyPreview and GetLifecyclePolicyPreview now dispatch through one composed policy-store-plus-image-catalog capability after generic authorization. Production reuses the registry's NATS metadata client; focused tests compose an in-memory metadata store. The obsolete wrappers are removed, and a missing composition fails closed rather than falling through to the raw action table. Focused domain, gateway, AWS-gateway and tagged integration validation passed. |
| `54f96ad78` | Gateway CreateRepository, DescribeRepositories and DeleteRepository wrappers | `spinifex/domains/ecr/awsapi.RepositoryActionService` | Repository actions now dispatch through one composed metadata-store-plus-endpoint-profile capability after generic authorization. The individual action adapters depend on a narrow repository metadata contract rather than constructing NATS clients themselves. The three obsolete wrappers are removed, and a missing composition fails closed. Focused domain, gateway, AWS-gateway and tagged integration validation passed. |
| `5ebbdcb53` | Gateway GetAuthorizationToken wrapper and endpoint adapter | `spinifex/domains/ecr/awsapi.AuthorizationTokenActionService` | GetAuthorizationToken now dispatches through a composed issuer-plus-endpoint capability after gateway constructs the canonical IAM/STS principal. The token subject remains gateway identity work; ECR retains credential response projection. The obsolete wrapper and endpoint adapter are removed, a missing action service fails closed, and an unavailable issuer retains the declared NotImplemented response. Focused domain, gateway, AWS-gateway and tagged integration validation passed. |
| `2b945ad88` | `spinifex/handlers/acm`, `spinifex/gateway/acm` | `spinifex/domains/acm`, `spinifex/domains/acm/awsapi` | ACM certificate state, issuance and renewal are ACM-domain ownership; its AWS JSON/NATS request adapters live beside them. All nine adapters, including the previously inlined RequestCertificate path, now use `domains/acm/awsapi`; generic gateway retains only routing, request context, policy gating and response writing. Focused domain, adapter and gateway request tests plus direct-caller and tagged integration compilation passed. |
| `47c6b2fcf` | `spinifex/gateway/bodyscope` | `spinifex/ingress/aws/bodyscope` | Case-aware, ambiguity-safe extraction of AWS JSON request identifiers is ingress policy machinery, not generic gateway ownership. Focused parser tests and compilation of all six direct service-policy consumers passed. |

The EC2 contract row is intentionally different from the directory moves: it creates
a compatibility boundary. `ec2.cmd.*` retains its deployed one-token NATS
subscription and permission shape, while callers use
`InstanceCommandSubject` rather than reconstructing the subject. The
contract's README states the route, scope and change rule.

### ECR composition boundary remaining

The ECR AWS JSON dispatcher and `/v2/*` route assembly remain methods on the
generic `GatewayConfig`. They establish the generic HTTP route, authenticated
request context, bounded-body read and policy gate, then invoke the ECR domain
packages above. They are not moved merely to remove the last ECR-named files
from `gateway`: doing so first would make the ECR domain own generic gateway
state and policy mechanics.

Registry-backed image actions have now crossed this boundary through
`awsapi.RegistryActionService`: the composition root supplies the OCI
capabilities, and the generic gateway dispatches through it after authorization.

Lifecycle-preview actions have likewise crossed it through
`awsapi.LifecyclePreviewActionService`, whose policy-store-plus-image-catalog
dependencies remain explicit and separate from the registry image-action
service.

Repository metadata actions have crossed it through
`awsapi.RepositoryActionService`, which makes both their narrow metadata-store
dependency and advertised endpoint profile composition explicit.

Authorization-token issuance has crossed it through
`awsapi.AuthorizationTokenActionService`. Gateway intentionally still builds
the canonical IAM/STS principal before dispatch, because ECR must not depend on
gateway identity context or ARN construction.

All implemented ECR control-plane actions now dispatch through explicit ECR
capabilities. The remaining ECR-named gateway code is generic `/v2/*` route
assembly, SigV4/registry authentication and policy enforcement; it remains in
gateway rather than inverting those dependencies into the ECR domain.

### ACM relocation residuals

The ACM move leaves two pre-existing dependency seams visible rather than
changing their behaviour in a structural slice:

- `domains/acm.Store` still imports IAM's AES-GCM helper functions. It does
  not read IAM records, but the crypto mechanism must move behind a shared
  crypto boundary before the final domain-dependency lint can prohibit that
  implementation import.
- `GatewayConfig` still owns the ACM action-name dispatch map. ACM owns the
  action adapters and resource-scope derivation, while generic gateway owns
  route handling, request context, policy gating and response writing. Moving
  service registration itself needs the later ingress registration contract;
  it is not hidden by a compatibility shim in this move.

## Recording rule

For every later slice, add the old and new path, source commit, focused
verification, and any finding exposed by the move. Record a conflict rather
than folding a behavioural repair into a structural commit. When this branch
is merged, the umbrella repository's execution plan records the resulting
Spinifex commit.
