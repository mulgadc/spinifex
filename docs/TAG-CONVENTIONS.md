# Spinifex Tag Conventions

Spinifex uses a small set of reserved tag keys to mark resources that are owned and managed by the platform itself (as opposed to resources created directly by customer AWS API calls). The UI uses these tags to hide system-owned resources from customer-facing listings so operators do not
mistake them for user workloads and accidentally terminate or modify them.

## `spinifex:managed-by`

Identifies the Spinifex platform component that owns a resource.

| Value   | Component                       | Tagged resource types                 |
|---------|---------------------------------|---------------------------------------|
| `elbv2` | ALB / HAProxy load balancers    | LB VM (`instance`), LB ENI, LB AMI    |

The tag is applied by the backend at create/launch time:

- **Instance** — set in `spinifex/spinifex/daemon/daemon_system_instance.go` when `LaunchSystemInstance` builds the `RunInstancesInput`, and mirrored onto the stored `vm.VM.ManagedBy` field for fast lookup.
- **ENI** — set in `spinifex/spinifex/handlers/elbv2/service_impl.go` when `CreateLoadBalancer` calls `CreateNetworkInterface`.
- **AMI** — declared on the catalog entry in `spinifex/spinifex/utils/images.go` and copied into `AMIMetadata.Tags` by the image importer in `spinifex/cmd/spinifex/cmd/admin.go`.

Tag keys and values are defined once in `spinifex/spinifex/tags/tags.go`; new system components should register their value there and reuse the constants rather than hardcoding strings.

## UI behaviour

The admin **Nodes** page and the **Images** (AMIs) page filter out resources that carry `spinifex:managed-by`. Engineers who need to see system resources for debugging can append `?system=1` to the URL — the filter is bypassed and a hidden-count banner is replaced with the full listing. Direct navigation to a system AMI detail page without `?system=1` renders the "Image not found" fallback.

The EC2 **Instances** page does not need UI-side filtering: backend SigV4 authentication scopes `DescribeInstances` by account ID, and system instances live under the global system account
(`000000000000`), so customers never see them.

## `spinifex:lb-arn`

Applied to ELBv2-managed ENIs alongside `spinifex:managed-by=elbv2`. Stores the ARN of the parent load balancer so reconciliation can map an ENI back to its owner without a separate lookup. Customer-facing
filtering keys off `spinifex:managed-by`; `spinifex:lb-arn` is informational.
