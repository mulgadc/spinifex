# Spinifex Command Reference

Implementation status of every CLI subcommand (`spx`) and AWS-compatible API
action served by the Spinifex gateway. Tables list flag coverage only — for
prerequisites, behaviour, and error semantics, read the linked source or
`spinifex/docs/DESIGN.md`.

**Status legend:** **DONE** — fully implemented · **STARTED** — partially
implemented (caveat in note) · **NOT STARTED** — not implemented.

---

## Spinifex Admin CLI (`spx`)

Platform-management commands not exposed via the AWS gateway. CLI-only.

### Service Management

`spx service <name> {start|stop|status}` — lifecycle for each cluster service.
`stop`/`status` take no flags.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx service predastore start` | `--port` (8443), `--host` (0.0.0.0), `--base-path`, `--config-path`, `--debug`, `--tls-cert`, `--tls-key`, `--backend` (distributed/filesystem), `--node-id`, `--pprof`, `--pprof-output` | — | **DONE** |
| `spx service predastore stop` / `status` | — | — | **DONE** |
| `spx service viperblock start` | `--s3-host`, `--s3-bucket`, `--s3-region`, `--plugin-path` | — | **DONE** |
| `spx service viperblock stop` / `status` | — | — | **DONE** |
| `spx service nats start` | `--port` (4222), `--host`, `--debug`, `--data-dir`, `--jetstream` | — | **DONE** |
| `spx service nats stop` / `status` | — | — | **DONE** |
| `spx service spinifex start` | `--wal-dir` | — | **DONE** |
| `spx service spinifex stop` / `status` | — | — | **DONE** |
| `spx service awsgw start` | `--host` (0.0.0.0:9999), `--tls-cert`, `--tls-key`, `--debug` | — | **DONE** |
| `spx service awsgw stop` / `status` | — | — | **DONE** |
| `spx service spinifex-ui start` | `--port` (3000), `--host`, `--tls-cert`, `--tls-key` | — | **DONE** |
| `spx service spinifex-ui stop` / `status` | — | — | **DONE** |
| `spx service vpcd start` / `stop` / `status` | — | — | **DONE** |

Viperblock `--plugin-path` is auto-detected via `nbdkit --dump-config plugindir`
and overridable via `SPINIFEX_VIPERBLOCK_PLUGIN_PATH` in
`/etc/spinifex/systemd.env`.

### Cluster Inspection

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx get nodes` | `--timeout` (3s) | — | **DONE** |
| `spx get vms` (alias: `instances`) | `--timeout` (3s) | — | **DONE** |
| `spx top nodes` | `--timeout` (3s) | — | **DONE** |

### Cluster Lifecycle

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx admin init` | `--nodes`, `--node`, `--bind`, `--port`, `--region`, `--az`, `--cluster-name`, `--cluster-bind`, `--cluster-routes`, `--predastore-nodes`, `--services`, `--formation-timeout`, `--token-ttl`, `--force` | — | **DONE** |
| `spx admin join` | `--host`, `--node`, `--token`, `--bind`, `--port`, `--region`, `--az`, `--cluster-bind`, `--cluster-routes`, `--data-dir`, `--services` | — | **DONE** |
| `spx admin cluster shutdown` | `--force`, `--timeout` (120s), `--dry-run` | — | **DONE** |
| `spx admin cert renew` | `--extra-ip`, `--extra-dns` | — | **DONE** |
| `spx admin upgrade` | `--yes`, `--config-dir`, `--spinifex-dir` | — | **DONE** |
| `spx version` | — | — | **DONE** |

### Account Management

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx admin account create` | `--name` | — | **DONE** |
| `spx admin account list` | — | — | **DONE** |

### Image Management

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx admin images import` | `--name`, `--file`, `--distro`, `--version`, `--arch`, `--platform`, `--boot-mode` (bios/uefi/uefi-preferred), `--tag`, `--force`, `--skip-verify` | — | **DONE** |
| `spx admin images list` | — | — | **DONE** |
| `spx admin images remove` | `--image-id`, `--force`, `--yes` | — | **DONE** |

#### Image integrity verification (CMMC SI.L1-3.14.2)

Catalog imports (`--name`) verify the image against the catalog-declared
SHA-256/SHA-512 digest before extraction. The sums file is fetched over HTTPS
only (cross-scheme redirects refused). Verification runs on fresh downloads
*and* cache hits, so a poisoned cache is caught on the next import. On
mismatch the import exits non-zero, the cached file is left for inspection,
and the guidance is to re-run with `--force`.

`--file` imports skip verification by design: operator-supplied media is
outside Spinifex's trust boundary. The skip is recorded as an INFO `slog`
event (`reason=local-file-import`).

`--skip-verify` bypasses checksum verification for catalog imports — only for
debugging upstream mirror issues. Logged at WARN
(`reason=skip-verify-flag`) and to stderr.

**Limitation:** verification confirms the image matches the digest the mirror
served. A mirror compromise that swaps both image and sums file is not
detected; GPG signature verification is a later phase.

#### `spx admin images remove` caveats

System AMIs (`ImageOwnerAlias = "system"`) cannot be removed via the AWS
`DeregisterImage`/`DeleteSnapshot` flow — those reject system owners with
`UnauthorizedOperation`. `spx admin images remove` is the admin-trust-boundary
counterpart: it walks the transitive dependent set (copied snapshots, derived
volumes, account AMIs created via `CopyImage`) and refuses if anything
references the target.

**TOCTOU window:** between the safety scan and the `config.json` delete a
concurrent `RunInstances` could create a new dependent volume. The window is
sub-second on a healthy cluster. The admin is expected to know fleet state;
if the race fires the result is a `vol-<id>` with deleted backing blocks,
recovered by terminating the orphaned instance.

**`--force` bypasses every safety check** (dependents, ownership, missing /
corrupt `config.json`). Use only for salvage of orphaned blocks. Running
`--force` against a live system AMI corrupts every dependent volume on the
next disk read.

### GPU Management

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `spx admin gpu status` | `--node` | — | **DONE** |
| `spx admin gpu enable` | — | — | **DONE** |
| `spx admin gpu disable` | — | — | **DONE** |
| `spx admin gpu setup` | — | — | **DONE** |

`gpu setup` must run as root. `gpu enable`/`disable` must run on the target
host (writes `spinifex.toml`, SIGHUPs `spinifex-daemon`).

---

## AWS-Compatible API

### EC2 — Instance Management

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `run-instances` | `--image-id`, `--instance-type`, `--count`, `--key-name`, `--user-data`, `--subnet-id`, `--block-device-mappings` (DeviceName, VolumeSize, VolumeType, Iops, DeleteOnTermination), `--placement` (GroupName), `--iam-instance-profile` (Name/Arn) | `--security-group-ids`, `--tag-specifications`, `--dry-run`, `--client-token`, `--disable-api-termination`, `--ebs-optimized`, `--network-interfaces`, `--private-ip-address`, `--monitoring`, `--credit-specification`, `--cpu-options`, `--metadata-options`, `--launch-template`, `--hibernate-options` | **DONE** |
| `describe-instances` | `--instance-ids`, `--filters` (instance-state-name, instance-id, instance-type, vpc-id, subnet-id, tag:*, tag-key, tag-value) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `start-instances` | `--instance-ids` | `--dry-run`, `--force` | **DONE** |
| `stop-instances` | `--instance-ids` | `--force`, `--hibernate`, `--dry-run` | **DONE** |
| `terminate-instances` | `--instance-ids`, `DeleteOnTermination` (per-volume) | `--dry-run` | **DONE** |
| `reboot-instances` | `--instance-ids` | `--dry-run` | **DONE** |
| `describe-instance-types` | `--filters` (capacity only) | `--instance-types`, `--max-results`, `--next-token`, `--dry-run`, other filters | **DONE** |
| `modify-instance-attribute` | `--instance-id`, `--instance-type`, `--user-data`, `--disable-api-termination` | `--ebs-optimized`, `--source-dest-check`, `--instance-initiated-shutdown-behavior`, `--block-device-mappings`, `--groups`, `--ena-support`, `--sriov-net-support` | **DONE** |
| `get-console-output` | `--instance-id` | `--latest`, `--dry-run` | **DONE** |
| `describe-instance-attribute` | `--instance-id`, `--attribute` (instanceType, userData, disableApiTermination, instanceInitiatedShutdownBehavior, disableApiStop, ebsOptimized, enaSupport, sourceDestCheck, rootDeviceName, kernel, ramdisk) | `--dry-run` | **DONE** |
| `describe-instance-credit-specifications` | `--instance-ids` | `--filters`, `--max-results`, `--dry-run` | **DONE** (stub — always returns `standard`) |
| `describe-instance-status` | `--instance-ids`, `--include-all-instances`, `--filters` (availability-zone, instance-state-code, instance-state-name, tag:*) | `--max-results`, `--next-token`, `--dry-run`, event/instance-status/system-status filters | **DONE** (static health) |
| `monitor-instances` | — | `--instance-ids` | **NOT STARTED** |
| `unmonitor-instances` | — | `--instance-ids` | **NOT STARTED** |

`run-instances --iam-instance-profile` enforces `iam:PassRole` on the contained
role ARN and rejects cross-account profile ARNs. Placement strategies: `spread`
(1 per node, atomic CAS) and `cluster` (pin all to one node).

### EC2 — IAM Instance Profile Associations

Stored as `vm.VM.IamInstanceProfileArn` + `IamInstanceProfileAssociationId`
(one ARN per instance). Association IDs (`iip-assoc-`) are regenerated on every
Associate/Replace and never reused. Auto-disassociated on terminate; preserved
across stop/start.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `associate-iam-instance-profile` | `--instance-id`, `--iam-instance-profile` (Name/Arn) | `--dry-run` | **DONE** |
| `disassociate-iam-instance-profile` | `--association-id` | `--dry-run` | **DONE** |
| `replace-iam-instance-profile-association` | `--association-id`, `--iam-instance-profile` (Name/Arn) | `--dry-run` | **DONE** |
| `describe-iam-instance-profile-associations` | `--association-ids`, `--filters` (instance-id, state) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |

### EC2 — Key Pairs

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-key-pair` | `--key-name`, `--key-type` (rsa/ed25519) | `--key-format`, `--tag-specifications`, `--dry-run` | **DONE** |
| `describe-key-pairs` | `--key-names`, `--key-pair-ids`, `--filters` (key-pair-id, key-name, fingerprint, tag:*) | `--max-results`, `--dry-run` | **DONE** |
| `delete-key-pair` | `--key-name`, `--key-pair-id` | `--dry-run` | **DONE** |
| `import-key-pair` | `--key-name`, `--public-key-material` | `--tag-specifications`, `--dry-run` | **DONE** |

### EC2 — AMI Images

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-images` | `--image-ids`, `--owners` (self/account-id/alias), `--filters` (name, state, architecture, image-id, is-public, owner-id, description, image-type, tag:*) | `--executable-users`, `--include-deprecated`, `--include-disabled`, `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `create-image` | `--instance-id`, `--name`, `--description`, `--tag-specifications` | `--no-reboot`, `--block-device-mappings`, `--dry-run` | **DONE** |
| `register-image` | `--name`, `--description`, `--architecture` (x86_64/arm64/i386), `--root-device-name`, `--virtualization-type` (hvm), `--boot-mode` (bios/uefi/uefi-preferred), `--block-device-mappings` (root w/ `Ebs.SnapshotId`+`VolumeSize`), `--tag-specifications` | `--billing-products`, `--uefi-data` | **DONE** |
| `deregister-image` | `--image-id` | `--dry-run` | **DONE** |
| `copy-image` | `--source-image-id`, `--source-region`, `--name`, `--description`, `--client-token`, `--copy-image-tags`, `--tag-specifications` (image only) | `--encrypted`, `--kms-key-id`, `--destination-outpost-arn`, `--dry-run` | **DONE** |
| `describe-image-attribute` | `--image-id`, `--attribute` (`description`, `blockDeviceMapping`) | `--dry-run`, other attributes (`launchPermission`, `bootMode`, `kernel`, `ramdisk`, `sriovNetSupport`, `productCodes`, `tpmSupport`, `uefiData`, `imdsSupport`, `lastLaunchedTime`, `deregistrationProtection`) | **DONE** |
| `modify-image-attribute` | `--image-id`, `--description` (top-level or structured) | `--launch-permission`, `--imds-support`, `--operation-type`, `--user-ids`, `--user-groups`, `--organization-arns`, `--product-codes`, `--dry-run`, other `--attribute` values | **DONE** |
| `reset-image-attribute` | `--image-id`, `--attribute description` | `--attribute launchPermission`, `--dry-run` | **DONE** |
| `import-image` | — | `--disk-containers`, `--description`, `--architecture`, `--platform` | **NOT STARTED** |

`copy-image` is metadata-only — no block copy. The new snapshot inherits the
source `VolumeID`. `register-image`/`copy-image` accept `uefi-preferred` as
input but treat it as `uefi` at launch.

### EC2 — Volumes (EBS)

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-volumes` | `--volume-ids`, `--filters` (volume-id, status, size, volume-type, attachment.instance-id, attachment.status, attachment.device, availability-zone, tag:*), persisted `DeleteOnTermination` | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `create-volume` | `--size`, `--availability-zone`, `--volume-type` (gp3), `--snapshot-id` | `--iops` (hardcoded 3000), `--encrypted` (hardcoded false), `--throughput`, `--tag-specifications` | **DONE** |
| `delete-volume` | `--volume-id` | `--dry-run` | **DONE** |
| `modify-volume` | `--volume-id`, `--size`, `--volume-type`, `--iops` | `--throughput`, `--dry-run`, `--multi-attach-enabled` | **DONE** |
| `attach-volume` | `--volume-id`, `--instance-id`, `--device` (auto-assigns `/dev/sd[f-p]`) | `--dry-run` | **DONE** |
| `detach-volume` | `--volume-id`, `--instance-id` (optional), `--device`, `--force` | `--dry-run` | **DONE** |
| `describe-volume-status` | `--volume-ids`, `--filters` (volume-id, volume-status.status, availability-zone) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `describe-volumes-modifications` | — | `--volume-ids`, `--filters`, `--max-results` | **NOT STARTED** |

### EC2 — Snapshots

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-snapshot` | `--volume-id`, `--description`, `--tag-specifications` | `--dry-run` | **DONE** |
| `delete-snapshot` | `--snapshot-id` | `--dry-run` | **DONE** |
| `describe-snapshots` | `--snapshot-ids`, `--filters` (snapshot-id, status, volume-id, volume-size, owner-id, tag:*) | `--owner-ids`, `--max-results`, `--dry-run` | **DONE** |
| `copy-snapshot` | `--source-snapshot-id`, `--source-region`, `--description` | `--encrypted`, `--dry-run` | **DONE** |
| `create-snapshots` | — | `--instance-specification`, `--description`, `--tag-specifications` | **NOT STARTED** |

### EC2 — Tags

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-tags` | `--resources`, `--tags` | `--dry-run` | **DONE** |
| `delete-tags` | `--resources`, `--tags` | `--dry-run` | **DONE** |
| `describe-tags` | `--filters` (resource-id, resource-type, key, value) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |

### EC2 — Regions, AZs, Account Attributes

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-regions` | — (returns configured region) | `--region-names`, `--filters`, `--all-regions`, `--dry-run` | **DONE** |
| `describe-availability-zones` | — (returns configured AZ) | `--zone-names`, `--filters`, `--all-availability-zones` | **DONE** |
| `describe-account-attributes` | `--attribute-names` | `--dry-run` | **DONE** |

### EC2 — Account Settings

Persistence works (NATS JetStream KV) but stored values are not yet enforced
by downstream services.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `enable-ebs-encryption-by-default` | — | `--dry-run` | **STARTED** (enforcement pending) |
| `disable-ebs-encryption-by-default` | — | `--dry-run` | **STARTED** (enforcement pending) |
| `get-ebs-encryption-by-default` | — | `--dry-run` | **STARTED** (enforcement pending) |
| `enable-serial-console-access` | — | `--dry-run` | **STARTED** (enforcement pending) |
| `disable-serial-console-access` | — | `--dry-run` | **STARTED** (enforcement pending) |
| `get-serial-console-access-status` | — | `--dry-run` | **DONE** |
| `enable-snapshot-block-public-access` | — | `--state` | **NOT STARTED** |
| `disable-snapshot-block-public-access` | — | `--dry-run` | **NOT STARTED** |
| `get-snapshot-block-public-access-state` | — | `--dry-run` | **NOT STARTED** |
| `enable-image-block-public-access` | — | `--image-block-public-access-state` | **NOT STARTED** |
| `disable-image-block-public-access` | — | `--dry-run` | **NOT STARTED** |
| `get-image-block-public-access-state` | — | `--dry-run` | **NOT STARTED** |
| `modify-instance-metadata-defaults` | — | `--http-tokens`, `--http-put-response-hop-limit`, `--http-endpoint`, `--instance-metadata-tags` | **NOT STARTED** |
| `get-instance-metadata-defaults` | — | `--dry-run` | **NOT STARTED** |

### EC2 — VPC Core

VPC/Subnet/ENI/SG CRUD stores metadata in NATS KV and publishes events to
vpcd for OVN translation. Single AZ for Spinifex v1.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-vpc` | `--cidr-block`, `--tag-specifications` | `--instance-tenancy`, `--dry-run` | **DONE** |
| `delete-vpc` | `--vpc-id` | `--dry-run` | **DONE** |
| `describe-vpcs` | `--vpc-ids`, `--filters` (vpc-id, state, cidr-block, is-default, owner-id, tag:*) | `--max-results`, `--dry-run` | **DONE** |
| `modify-vpc-attribute` | `--vpc-id`, `--enable-dns-hostnames`, `--enable-dns-support`, `--enable-network-address-usage-metrics` | `--dry-run` | **DONE** |
| `describe-vpc-attribute` | `--vpc-id`, `--attribute` (enableDnsHostnames, enableDnsSupport, enableNetworkAddressUsageMetrics) | `--dry-run` | **DONE** |
| `associate-vpc-cidr-block` | — | `--vpc-id`, `--cidr-block` | **NOT STARTED** |
| `disassociate-vpc-cidr-block` | — | `--association-id` | **NOT STARTED** |
| `create-subnet` | `--vpc-id`, `--cidr-block`, `--availability-zone`, `--tag-specifications` | `--dry-run` | **DONE** |
| `delete-subnet` | `--subnet-id` | `--dry-run` | **DONE** |
| `describe-subnets` | `--subnet-ids`, `--filters` (vpc-id, subnet-id, availability-zone, cidr-block, state, default-for-az, tag:*) | `--max-results`, `--dry-run` | **DONE** |
| `modify-subnet-attribute` | `--subnet-id`, `--map-public-ip-on-launch` | `--assign-ipv6-address-on-creation`, `--dry-run` | **DONE** |
| `associate-subnet-cidr-block` | — | `--subnet-id`, `--ipv6-cidr-block` | **NOT STARTED** |
| `disassociate-subnet-cidr-block` | — | `--association-id` | **NOT STARTED** |

### EC2 — Security Groups

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-security-group` | `--group-name`, `--description`, `--vpc-id`, `--tag-specifications` | `--dry-run` | **DONE** |
| `delete-security-group` | `--group-id` | `--dry-run` | **DONE** |
| `describe-security-groups` | `--group-ids`, `--filters` (vpc-id, group-name, group-id, description, ip-permission.cidr, tag:*) | `--group-names`, `--max-results`, `--dry-run` | **DONE** |
| `authorize-security-group-ingress` | `--group-id`, `--ip-permissions` | `--dry-run` | **DONE** |
| `authorize-security-group-egress` | `--group-id`, `--ip-permissions` | `--dry-run` | **DONE** |
| `revoke-security-group-ingress` | `--group-id`, `--ip-permissions` | `--dry-run` | **DONE** |
| `revoke-security-group-egress` | `--group-id`, `--ip-permissions` | `--dry-run` | **DONE** |
| `describe-security-group-rules` | `--filters` (group-id, security-group-rule-id, tag:*, tag-key), `--security-group-rule-ids` | `--max-results`, `--next-token`, `--dry-run` | **DONE** |

### EC2 — Internet Gateway

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-internet-gateway` | `--tag-specifications` | `--dry-run` | **DONE** |
| `attach-internet-gateway` | `--internet-gateway-id`, `--vpc-id` | `--dry-run` | **DONE** |
| `detach-internet-gateway` | `--internet-gateway-id`, `--vpc-id` | `--dry-run` | **DONE** |
| `delete-internet-gateway` | `--internet-gateway-id` | `--dry-run` | **DONE** |
| `describe-internet-gateways` | `--internet-gateway-ids`, `--filters` (internet-gateway-id, attachment.vpc-id, attachment.state, tag:*) | `--max-results`, `--dry-run` | **DONE** |

### EC2 — Egress-Only Internet Gateway

KV CRUD only — no OVN/OVS integration. EIGWs are stored but have no effect on
network topology.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-egress-only-internet-gateway` | `--vpc-id`, `--tag-specifications` | `--client-token`, `--dry-run` | **STARTED** (KV only, no OVN) |
| `delete-egress-only-internet-gateway` | `--egress-only-internet-gateway-id` | `--dry-run` | **STARTED** (KV only, no OVN) |
| `describe-egress-only-internet-gateways` | `--egress-only-internet-gateway-ids`, `--filters` (egress-only-internet-gateway-id, tag:*) | `--max-results`, `--next-token`, `--dry-run` | **STARTED** (KV only, no OVN) |

### EC2 — Route Tables

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-route-table` | `--vpc-id` | `--tag-specifications`, `--dry-run` | **DONE** |
| `delete-route-table` | `--route-table-id` | `--dry-run` | **DONE** |
| `describe-route-tables` | `--route-table-ids`, `--filters` (vpc-id, route-table-id, association.main, association.route-table-association-id, association.subnet-id, route.destination-cidr-block, route.gateway-id) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `create-route` | `--route-table-id`, `--destination-cidr-block`, `--gateway-id`, `--nat-gateway-id` | `--egress-only-internet-gateway-id`, `--vpc-peering-connection-id`, `--dry-run` | **DONE** |
| `delete-route` | `--route-table-id`, `--destination-cidr-block` | `--dry-run` | **DONE** |
| `replace-route` | `--route-table-id`, `--destination-cidr-block`, `--gateway-id` | `--nat-gateway-id`, `--dry-run` | **DONE** |
| `associate-route-table` | `--route-table-id`, `--subnet-id` | `--gateway-id`, `--dry-run` | **DONE** |
| `disassociate-route-table` | `--association-id` | `--dry-run` | **DONE** |
| `replace-route-table-association` | `--association-id`, `--route-table-id` | `--dry-run` | **DONE** |

### EC2 — Network Interfaces (ENIs)

ENIs are auto-created by `run-instances --subnet-id` and auto-deleted on
termination. Standalone attach/detach API is internal-only.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-network-interface` | `--subnet-id`, `--private-ip-address`, `--description`, `--tag-specifications` | `--groups`, `--dry-run` | **DONE** |
| `delete-network-interface` | `--network-interface-id` | `--dry-run` | **DONE** |
| `describe-network-interfaces` | `--network-interface-ids`, `--filters` (subnet-id, vpc-id, attachment.instance-id) | `--max-results`, `--dry-run` | **DONE** |
| `modify-network-interface-attribute` | — | `--network-interface-id`, `--description`, `--groups` | **DONE** |
| `attach-network-interface` | — | `--network-interface-id`, `--instance-id`, `--device-index` | **NOT STARTED** (internal only) |
| `detach-network-interface` | — | `--attachment-id`, `--force` | **NOT STARTED** (internal only) |
| `assign-private-ip-addresses` | — | `--network-interface-id`, `--private-ip-addresses`, `--secondary-private-ip-address-count` | **NOT STARTED** |
| `unassign-private-ip-addresses` | — | `--network-interface-id`, `--private-ip-addresses` | **NOT STARTED** |

### EC2 — Elastic IP

EIP commands are only registered when an external IPAM pool is configured.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `allocate-address` | `--public-ipv4-pool`, `--tag-specifications` | `--domain`, `--dry-run` | **DONE** |
| `release-address` | `--allocation-id` | `--dry-run` | **DONE** |
| `associate-address` | `--allocation-id`, `--network-interface-id`, `--instance-id`, `--private-ip-address` | `--dry-run`, `--allow-reassociation` | **DONE** |
| `disassociate-address` | `--association-id` | `--dry-run` | **DONE** |
| `describe-addresses` | `--allocation-ids`, `--public-ips`, `--filters` (allocation-id, public-ip, instance-id, association-id, domain, tag:*) | `--dry-run` | **DONE** |
| `describe-addresses-attribute` | `--allocation-ids` | `--attribute`, `--dry-run`, `--max-results`, `--next-token` | **DONE** |

### EC2 — NAT Gateway

Deleted gateways move to a separate KV bucket with 1-hour TTL for Terraform
polling.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-nat-gateway` | `--subnet-id`, `--allocation-id` | `--connectivity-type`, `--tag-specifications`, `--dry-run` | **DONE** |
| `delete-nat-gateway` | `--nat-gateway-id` | `--dry-run` | **DONE** |
| `describe-nat-gateways` | `--nat-gateway-ids`, `--filters` (vpc-id, state) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `assign-private-nat-gateway-address` | — | `--nat-gateway-id`, `--private-ip-addresses` | **NOT STARTED** |
| `associate-nat-gateway-address` | — | `--nat-gateway-id`, `--allocation-ids` | **NOT STARTED** |

### EC2 — Placement Groups

Strategies: `spread` (1 instance per node, strict) and `cluster` (all pinned
to single node). `partition` rejected.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-placement-group` | `--group-name`, `--strategy` (spread/cluster) | `--partition-count`, `--spread-level`, `--tag-specifications`, `--dry-run` | **DONE** |
| `delete-placement-group` | `--group-name` | `--dry-run` | **DONE** |
| `describe-placement-groups` | `--group-names`, `--group-ids`, `--filters` (strategy, state, spread-level, group-name) | `--dry-run` | **DONE** |

### EC2 — VPC Peering

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-vpc-peering-connection` | — | `--vpc-id`, `--peer-vpc-id`, `--peer-owner-id`, `--peer-region`, `--tag-specifications` | **NOT STARTED** |
| `accept-vpc-peering-connection` | — | `--vpc-peering-connection-id`, `--dry-run` | **NOT STARTED** |
| `reject-vpc-peering-connection` | — | `--vpc-peering-connection-id`, `--dry-run` | **NOT STARTED** |
| `delete-vpc-peering-connection` | — | `--vpc-peering-connection-id`, `--dry-run` | **NOT STARTED** |
| `describe-vpc-peering-connections` | — | `--vpc-peering-connection-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `modify-vpc-peering-connection-options` | — | `--vpc-peering-connection-id`, `--requester-peering-connection-options`, `--accepter-peering-connection-options` | **NOT STARTED** |

### EC2 — VPC Endpoints

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-vpc-endpoint` | — | `--vpc-id`, `--service-name`, `--vpc-endpoint-type`, `--route-table-ids`, `--subnet-ids`, `--tag-specifications` | **NOT STARTED** |
| `delete-vpc-endpoints` | — | `--vpc-endpoint-ids`, `--dry-run` | **NOT STARTED** |
| `describe-vpc-endpoints` | — | `--vpc-endpoint-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `describe-vpc-endpoint-services` | — | `--service-names`, `--filters`, `--max-results` | **NOT STARTED** |
| `modify-vpc-endpoint` | — | `--vpc-endpoint-id`, `--add-route-table-ids`, `--remove-route-table-ids`, `--add-subnet-ids`, `--remove-subnet-ids` | **NOT STARTED** |

### EC2 — VPN & Customer Gateway

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-customer-gateway` | — | `--type`, `--bgp-asn`, `--ip-address`, `--tag-specifications` | **NOT STARTED** |
| `delete-customer-gateway` | — | `--customer-gateway-id`, `--dry-run` | **NOT STARTED** |
| `describe-customer-gateways` | — | `--customer-gateway-ids`, `--filters` | **NOT STARTED** |
| `create-vpn-gateway` | — | `--type`, `--amazon-side-asn`, `--tag-specifications` | **NOT STARTED** |
| `delete-vpn-gateway` | — | `--vpn-gateway-id`, `--dry-run` | **NOT STARTED** |
| `attach-vpn-gateway` | — | `--vpn-gateway-id`, `--vpc-id` | **NOT STARTED** |
| `detach-vpn-gateway` | — | `--vpn-gateway-id`, `--vpc-id` | **NOT STARTED** |
| `describe-vpn-gateways` | — | `--vpn-gateway-ids`, `--filters` | **NOT STARTED** |
| `create-vpn-connection` | — | `--type`, `--customer-gateway-id`, `--vpn-gateway-id`, `--options`, `--tag-specifications` | **NOT STARTED** |
| `delete-vpn-connection` | — | `--vpn-connection-id`, `--dry-run` | **NOT STARTED** |
| `describe-vpn-connections` | — | `--vpn-connection-ids`, `--filters` | **NOT STARTED** |
| `modify-vpn-connection` | — | `--vpn-connection-id`, `--vpn-gateway-id`, `--customer-gateway-id` | **NOT STARTED** |
| `modify-vpn-connection-options` | — | `--vpn-connection-id`, `--local-ipv4-network-cidr`, `--remote-ipv4-network-cidr` | **NOT STARTED** |

### EC2 — Network ACLs

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-network-acl` | — | `--vpc-id`, `--tag-specifications` | **NOT STARTED** |
| `delete-network-acl` | — | `--network-acl-id`, `--dry-run` | **NOT STARTED** |
| `describe-network-acls` | — | `--network-acl-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `create-network-acl-entry` | — | `--network-acl-id`, `--rule-number`, `--protocol`, `--rule-action`, `--cidr-block`, `--ingress`/`--egress`, `--port-range` | **NOT STARTED** |
| `delete-network-acl-entry` | — | `--network-acl-id`, `--rule-number`, `--ingress`/`--egress` | **NOT STARTED** |
| `replace-network-acl-association` | — | `--association-id`, `--network-acl-id` | **NOT STARTED** |
| `replace-network-acl-entry` | — | `--network-acl-id`, `--rule-number`, `--protocol`, `--rule-action`, `--cidr-block`, `--ingress`/`--egress`, `--port-range` | **NOT STARTED** |

### EC2 — Prefix Lists

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-managed-prefix-list` | — | `--prefix-list-name`, `--address-family`, `--max-entries`, `--entries`, `--tag-specifications` | **NOT STARTED** |
| `delete-managed-prefix-list` | — | `--prefix-list-id`, `--dry-run` | **NOT STARTED** |
| `describe-managed-prefix-lists` | — | `--prefix-list-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `modify-managed-prefix-list` | — | `--prefix-list-id`, `--current-version`, `--add-entries`, `--remove-entries`, `--prefix-list-name` | **NOT STARTED** |
| `get-managed-prefix-list-entries` | — | `--prefix-list-id`, `--target-version`, `--max-results` | **NOT STARTED** |
| `get-managed-prefix-list-associations` | — | `--prefix-list-id`, `--max-results` | **NOT STARTED** |

### EC2 — Launch Templates

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-launch-template` | — | `--launch-template-name`, `--launch-template-data`, `--tag-specifications` | **NOT STARTED** |
| `create-launch-template-version` | — | `--launch-template-id`/`--launch-template-name`, `--launch-template-data`, `--source-version` | **NOT STARTED** |
| `delete-launch-template` | — | `--launch-template-id`/`--launch-template-name`, `--dry-run` | **NOT STARTED** |
| `describe-launch-templates` | — | `--launch-template-ids`, `--launch-template-names`, `--filters` | **NOT STARTED** |
| `describe-launch-template-versions` | — | `--launch-template-id`/`--launch-template-name`, `--versions`, `--min-version`, `--max-version` | **NOT STARTED** |

### EC2 — Dedicated Hosts, IPv4 Pools, DHCP, Capacity Reservations

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `allocate-hosts` | — | `--availability-zone`, `--instance-type`, `--quantity`, `--auto-placement`, `--tag-specifications` | **NOT STARTED** |
| `describe-hosts` | — | `--host-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `release-hosts` | — | `--host-ids` | **NOT STARTED** |
| `create-public-ipv4-pool` | — | `--tag-specifications`, `--dry-run` | **NOT STARTED** |
| `delete-public-ipv4-pool` | — | `--pool-id`, `--dry-run` | **NOT STARTED** |
| `describe-public-ipv4-pools` | — | `--pool-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `create-dhcp-options` | — | `--dhcp-configurations`, `--tag-specifications` | **NOT STARTED** |
| `delete-dhcp-options` | — | `--dhcp-options-id`, `--dry-run` | **NOT STARTED** |
| `describe-dhcp-options` | — | `--dhcp-options-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `associate-dhcp-options` | — | `--dhcp-options-id`, `--vpc-id`, `--dry-run` | **NOT STARTED** |
| `create-capacity-reservation` | — | `--instance-type`, `--instance-platform`, `--availability-zone`, `--instance-count`, `--end-date`, `--end-date-type`, `--instance-match-criteria`, `--tag-specifications` | **NOT STARTED** |
| `cancel-capacity-reservation` | — | `--capacity-reservation-id`, `--dry-run` | **NOT STARTED** |
| `describe-capacity-reservations` | — | `--capacity-reservation-ids`, `--filters`, `--max-results` | **NOT STARTED** |
| `modify-capacity-reservation` | — | `--capacity-reservation-id`, `--instance-count`, `--end-date`, `--end-date-type` | **NOT STARTED** |

### EC2 — Misc

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `delete-network-interface-permission` | — | `--network-interface-permission-id`, `--force` | **NOT STARTED** |
| `enable-address-transfer` | — | `--allocation-id`, `--transfer-account-id` | **NOT STARTED** |
| `disable-address-transfer` | — | `--allocation-id` | **NOT STARTED** |

### EBS Direct API

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `start-snapshot` | — | `--volume-size`, `--parent-snapshot-id`, `--description`, `--encrypted` | **NOT STARTED** |
| `put-snapshot-block` | — | `--snapshot-id`, `--block-index`, `--block-data`, `--checksum` | **NOT STARTED** |
| `get-snapshot-block` | — | `--snapshot-id`, `--block-index` | **NOT STARTED** |
| `complete-snapshot` | — | `--snapshot-id`, `--changed-blocks-count` | **NOT STARTED** |
| `list-snapshot-blocks` | — | `--snapshot-id`, `--max-results`, `--next-token` | **NOT STARTED** |
| `list-changed-blocks` | — | `--second-snapshot-id`, `--first-snapshot-id`, `--max-results` | **NOT STARTED** |

---

## IAM

All IAM operations are account-scoped. Root user (account `000000000000`)
bypasses policy evaluation entirely.

### IAM — Users

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-user` | `--user-name`, `--path` | `--tags`, `--permissions-boundary` | **DONE** |
| `get-user` | `--user-name` | — | **DONE** |
| `list-users` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `delete-user` | `--user-name` | — | **DONE** |

### IAM — Access Keys

Max 2 keys per user. Secrets encrypted with AES-256-GCM using master key,
returned plaintext only at creation.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-access-key` | `--user-name` | — | **DONE** |
| `list-access-keys` | `--user-name` | `--max-items`, `--marker` | **DONE** |
| `delete-access-key` | `--access-key-id`, `--user-name` | — | **DONE** |
| `update-access-key` | `--access-key-id`, `--user-name`, `--status` (Active/Inactive) | — | **DONE** |

### IAM — Policies

Policy documents require `Version: "2012-10-17"` and a valid Statement array.
Wildcard action matching supported (`ec2:*`, `ec2:Describe*`, `*`). Evaluation
order: explicit Deny > explicit Allow > implicit Deny.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-policy` | `--policy-name`, `--policy-document`, `--path`, `--description` | `--tags` | **DONE** |
| `get-policy` | `--policy-arn` | — | **DONE** |
| `get-policy-version` | `--policy-arn`, `--version-id` | — | **DONE** |
| `list-policies` | — | `--scope`, `--only-attached`, `--path-prefix`, `--max-items`, `--marker` | **DONE** |
| `delete-policy` | `--policy-arn` | — | **DONE** |
| `attach-user-policy` | `--user-name`, `--policy-arn` | — | **DONE** |
| `detach-user-policy` | `--user-name`, `--policy-arn` | — | **DONE** |
| `list-attached-user-policies` | `--user-name` | `--path-prefix`, `--max-items`, `--marker` | **DONE** |

### IAM — Policy Evaluation (internal — not an AWS API)

| Feature | Status |
|---------|--------|
| Root user bypass | **DONE** |
| Default deny | **DONE** |
| Explicit allow | **DONE** |
| Explicit deny | **DONE** |
| Wildcard matching (`*`, `ec2:*`, `ec2:Describe*`) | **DONE** |
| Account-scoped evaluation | **DONE** |
| EC2 action mapping | **DONE** |
| IAM action mapping (16 actions) | **DONE** |

### IAM — Roles

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-role` | `--role-name`, `--assume-role-policy-document`, `--path`, `--description`, `--max-session-duration`, `--tags` | `--permissions-boundary` | **DONE** |
| `get-role` | `--role-name` | — | **DONE** |
| `list-roles` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `delete-role` | `--role-name` | — | **DONE** |
| `update-role` | `--role-name`, `--description`, `--max-session-duration` | — | **DONE** |
| `update-assume-role-policy` | `--role-name`, `--policy-document` | — | **DONE** |
| `attach-role-policy` | `--role-name`, `--policy-arn` | — | **DONE** |
| `detach-role-policy` | `--role-name`, `--policy-arn` | — | **DONE** |
| `list-attached-role-policies` | `--role-name`, `--path-prefix` | `--max-items`, `--marker` | **DONE** |

### IAM — Instance Profiles

Containers for IAM roles that allow EC2 instances to assume a role. Required
for `run-instances --iam-instance-profile`. Max 1 role per profile.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-instance-profile` | `--instance-profile-name`, `--path`, `--tags` | — | **DONE** |
| `get-instance-profile` | `--instance-profile-name` | — | **DONE** |
| `list-instance-profiles` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `list-instance-profiles-for-role` | `--role-name` | `--max-items`, `--marker` | **DONE** |
| `delete-instance-profile` | `--instance-profile-name` | — | **DONE** |
| `add-role-to-instance-profile` | `--instance-profile-name`, `--role-name` | — | **DONE** |
| `remove-role-from-instance-profile` | `--instance-profile-name`, `--role-name` | — | **DONE** |

### IAM — Groups

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-group` | — | `--group-name`, `--path` | **NOT STARTED** |
| `get-group` | — | `--group-name` | **NOT STARTED** |
| `list-groups` | — | `--path-prefix`, `--max-items`, `--marker` | **NOT STARTED** |
| `delete-group` | — | `--group-name` | **NOT STARTED** |
| `add-user-to-group` | — | `--group-name`, `--user-name` | **NOT STARTED** |
| `remove-user-from-group` | — | `--group-name`, `--user-name` | **NOT STARTED** |
| `list-groups-for-user` | — | `--user-name`, `--max-items`, `--marker` | **NOT STARTED** |
| `attach-group-policy` | — | `--group-name`, `--policy-arn` | **NOT STARTED** |
| `detach-group-policy` | — | `--group-name`, `--policy-arn` | **NOT STARTED** |
| `list-attached-group-policies` | — | `--group-name`, `--path-prefix`, `--max-items`, `--marker` | **NOT STARTED** |

---

## STS

| Command | Implemented Flags | Missing Flags (rejected if supplied) | Status |
|---------|-------------------|--------------------------------------|--------|
| `get-caller-identity` | — | — | **DONE** |
| `assume-role` | `--role-arn`, `--role-session-name`, `--duration-seconds` (900–min(role MaxSessionDuration, 43200)) | `--policy`, `--policy-arns` (→ `PackedPolicyTooLarge`); `--tags`, `--transitive-tag-keys` (→ `InvalidParameterValue`); `--serial-number`, `--token-code` (→ `InvalidParameterValue`); `--external-id`, `--source-identity` (accepted and logged, **not enforced** — no Condition evaluator in v1) | **DONE** |
| `get-session-token` | — | `--duration-seconds`, `--serial-number`, `--token-code` | **NOT STARTED** |
| `assume-role-with-web-identity` | — | `--role-arn`, `--role-session-name`, `--web-identity-token`, `--provider-id`, `--policy`, `--policy-arns`, `--duration-seconds` | **NOT STARTED** |
| `assume-role-with-saml` | — | `--role-arn`, `--principal-arn`, `--saml-assertion`, `--policy`, `--policy-arns`, `--duration-seconds` | **NOT STARTED** |
| `get-access-key-info` | — | `--access-key-id` | **NOT STARTED** |
| `get-federation-token` | — | `--name`, `--policy`, `--policy-arns`, `--duration-seconds`, `--tags` | **NOT STARTED** |
| `decode-authorization-message` | — | `--encoded-message` | **NOT STARTED** |

Trust policies stored on roles (`AssumeRolePolicyDocument`) reject `Condition`,
`NotPrincipal`, `NotAction`, empty-string `Action` elements, and empty
`Principal` blocks at write time (`MalformedPolicyDocument`) — v1 has no
Condition evaluator so accepting them would silently allow.

---

## ELBv2 (Application & Network Load Balancer)

The data plane uses a system-managed LB VM running HAProxy, launched automatically during `create-load-balancer`. HAProxy config is pushed via NATS on listener/target changes.

### ELBv2 — Load Balancers

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-load-balancer` | `--name`, `--subnets`, `--security-groups`, `--scheme` (internet-facing/internal), `--tags`, `--ip-address-type` (ipv4) | `--type` (hardcoded application), `--customer-owned-ipv4-pool`, `--dry-run` | **DONE** |
| `delete-load-balancer` | `--load-balancer-arn` | `--dry-run` | **DONE** |
| `describe-load-balancers` | `--load-balancer-arns`, `--names` | `--page-size`, `--marker`, `--dry-run` | **DONE** |

### ELBv2 — Target Groups

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-target-group` | `--name`, `--protocol` (HTTP), `--port`, `--vpc-id`, `--target-type` (instance), `--health-check-protocol`, `--health-check-port`, `--health-check-path`, `--health-check-interval-seconds`, `--health-check-timeout-seconds`, `--healthy-threshold-count`, `--unhealthy-threshold-count`, `--matcher`, `--tags` | `--health-check-enabled`, `--protocol-version`, `--ip-address-type`, `--dry-run` | **DONE** |
| `delete-target-group` | `--target-group-arn` | `--dry-run` | **DONE** |
| `describe-target-groups` | `--target-group-arns`, `--names`, `--load-balancer-arn` | `--page-size`, `--marker`, `--dry-run` | **DONE** |

### ELBv2 — Targets

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `register-targets` | `--target-group-arn`, `--targets` (Id, Port) | `--dry-run` | **DONE** |
| `deregister-targets` | `--target-group-arn`, `--targets` (Id, Port) | `--dry-run` | **DONE** |
| `describe-target-health` | `--target-group-arn`, `--targets` | `--include` | **DONE** |

### ELBv2 — Listeners

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-listener` | `--load-balancer-arn`, `--default-actions` (Type=forward, TargetGroupArn), `--protocol` (HTTP), `--port` | `--ssl-policy`, `--certificates`, `--alpn-policy`, `--mutual-authentication`, `--dry-run` | **DONE** |
| `delete-listener` | `--listener-arn` | `--dry-run` | **DONE** |
| `describe-listeners` | `--load-balancer-arn`, `--listener-arns` | `--page-size`, `--marker`, `--dry-run` | **DONE** |

### ELBv2 — Tags

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-tags` | `--resource-arns` (loadbalancer, targetgroup, listener) | — | **DONE** |

### ELBv2 — Not Yet Implemented

| Feature | Priority | Status |
|---------|----------|--------|
| Listener rules (CreateRule, DeleteRule, DescribeRules, ModifyRule) | High | **NOT STARTED** |
| HTTPS/TLS termination (SSL certificates, policies, ALPN) | High | **NOT STARTED** |
| Modify operations (`ModifyLoadBalancerAttributes`, `ModifyTargetGroup`, `ModifyTargetGroupAttributes`, `ModifyListener`) | Medium | **NOT STARTED** |
| Connection draining (deregistration delay) | Medium | **NOT STARTED** |
| Stickiness / session affinity | Medium | **NOT STARTED** |
| Active health checking (API-driven, vs. HAProxy-only today) | Medium | **NOT STARTED** |
| IP and Lambda target types | Low | **NOT STARTED** |
| S3 access log delivery | Low | **NOT STARTED** |
| WAF integration | Low | **NOT STARTED** |

---

## CloudWatch (Basic Monitoring)

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `put-metric-data` | — | `--namespace`, `--metric-data` | **NOT STARTED** |
| `get-metric-statistics` | — | `--namespace`, `--metric-name`, `--start-time`, `--end-time`, `--period`, `--statistics`, `--dimensions` | **NOT STARTED** |
| `list-metrics` | — | `--namespace`, `--metric-name`, `--dimensions`, `--recently-active` | **NOT STARTED** |
| `describe-alarms` | — | `--alarm-names`, `--alarm-name-prefix`, `--state-value`, `--action-prefix` | **NOT STARTED** |
| `put-metric-alarm` | — | `--alarm-name`, `--namespace`, `--metric-name`, `--statistic`, `--period`, `--evaluation-periods`, `--threshold`, `--comparison-operator`, `--alarm-actions`, `--dimensions` | **NOT STARTED** |
| `delete-alarms` | — | `--alarm-names` | **NOT STARTED** |

---

## Auto Scaling

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-auto-scaling-group` | — | `--auto-scaling-group-name`, `--launch-template`, `--min-size`, `--max-size`, `--desired-capacity`, `--vpc-zone-identifier`, `--target-group-arns`, `--health-check-type`, `--health-check-grace-period`, `--tags` | **NOT STARTED** |
| `update-auto-scaling-group` | — | `--auto-scaling-group-name`, `--min-size`, `--max-size`, `--desired-capacity`, `--launch-template`, `--health-check-type`, `--health-check-grace-period` | **NOT STARTED** |
| `delete-auto-scaling-group` | — | `--auto-scaling-group-name`, `--force-delete` | **NOT STARTED** |
| `describe-auto-scaling-groups` | — | `--auto-scaling-group-names`, `--filters`, `--max-records` | **NOT STARTED** |
| `set-desired-capacity` | — | `--auto-scaling-group-name`, `--desired-capacity`, `--honor-cooldown` | **NOT STARTED** |
| `describe-auto-scaling-instances` | — | `--instance-ids`, `--max-records` | **NOT STARTED** |
| `put-scaling-policy` | — | `--auto-scaling-group-name`, `--policy-name`, `--policy-type`, `--target-tracking-configuration`, `--scaling-adjustment`, `--cooldown` | **NOT STARTED** |
| `delete-scaling-policy` | — | `--auto-scaling-group-name`, `--policy-name` | **NOT STARTED** |
