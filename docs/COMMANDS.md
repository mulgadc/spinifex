# Command Implementation Matrix

## Spinifex Admin CLI

Platform management commands not exposed via the AWS gateway API. These are CLI-only commands.

### Service Management

Service lifecycle commands for starting, stopping, and checking status of all Spinifex cluster services. Each service subcommand supports `start`, `stop`, and `status` operations.

| Command | Flags | Description |
|---------|-------|-------------|
| `spx service predastore start` | `--port` (default: 8443), `--host` (default: 0.0.0.0), `--base-path`, `--config-path`, `--debug`, `--tls-cert`, `--tls-key`, `--backend` (distributed/filesystem, default: distributed), `--node-id` (default: 0), `--pprof`, `--pprof-output` | Creates predastore service instance with S3-compatible storage backend → starts service |
| `spx service predastore stop` | — | Stops the predastore service |
| `spx service predastore status` | — | Reports predastore service status |
| `spx service viperblock start` | `--s3-host` (default: 0.0.0.0:8443), `--s3-bucket` (default: predastore), `--s3-region` (default: ap-southeast-2), `--plugin-path` (auto-detected via `nbdkit --dump-config plugindir`; typically `/usr/lib/x86_64-linux-gnu/nbdkit/plugins/nbdkit-viperblock-plugin.so` on amd64, overridable via `SPINIFEX_VIPERBLOCK_PLUGIN_PATH` in `/etc/spinifex/systemd.env`) | Loads cluster config → connects to NATS and Predastore → starts viperblock block storage service with NBD plugin |
| `spx service viperblock stop` | — | Stops the viperblock service |
| `spx service viperblock status` | — | Reports viperblock service status |
| `spx service nats start` | `--port` (default: 4222), `--host` (default: 0.0.0.0), `--debug`, `--data-dir`, `--jetstream` | Starts embedded NATS server with optional JetStream |
| `spx service nats stop` | — | Stops the NATS service |
| `spx service nats status` | — | Reports NATS service status |
| `spx service spinifex start` | `--wal-dir` | Loads cluster config → starts spinifex daemon (VM orchestration, NATS subscriptions, health endpoint) |
| `spx service spinifex stop` | — | Stops the spinifex daemon service |
| `spx service spinifex status` | — | Reports spinifex daemon service status |
| `spx service awsgw start` | `--host` (default: 0.0.0.0:9999), `--tls-cert`, `--tls-key`, `--debug` | Loads cluster config → starts AWS-compatible gateway with SigV4 auth, IAM policy enforcement, TLS |
| `spx service awsgw stop` | — | Stops the AWS gateway service |
| `spx service awsgw status` | — | Reports AWS gateway service status |
| `spx service spinifex-ui start` | `--port` (default: 3000), `--host` (default: 0.0.0.0), `--tls-cert`, `--tls-key` | Starts embedded web UI server serving the React frontend. Aliases: `ui`, `spinifexui` |
| `spx service spinifex-ui stop` | — | Stops the spinifex-ui service |
| `spx service spinifex-ui status` | — | Reports spinifex-ui service status |
| `spx service vpcd start` | — | Loads cluster config → starts VPC daemon (subscribes to `vpc.*` NATS events, translates to OVN logical switches/ports/routers) |
| `spx service vpcd stop` | — | Stops the vpcd service |
| `spx service vpcd status` | — | Reports vpcd service status |
| `spx service qmp-collector start` | — | Starts the guest-metrics collector (polls per-VM telemetry QMP sockets + tap counters, publishes CloudWatch-shaped series to NATS `metrics.ec2.*`) |
| `spx service qmp-collector stop` | — | Stops the qmp-collector service |
| `spx service qmp-collector status` | — | Reports qmp-collector service status |

### Cluster Inspection

Operational commands for inspecting cluster state. These fan out NATS requests to all nodes and aggregate responses.

| Command | Flags | Description |
|---------|-------|-------------|
| `spx get nodes` | `--timeout` (default: 3s) | Loads config → publishes to `spinifex.node.status` fan-out topic → collects responses within timeout → merges config-known nodes with NATS responders → nodes that don't respond shown as `NotReady` |
| `spx get vms` | `--timeout` (default: 3s) | Publishes to `spinifex.node.vms` fan-out topic → collects VM info from all nodes → sorts by node then instance ID → prints table. Alias: `spx get instances` |

### Resource Monitoring

| Command | Flags | Description |
|---------|-------|-------------|
| `spx top nodes` | `--timeout` (default: 3s) | Publishes to `spinifex.node.status` fan-out topic → collects CPU/memory usage per node → aggregates instance type capacity across all nodes → prints two tables: per-node resource usage and cluster-wide instance type availability |

### Cluster Initialization

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin init` | `--nodes`, `--node`, `--bind`, `--port`, `--region`, `--az`, `--cluster-name`, `--cluster-bind`, `--cluster-routes`, `--predastore-nodes`, `--services`, `--formation-timeout`, `--token-ttl`, `--force`, `--external-mode` (`pool` \| `nat` routed NAT for non-bridgeable uplinks, single-node; pair with `setup-ovn.sh --nat-uplink`; add `--external-pool start-end --external-gateway <ip>` or `--external-source=dhcp [--external-bind-bridge <iface>]` for a public pool with full EIP support; note: system instances (ECS/EKS/load-balancer agents) are unsupported in nat v1 — they require `pool` mode) | Generates root IAM credentials (AKIA-prefixed access key + secret) → creates master.key (AES-256, 32 bytes, 0600) → writes bootstrap.json (consumed on first start) → generates CA + server TLS certificates → generates join token (written to `join-token` file, displayed in join command) → creates NATS config with auth token → writes spinifex.toml, awsgw.toml, predastore.toml → configures AWS CLI `spx` profile → creates directory structure under `~/spinifex/`. `--force` on an already-initialized node is idempotent for crypto: it preserves the existing keys (master.key, predastore/viperblock encryption keys), system + admin credentials, and CA, and refreshes only the config files and the CA-signed server certificate (so a changed bind IP / SANs is picked up without breaking already-joined nodes or CA-baked AMIs). A genuine clean slate requires removing the data dirs first. |
| `spx admin join` | `--host` (required), `--node` (required), `--token` (required), `--bind`, `--port`, `--region`, `--az`, `--cluster-bind`, `--cluster-routes`, `--data-dir`, `--services` | Connects to leader node with join token (Authorization: Bearer header) → retrieves cluster configuration → configures local node to join cluster and participate in distributed operations |

### Version

| Command | Flags | Description |
|---------|-------|-------------|
| `spx version` | — | Prints Spinifex version, commit hash, OS, and architecture (populated via build-time ldflags) |

### Cluster Operations

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin cluster shutdown` | `--force` (shutdown even if nodes don't respond), `--timeout` (max wait per phase, default 120s), `--dry-run` (print phase plan without executing) | Performs coordinated, phased shutdown of entire cluster. Phases execute in order: GATE (stop API/UI) → DRAIN (stop VMs) → STORAGE (stop viperblock) → PERSIST (stop predastore) → INFRA (stop NATS/daemon). Each phase waits for all nodes to ACK before proceeding. Uses JetStream state tracking. |
| `spx admin cluster drain-dhcp` | `--timeout` (reply-collection window, default 30s) | Asks each vpcd to DHCPRELEASE every external-pool DHCP lease it currently holds, returning them to the upstream DHCP server. Run on teardown before stopping services — an env reset otherwise strands held leases upstream until their TTL expires, eventually exhausting the upstream scope. Best-effort: warns and exits 0 if the cluster is already down. |
| `spx admin node drain --local` | `--local` (drain the local node only, required), `--timeout` (max wait per phase, default 120s) | Runs the GATE and DRAIN phases against the local node only: powers down its guests via QMP and unmounts their volumes (flushing the viperblock WAL) while every service is still up. STORAGE/PERSIST/INFRA are left to systemd's ordered teardown. Wired as the `ExecStop` of `spinifex-shutdown.service`, so `systemctl stop spinifex.target` and host reboot/poweroff drain guests before any storage service stops. |

### Certificate Management

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin cert renew` | `--extra-ip` (additional IPs for SANs), `--extra-dns` (additional DNS names for SANs) | Reads existing CA → regenerates server certificate with all current network interface IPs and machine hostname in SANs → writes new cert. Use after adding a new network interface or changing IP addresses. |

### Upgrade Management

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin upgrade` | `--yes` (apply without prompting), `--config-dir` (persistent, default: `~/spinifex/config`), `--spinifex-dir` (persistent, default: `~/spinifex`) | Reads current config versions from registry → computes pending config migrations (from→to per target) → prints version summary and pending list → prompts for confirmation unless `--yes` → runs `migrate.DefaultRegistry.RunAllConfig()` to apply migrations to config files. Intended for upgrades between Spinifex versions. Operators can skip migrations during install by setting `INSTALL_SPINIFEX_SKIP_MIGRATE=1`, then run `spx admin upgrade` manually to review and apply. After completion, services must be restarted with `sudo systemctl restart spinifex.target`. Invoked non-interactively by `scripts/setup.sh` with `--yes`. |

### Account Management

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin account create` | `--name` | Connects to NATS → CAS loop on `spinifex-account-counter:next_id` for sequential 12-digit ID → creates Account record in `spinifex-accounts` KV → creates `admin` user in new account → creates access key for admin → creates AdministratorAccess policy (Action:*, Resource:*) → attaches policy → prints credentials |
| `spx admin account list` | — | Connects to NATS → IAMService.ListAccounts() → prints table with Account ID, Name, Status, Created |

### Image Management

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin images import` | `--name`, `--file`, `--distro`, `--version`, `--arch`, `--platform`, `--boot-mode` (bios/uefi/uefi-preferred), `--tag`, `--force`, `--skip-verify` | Catalog imports (`--name`) download the image, fetch the catalog `Checksum` URL, verify the SHA-256/SHA-512 digest, and inherit `BootMode` from the catalog entry. `--boot-mode` overrides the catalog value when set. Mismatch fails closed; the cached file is left on disk and `--force` re-downloads. `--file` imports skip checksum verification (operator-supplied media is outside Spinifex's trust boundary, the skip is logged at INFO for audit) and require an explicit `--boot-mode` because there is no catalog metadata to inherit from. `--skip-verify` bypasses verification for catalog imports and emits a WARN slog + stderr notice; use only for debugging or when upstream mirrors are confirmed-broken. On every import a best-effort `virt-customize` step bakes the deployment CA (`<data-root>/config/ca.pem`) into the image's trust store before the block copy, so in-guest SDK-over-TLS calls to Spinifex endpoints trust the gateway from first boot; an image libguestfs cannot customize is imported as-is (CA-free) and logs a skip. Because the CA is fixed into the image, rotating the cluster CA requires re-importing affected images. |
| `spx admin images list` | — | Lists available OS images that can be imported or downloaded |
| `spx admin images promote` | `--image-id` (required), `--yes` | Reads `ami-<id>/config.json`, validates the AMI is account-owned, then rewrites `ImageOwnerAlias` to `"system"` in-place. No block data is copied. The change takes effect immediately — the AMI becomes visible to all accounts via `DescribeImages`. Prompts for confirmation (skipped with `--yes`). Already-system AMIs are refused. |
| `spx admin images remove` | `--image-id` (required), `--force`, `--yes` | Loads `ami-<id>/config.json`, walks transitive dependents — copied snapshots whose `VolumeID == imageID`, volumes whose `SnapshotID` references the internal `snap-ami-<id>` or any derived snap, and account AMIs created via `CopyImage` whose `SnapshotID` is a derived snap — then prompts (skipped with `--yes`) before deleting `ami-<id>/config.json` (the DescribeImages barrier) followed by the rest of `ami-<id>/` and `snap-ami-<id>/`. Account-owned AMIs are refused with a hint pointing at `aws ec2 deregister-image` + `aws ec2 delete-snapshot`. `--force` bypasses the dependency, ownership and config-corrupt checks for salvage of orphaned blocks. |

#### Image integrity verification (CMMC SI.L1-3.14.2)

Catalog imports (`spx admin images import --name <name>`) verify the image
against the catalog-declared SHA-256/SHA-512 digest before extraction. The sums
file is fetched from the catalog `Checksum` URL over HTTPS only (cross-scheme
redirects refused), and verification runs on both fresh downloads and cache
hits so a poisoned cache is caught on the next import.

On mismatch the import exits non-zero, the cached file is left on disk for
inspection, and the printed guidance is `spx admin images import --name <name>
--force` to re-download.

`--file` imports skip verification by design: operator-supplied media is
outside Spinifex's trust boundary and the operator is responsible for
integrity (e.g. `sha256sum` against a trusted upstream digest before import).
The skip is recorded as an INFO `slog` event with `reason=local-file-import`
so a CMMC assessor can audit the decision from journald.

`--skip-verify` bypasses the checksum step for catalog imports. The command
still downloads via the catalog URL but does not compare the image digest
against the sums file. Intended for narrow cases such as debugging upstream
mirror issues or running against a transiently-broken `latest/` path; the
skip is logged at WARN with `reason=skip-verify-flag` and printed to stderr
so operators and assessors see it. Prefer `--file` with an out-of-band
verified image over `--skip-verify` whenever possible.

**Limitation:** verification confirms the image matches the digest the mirror
served. A mirror compromise that swaps both image and sums file is not
detected; closing that gap requires GPG signature verification of the sums
file, deferred to a later phase.

#### `spx admin images remove` caveats

Admin-imported AMIs (`ImageOwnerAlias = "system"`) live
under the `ami-<id>/` S3 prefix and use a viperblock-internal snap checkpoint
at `snap-ami-<id>/` — there is no `snap-<id>/metadata.json`. The AWS handlers
(`DeregisterImage`, `DeleteSnapshot`) reject system owners with
`UnauthorizedOperation`, which is the right behaviour for tenant API callers
but leaves no AWS-flow path to reclaim space. `spx admin images remove` is
the admin-trust-boundary counterpart that performs the dependency walk and
hard-deletes the blocks directly against predastore.

`CopyImage` of a system AMI is metadata-only: it writes a fresh
`snap-<acct>/metadata.json` whose `VolumeID` points at `ami-<sys>` and a new
`ami-<acct>/config.json` referencing that snap. Volumes launched from the
copied AMI read transitively from `ami-<sys>/chunks/...`. The remove command
walks this transitive set and refuses if anything references the target.

**TOCTOU window:** between the safety scan and the `config.json` delete a
concurrent `RunInstances` against the AMI could create a new dependent
volume. The window is sub-second on a healthy cluster. The admin running
this command is expected to know the fleet's operational state; if the race
fires the result is a `vol-<id>` with deleted backing blocks, recovered by
terminating the orphaned instance.

**`--force` bypasses every safety check** (dependents, ownership, missing /
corrupt `config.json`). Use only for salvage of orphaned blocks. Running it
against a live system AMI corrupts every dependent volume on the next disk
read.

### GPU Management

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin gpu status` | `--node` (default: local node) | Queries `spinifex.node.status` fan-out → finds the target node response → prints Node, GPU hardware (model list or "none detected"), IOMMU state, vfio-pci state, passthrough enabled/disabled, and GPU pool allocation (`allocated/total`). Also lists GPU-capable instance types when passthrough is active. |
| `spx admin gpu enable` | — | Checks current passthrough state via NATS → errors if already enabled or prerequisites not met (directs to `setup`) → writes `gpu_passthrough = true` to `spinifex.toml` via `admin.SetGPUPassthrough` → sends SIGHUP to `spinifex-daemon` via `systemctl kill -s HUP` → polls node status for up to 30 s until daemon confirms new state → prints final `gpu status` output. Must be run directly on the target host. |
| `spx admin gpu disable` | — | Checks current passthrough state via NATS → errors if already disabled or if `AllocGPUs > 0` (must terminate GPU instances first) → writes `gpu_passthrough = false` → sends SIGHUP to `spinifex-daemon` → polls for up to 30 s → prints final `gpu status` output. Must be run directly on the target host. |
| `spx admin gpu setup` | — | Idempotent host configuration for GPU passthrough. Steps (skipped if already applied): detect GPUs via `gpu.Discover` → collect PCI IDs for all IOMMU-group siblings → check/enable IOMMU in GRUB (`intel_iommu=on iommu=pt` or `amd_iommu=on iommu=pt`) → write vfio udev rule (`/etc/udev/rules.d/99-spinifex-vfio.rules`) → blacklist nouveau (`/etc/modprobe.d/blacklist-nouveau.conf`) → blacklist amdgpu if AMD GPU present (`/etc/modprobe.d/blacklist-amdgpu.conf`) → write vfio-pci early binding config (`/etc/modprobe.d/vfio-pci.conf`) → add vfio modules to initramfs. If any change requires a reboot: runs `update-initramfs -u` and exits with reboot instructions. After reboot: verifies `vfio_pci` module is loaded → verifies each GPU is bound to `vfio-pci` (binds explicitly via `driver_override` if unbound) → calls `gpu enable` to activate passthrough. |
| `spx admin gpu mig status` | — (must be run on target host) | Runs `gpu.Discover()` locally → for each GPU prints: PCI address, model, MIG capability, MIG mode (enabled/disabled/N/A); for GPUs with MIG enabled lists active MIG slices with GI ID, profile name, and mdev path. |
| `spx admin gpu mig enable` | `--profile <name>` (required, e.g. `1g.10gb`), `--gpu <pci-addr>` (optional, default: all MIG-capable GPUs) | Checks no GPU instances running (NATS) → discovers MIG-capable GPUs via `gpu.Discover()` (filtered by `--gpu` if set) → enables MIG mode on each target (`gpu.EnableMIGMode`) → lists available profiles (`gpu.ListProfiles`) and validates requested profile name → destroys any existing instances (`gpu.DestroyAllInstances`) → creates new instances filling GPU capacity (`gpu.CreateInstances`) → writes `mig_profile` to `spinifex.toml` via `admin.SetMIGProfile` → sends SIGHUP to `spinifex-daemon`. Must be run directly on the target host. |
| `spx admin gpu mig disable` | `--gpu <pci-addr>` (optional, default: all MIG-capable GPUs) | Checks no GPU instances running (NATS) → discovers MIG-capable GPUs via `gpu.Discover()` (filtered by `--gpu` if set) → destroys all GPU instances (`gpu.DestroyAllInstances`) → disables MIG mode (`gpu.DisableMIGMode`) → clears `mig_profile` in `spinifex.toml` via `admin.SetMIGProfile` → sends SIGHUP to `spinifex-daemon`. Must be run directly on the target host. |

### EKS Control-Plane Disaster Recovery

| Command | Flags | Description |
|---------|-------|-------------|
| `spx admin eks restore-snapshot` | `--cluster` (required), `--snapshot` (optional, defaults to the latest snapshot in predastore), `--account` (optional, defaults to the bootstrap account) | Single-CP total-loss DR path (fail-safe): validates the snapshot exists in predastore BEFORE any mutation (a typo'd/missing key hard-fails, never resets into an empty datastore) → launches a fresh control-plane VM as a cluster-init seed (replaying the persisted create-time launch template) → sets a required-snapshot `RecoveryDirective` (`cluster-reset`) so the boot-time recovery agent aborts rather than resets-into-empty if it cannot fetch the snapshot → persists the replacement in cluster meta BEFORE re-pointing the NLB (so an NLB failure is convergeable by the reconciler, returned as a provisional status, not a hard error) → re-points the cluster NLB's apiserver and konnectivity target groups from the old CP's ENI to the new one → fences the old CP with retries, failing loudly if it cannot be confirmed terminated (split-brain guard). Any failure before the meta commit unwinds the fresh CP (terminate + clear directive) so a re-run does not stack a second resetting control plane. The returned status is provisional — success means the sequence completed, not that etcd is restored and serving; verify cluster health. HA clusters (a spread with a potentially surviving quorum) are rejected — recover those via quorum reformation instead. |

## AWS-Compatible API

### EC2 — Instance Management

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `run-instances` | `--image-id`, `--instance-type`, `--count`, `--key-name`, `--user-data`, `--subnet-id`, `--security-group-ids`, `--tag-specifications` (instance-scoped), `--block-device-mappings` (DeviceName, VolumeSize, VolumeType, Iops, DeleteOnTermination), `--placement` (GroupName), `--iam-instance-profile` (Name/Arn), `--capacity-reservation-specification` (CapacityReservationTarget.CapacityReservationId, targeted-by-id only), `--metadata-options` (HttpPutResponseHopLimit; IMDSv2-only enforced — rejects `http-tokens=optional`), `--launch-template` (LaunchTemplateId/LaunchTemplateName, Version — resolves `$Default`/`$Latest`; direct params override the template) | `--dry-run`, `--client-token`, `--disable-api-termination`, `--ebs-optimized`, `--network-interfaces`, `--private-ip-address`, `--monitoring`, `--credit-specification`, `--cpu-options`, `--hibernate-options` | **DONE** |
| `describe-instances` | `--instance-ids`, `--filters` (instance-state-name, instance-id, instance-type, vpc-id, subnet-id, tag:*, tag-key, tag-value) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `start-instances` | `--instance-ids` | `--dry-run`, `--force` | **DONE** |
| `stop-instances` | `--instance-ids` | `--force`, `--hibernate`, `--dry-run` | **DONE** |
| `terminate-instances` | `--instance-ids`, `DeleteOnTermination` (per-volume) | `--dry-run` | **DONE** |
| `reboot-instances` | `--instance-ids` | `--dry-run` | **DONE** |
| `describe-instance-types` | `--filters` (capacity only) | `--instance-types`, `--max-results`, `--next-token`, `--dry-run`, other filters | **DONE** |
| `modify-instance-attribute` | `--instance-id`, `--instance-type`, `--user-data`, `--disable-api-termination` | `--ebs-optimized`, `--source-dest-check`, `--instance-initiated-shutdown-behavior`, `--block-device-mappings`, `--groups`, `--ena-support`, `--sriov-net-support` | **DONE** |
| `get-console-output` | `--instance-id` | `--latest`, `--dry-run` | **DONE** |
| `describe-instance-attribute` | `--instance-id`, `--attribute` (instanceType, userData, disableApiTermination, instanceInitiatedShutdownBehavior, disableApiStop, ebsOptimized, enaSupport, sourceDestCheck, rootDeviceName, kernel, ramdisk) | `--dry-run` | **DONE** |
| `modify-instance-metadata-options` | `--instance-id`, `--http-put-response-hop-limit` (1–64), `--http-tokens` (`required`), `--http-endpoint` (`enabled`), `--http-protocol-ipv6`/`--instance-metadata-tags` (`disabled`) — secure values are no-ops, downgrades return `UnsupportedOperation` | `--dry-run` | **DONE** |
| `describe-instance-credit-specifications` | `--instance-ids` | `--filters`, `--max-results`, `--dry-run` | **DONE** (stub — always returns `standard`) |
| `describe-instance-status` | `--instance-ids`, `--include-all-instances`, `--filters` (availability-zone, instance-state-code, instance-state-name, tag:*) | `--max-results`, `--next-token`, `--dry-run`, event/instance-status/system-status filters | **DONE** (static health) |
| `monitor-instances` | — | `--instance-ids` | **NOT STARTED** |
| `unmonitor-instances` | — | `--instance-ids` | **NOT STARTED** |

### EC2 — Spot Instances

Spot Instance Requests (SIRs) are a **mock** over the on-demand `run-instances` path: a request synchronously launches real VMs on the operator's own compute and is then reported `active`/`fulfilled`. There is no spot market — no bidding, price rejection, interruption, or reclamation, and instances are never reclaimed.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `request-spot-instances` | `--instance-count` (default 1), `--type` (`one-time`/`persistent` — stored, behaviour identical), `--spot-price` (echoed only), `--client-token`, `--launch-specification` (ImageId, InstanceType, KeyName, SubnetId, SecurityGroupIds, UserData, BlockDeviceMappings, IamInstanceProfile, Placement.GroupName, NetworkInterfaces), `--tag-specifications` (spot-instances-request) | `--valid-from`, `--valid-until`, `--launch-group`, `--availability-zone-group`, `--block-duration-minutes`, `--instance-interruption-behavior`, `--dry-run` | **DONE** (mock) |
| `describe-spot-instance-requests` | `--spot-instance-request-ids`, `--filters` (spot-instance-request-id, state, instance-id, launch.image-id, launch.instance-type, launch.key-name, type, launched-availability-zone, tag-key, tag:*) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `cancel-spot-instance-requests` | `--spot-instance-request-ids` | `--dry-run` | **DONE** |

`describe-spot-price-history` is **unsupported** (returns `InvalidAction`): on owned hardware there is no spot/on-demand price differential, so any synthetic price would be misleading rather than helpful.

### EC2 — IAM Instance Profile Associations

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `associate-iam-instance-profile` | `--instance-id`, `--iam-instance-profile` (Name/Arn) | `--dry-run` | **DONE** |
| `disassociate-iam-instance-profile` | `--association-id` | `--dry-run` | **DONE** |
| `replace-iam-instance-profile-association` | `--association-id`, `--iam-instance-profile` (Name/Arn) | `--dry-run` | **DONE** |
| `describe-iam-instance-profile-associations` | `--association-ids`, `--filters` (instance-id, state) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |

### EC2 — Key Pairs

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-key-pair` | `--key-name`, `--key-type` (rsa/ed25519), `--tag-specifications` | `--key-format`, `--dry-run` | **DONE** |
| `describe-key-pairs` | `--key-names`, `--key-pair-ids`, `--filters` (key-pair-id, key-name, fingerprint, tag:*) | `--max-results`, `--dry-run` | **DONE** |
| `delete-key-pair` | `--key-name`, `--key-pair-id` | `--dry-run` | **DONE** |
| `import-key-pair` | `--key-name`, `--public-key-material`, `--tag-specifications` | `--dry-run` | **DONE** |

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

`copy-image` is metadata-only — no block copy. The new snapshot inherits the. source `VolumeID`.

### EC2 — Volumes (EBS)

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-volumes` | `--volume-ids`, `--filters` (volume-id, status, size, volume-type, attachment.instance-id, attachment.status, attachment.device, availability-zone, tag:*), persisted `DeleteOnTermination` | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `create-volume` | `--size`, `--availability-zone`, `--volume-type` (gp3), `--snapshot-id`, `--tag-specifications` | `--iops` (hardcoded 3000), `--encrypted` (hardcoded false), `--throughput` | **DONE** |
| `delete-volume` | `--volume-id` | `--dry-run` | **DONE** |
| `modify-volume` | `--volume-id`, `--size`, `--volume-type`, `--iops` | `--throughput`, `--dry-run`, `--multi-attach-enabled` | **DONE** |
| `attach-volume` | `--volume-id`, `--instance-id`, `--device` (auto-assigns `/dev/sd[f-p]`) | `--dry-run` | **DONE** |
| `detach-volume` | `--volume-id`, `--instance-id` (optional), `--device`, `--force` | `--dry-run` | **DONE** |
| `describe-volume-status` | `--volume-ids`, `--filters` (volume-id, volume-status.status, availability-zone) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `describe-volumes-modifications` | `--volume-ids`, `--filters` (modification-state, original-iops, original-size, original-volume-type, start-time, target-iops, target-size, target-volume-type, volume-id) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |

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

Persistence works but stored values are not yet enforced by downstream services.

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

The account-level `modify-/get-instance-metadata-defaults` are blocked on an `aws-sdk-go` bump: the `InstanceMetadataDefaultsResponse` type is absent from our version (these APIs landed ~March 2024), and the gateway's generic handler needs the typed input struct to route them.

### EC2 — VPC Core

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

KV CRUD only — no OVN/OVS integration. EIGWs are stored but have no effect on network topology.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-egress-only-internet-gateway` | `--vpc-id`, `--tag-specifications` | `--client-token`, `--dry-run` | **STARTED** (KV only, no OVN) |
| `delete-egress-only-internet-gateway` | `--egress-only-internet-gateway-id` | `--dry-run` | **STARTED** (KV only, no OVN) |
| `describe-egress-only-internet-gateways` | `--egress-only-internet-gateway-ids`, `--filters` (egress-only-internet-gateway-id, tag:*) | `--max-results`, `--next-token`, `--dry-run` | **STARTED** (KV only, no OVN) |

### EC2 — Route Tables

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-route-table` | `--vpc-id`, `--tag-specifications` | `--dry-run` | **DONE** |
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

EIP handlers are always registered. Without a public IPAM pool (external mode disabled, or `nat` without a public pool), `describe-addresses` returns an empty list and mutating commands return `UnsupportedOperation`. In `nat` mode a public pool (`--external-pool` or `--external-source=dhcp` at init) enables the full EIP surface with host-delivered ingress.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `allocate-address` | `--public-ipv4-pool`, `--tag-specifications` | `--domain`, `--dry-run` | **DONE** |
| `release-address` | `--allocation-id` | `--dry-run` | **DONE** |
| `associate-address` | `--allocation-id`, `--network-interface-id`, `--instance-id`, `--private-ip-address` | `--dry-run`, `--allow-reassociation` | **DONE** |
| `disassociate-address` | `--association-id` | `--dry-run` | **DONE** |
| `describe-addresses` | `--allocation-ids`, `--public-ips`, `--filters` (allocation-id, public-ip, instance-id, association-id, domain, tag:*) | `--dry-run` | **DONE** |
| `describe-addresses-attribute` | `--allocation-ids` | `--attribute`, `--dry-run`, `--max-results`, `--next-token` | **DONE** |

### EC2 — NAT Gateway

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-nat-gateway` | `--subnet-id`, `--allocation-id`, `--tag-specifications` | `--connectivity-type`, `--dry-run` | **DONE** |
| `delete-nat-gateway` | `--nat-gateway-id` | `--dry-run` | **DONE** |
| `describe-nat-gateways` | `--nat-gateway-ids`, `--filters` (vpc-id, state) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `assign-private-nat-gateway-address` | — | `--nat-gateway-id`, `--private-ip-addresses` | **NOT STARTED** |
| `associate-nat-gateway-address` | — | `--nat-gateway-id`, `--allocation-ids` | **NOT STARTED** |

### EC2 — Placement Groups

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-placement-group` | `--group-name`, `--strategy` (spread/cluster), `--tag-specifications` | `--partition-count`, `--spread-level`, `--dry-run` | **DONE** |
| `delete-placement-group` | `--group-name` | `--dry-run` | **DONE** |
| `describe-placement-groups` | `--group-names`, `--group-ids`, `--filters` (strategy, state, spread-level, group-name, tag:*, tag-key, tag-value) | `--dry-run` | **DONE** |

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
| `create-launch-template` | `--launch-template-name`, `--launch-template-data` (full nested RequestLaunchTemplateData), `--version-description`, `--tag-specifications` (launch-template-scoped), `--dry-run` (no-op) | `--client-token` (idempotency) | **DONE** |
| `create-launch-template-version` | `--launch-template-id`/`--launch-template-name`, `--launch-template-data`, `--source-version` (clone-and-override), `--version-description`, `--dry-run` (no-op) | `--client-token`, `--resolve-alias` | **DONE** |
| `delete-launch-template` | `--launch-template-id`/`--launch-template-name`, `--dry-run` (no-op) | — | **DONE** |
| `delete-launch-template-versions` | `--launch-template-id`/`--launch-template-name`, `--versions` (rejects the current default version), `--dry-run` (no-op) | — | **DONE** |
| `modify-launch-template` | `--launch-template-id`/`--launch-template-name`, `--default-version`, `--dry-run` (no-op) | — | **DONE** |
| `describe-launch-templates` | `--launch-template-ids`, `--launch-template-names`, `--filters` (launch-template-id, launch-template-name, create-time, tag:*, tag-key) | `--max-results`, `--next-token`, `--dry-run` | **DONE** |
| `describe-launch-template-versions` | `--launch-template-id`/`--launch-template-name`, `--versions` (`$Default`/`$Latest`/numeric), `--min-version`, `--max-version`, `--filters` (is-default-version, image-id, instance-type, kernel-id, ram-disk-id, ebs-optimized) | `--max-results`, `--next-token`, `--dry-run`, `--resolve-alias` | **DONE** |

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
| `create-capacity-reservation` | `--instance-type`, `--instance-count`, `--availability-zone`, `--instance-platform`, `--instance-match-criteria`, `--tenancy` (default only), `--dry-run` | `--end-date`, `--end-date-type` (unlimited only), `--availability-zone-id`, `--tag-specifications` | **DONE** |
| `cancel-capacity-reservation` | `--capacity-reservation-id`, `--dry-run` | — | **DONE** |
| `describe-capacity-reservations` | `--capacity-reservation-ids`, `--filters` | `--max-results`, `--next-token` | **DONE** |
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

All IAM operations are account-scoped. Root user (account `000000000000`) bypasses policy evaluation entirely.

### IAM — Users

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-user` | `--user-name`, `--path` | `--tags`, `--permissions-boundary` | **DONE** |
| `get-user` | `--user-name` | — | **DONE** |
| `list-users` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `delete-user` | `--user-name` | — | **DONE** |
| `update-user` | — | `--user-name`, `--new-path`, `--new-user-name` | **NOT STARTED** |
| `put-user-policy` | `--user-name`, `--policy-name`, `--policy-document` | — | **DONE** |
| `get-user-policy` | `--user-name`, `--policy-name` | — | **DONE** |
| `delete-user-policy` | `--user-name`, `--policy-name` | — | **DONE** |
| `list-user-policies` | `--user-name` | `--max-items`, `--marker` | **DONE** |
| `tag-user` | `--user-name`, `--tags` | — | **DONE** |
| `untag-user` | `--user-name`, `--tag-keys` | — | **DONE** |
| `list-user-tags` | `--user-name` | `--max-items`, `--marker` | **DONE** |
| `put-user-permissions-boundary` | — | `--user-name`, `--permissions-boundary` | **NOT STARTED** |
| `delete-user-permissions-boundary` | — | `--user-name` | **NOT STARTED** |
| `create-login-profile` | — | `--user-name`, `--password` | **NOT STARTED** |
| `get-login-profile` | — | `--user-name` | **NOT STARTED** |
| `update-login-profile` | — | `--user-name`, `--password` | **NOT STARTED** |
| `delete-login-profile` | — | `--user-name` | **NOT STARTED** |
| `change-password` | — | `--old-password`, `--new-password` | **NOT STARTED** |

### IAM — Access Keys

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-access-key` | `--user-name` | — | **DONE** |
| `list-access-keys` | `--user-name` | `--max-items`, `--marker` | **DONE** |
| `delete-access-key` | `--access-key-id`, `--user-name` | — | **DONE** |
| `update-access-key` | `--access-key-id`, `--user-name`, `--status` (Active/Inactive) | — | **DONE** |
| `get-access-key-last-used` | — | `--access-key-id` | **NOT STARTED** |

### IAM — Policies

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-policy` | `--policy-name`, `--policy-document`, `--path`, `--description` | `--tags` | **DONE** |
| `get-policy` | `--policy-arn` | — | **DONE** |
| `get-policy-version` | `--policy-arn`, `--version-id` | — | **DONE** |
| `list-policy-versions` | `--policy-arn` | `--max-items`, `--marker` | **DONE** |
| `list-policies` | — | `--scope`, `--only-attached`, `--path-prefix`, `--max-items`, `--marker` | **DONE** |
| `delete-policy` | `--policy-arn` | — | **DONE** |
| `attach-user-policy` | `--user-name`, `--policy-arn` | — | **DONE** |
| `detach-user-policy` | `--user-name`, `--policy-arn` | — | **DONE** |
| `list-attached-user-policies` | `--user-name` | `--path-prefix`, `--max-items`, `--marker` | **DONE** |
| `create-policy-version` | — | `--policy-arn`, `--policy-document`, `--set-as-default` | **NOT STARTED** |
| `delete-policy-version` | — | `--policy-arn`, `--version-id` | **NOT STARTED** |
| `set-default-policy-version` | — | `--policy-arn`, `--version-id` | **NOT STARTED** |
| `list-entities-for-policy` | — | `--policy-arn`, `--entity-filter`, `--path-prefix`, `--policy-usage-filter` | **NOT STARTED** |
| `tag-policy` | `--policy-arn`, `--tags` | — | **DONE** |
| `untag-policy` | `--policy-arn`, `--tag-keys` | — | **DONE** |
| `list-policy-tags` | `--policy-arn` | `--max-items`, `--marker` | **DONE** |
| `generate-service-last-accessed-details` | — | `--arn`, `--granularity` | **NOT STARTED** |
| `get-service-last-accessed-details` | — | `--job-id` | **NOT STARTED** |
| `get-service-last-accessed-details-with-entities` | — | `--job-id`, `--service-namespace` | **NOT STARTED** |
| `list-policies-granting-service-access` | — | `--arn`, `--service-namespaces` | **NOT STARTED** |

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
| `list-role-policies` | `--role-name` | `--max-items`, `--marker` | **DONE** |
| `put-role-policy` | `--role-name`, `--policy-name`, `--policy-document` | — | **DONE** |
| `get-role-policy` | `--role-name`, `--policy-name` | — | **DONE** (document returned as raw JSON, not URL-encoded) |
| `delete-role-policy` | `--role-name`, `--policy-name` | — | **DONE** |
| `put-role-permissions-boundary` | — | `--role-name`, `--permissions-boundary` | **NOT STARTED** |
| `delete-role-permissions-boundary` | — | `--role-name` | **NOT STARTED** |
| `tag-role` | `--role-name`, `--tags` | — | **DONE** |
| `untag-role` | `--role-name`, `--tag-keys` | — | **DONE** |
| `list-role-tags` | `--role-name` | `--max-items`, `--marker` | **DONE** |
| `update-role-description` | — | `--role-name`, `--description` | **NOT STARTED** |

### IAM — Instance Profiles

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-instance-profile` | `--instance-profile-name`, `--path`, `--tags` | — | **DONE** |
| `get-instance-profile` | `--instance-profile-name` | — | **DONE** |
| `list-instance-profiles` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `list-instance-profiles-for-role` | `--role-name` | `--max-items`, `--marker` | **DONE** |
| `delete-instance-profile` | `--instance-profile-name` | — | **DONE** |
| `add-role-to-instance-profile` | `--instance-profile-name`, `--role-name` | — | **DONE** |
| `remove-role-from-instance-profile` | `--instance-profile-name`, `--role-name` | — | **DONE** |
| `tag-instance-profile` | `--instance-profile-name`, `--tags` | — | **DONE** |
| `untag-instance-profile` | `--instance-profile-name`, `--tag-keys` | — | **DONE** |
| `list-instance-profile-tags` | `--instance-profile-name` | `--max-items`, `--marker` | **DONE** |

### IAM — OIDC Providers

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-open-id-connect-provider` | `--url`, `--client-id-list`, `--thumbprint-list`, `--tags` | — | **DONE** |
| `get-open-id-connect-provider` | `--open-id-connect-provider-arn` | — | **DONE** |
| `list-open-id-connect-providers` | — | — | **DONE** |
| `delete-open-id-connect-provider` | `--open-id-connect-provider-arn` | — | **DONE** |
| `add-client-id-to-open-id-connect-provider` | — | `--open-id-connect-provider-arn`, `--client-id` | **NOT STARTED** |
| `remove-client-id-from-open-id-connect-provider` | — | `--open-id-connect-provider-arn`, `--client-id` | **NOT STARTED** |
| `update-open-id-connect-provider-thumbprint` | — | `--open-id-connect-provider-arn`, `--thumbprint-list` | **NOT STARTED** |
| `tag-open-id-connect-provider` / `untag-open-id-connect-provider` | `--open-id-connect-provider-arn`, `--tags`/`--tag-keys` | — | **DONE** |
| `list-open-id-connect-provider-tags` | `--open-id-connect-provider-arn` | `--max-items`, `--marker` | **DONE** |

### IAM — Groups

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-group` | `--group-name`, `--path` | — | **DONE** |
| `get-group` | `--group-name` | — | **DONE** |
| `list-groups` | `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `delete-group` | `--group-name` | — | **DONE** |
| `update-group` | — | `--group-name`, `--new-path`, `--new-group-name` | **NOT STARTED** |
| `add-user-to-group` | `--group-name`, `--user-name` | — | **DONE** |
| `remove-user-from-group` | `--group-name`, `--user-name` | — | **DONE** |
| `list-groups-for-user` | `--user-name` | `--max-items`, `--marker` | **DONE** |
| `attach-group-policy` | `--group-name`, `--policy-arn` | — | **DONE** |
| `detach-group-policy` | `--group-name`, `--policy-arn` | — | **DONE** |
| `list-attached-group-policies` | `--group-name`, `--path-prefix` | `--max-items`, `--marker` | **DONE** |
| `put-group-policy` | `--group-name`, `--policy-name`, `--policy-document` | — | **DONE** |
| `get-group-policy` | `--group-name`, `--policy-name` | — | **DONE** |
| `delete-group-policy` | `--group-name`, `--policy-name` | — | **DONE** |
| `list-group-policies` | `--group-name` | `--max-items`, `--marker` | **DONE** |

### IAM — Account

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `get-account-summary` | — | — | **DONE** |

---

## STS

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|--------------------------------------|--------|
| `get-caller-identity` | — | — | **DONE** |
| `assume-role` | `--role-arn`, `--role-session-name`, `--duration-seconds` (900–min(role MaxSessionDuration, 43200)) | `--policy`, `--policy-arns` (→ `PackedPolicyTooLarge`); `--tags`, `--transitive-tag-keys` (→ `InvalidParameterValue`); `--serial-number`, `--token-code` (→ `InvalidParameterValue`); `--external-id`, `--source-identity` (accepted and logged, **not enforced** — no Condition evaluator in v1) | **DONE** |
| `get-session-token` | `--duration-seconds` (900–129600, default 43200 = 12h; clamped, not rejected) | `--serial-number`, `--token-code` (MFA → `InvalidParameterValue`) | **DONE** |
| `assume-role-with-web-identity` | `--role-arn`, `--role-session-name`, `--web-identity-token`, `--duration-seconds` (900–43200, default 3600) | `--provider-id`; `--policy`, `--policy-arns` (→ `PackedPolicyTooLarge`) | **DONE** |
| `assume-role-with-saml` | — | `--role-arn`, `--principal-arn`, `--saml-assertion`, `--policy`, `--policy-arns`, `--duration-seconds` | **NOT STARTED** |
| `get-access-key-info` | — | `--access-key-id` | **NOT STARTED** |
| `get-federation-token` | — | `--name`, `--policy`, `--policy-arns`, `--duration-seconds`, `--tags` | **NOT STARTED** |
| `decode-authorization-message` | — | `--encoded-message` | **NOT STARTED** |

Trust policies (`AssumeRolePolicyDocument`) reject `NotPrincipal`, `NotAction`, empty-string `Action` elements, and empty `Principal` blocks at write time (`MalformedPolicyDocument`). `Condition` blocks are rejected except on `sts:AssumeRoleWithWebIdentity` with `StringEquals` (IRSA), which v1 evaluates at assume time (`{iss}:sub`, `{iss}:aud`); anything wider is rejected to avoid silent over-grant.

---

## IMDS (Instance Metadata Service)

Available at `169.254.169.254` from inside every running guest VM, matching AWS. The endpoint is reached from within a guest over plain HTTP, exactly as on EC2, with no in-VM agent to install. DHCP and fully static guests reach it identically, with no in-guest route configuration.

**IMDSv2-only.** Every read requires a session token. A tokenless (v1-style) `GET` returns `401 Unauthorized` with an empty body. Obtain a token with a `PUT /latest/api/token` carrying `X-aws-ec2-metadata-token-ttl-seconds` (1–21600), then send it back in `X-aws-ec2-metadata-token` on every read.

The EC2 control plane reports this posture faithfully: `describe-instances` returns `MetadataOptions.HttpTokens=required`, and `run-instances`/`modify-instance-metadata-options` reject `--http-tokens optional` (and `--http-endpoint disabled`) with `UnsupportedOperation` — exactly as AWS does under account-level IMDSv2 enforcement.

```bash
# Inside the guest VM:
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/instance-id

# Tokenless GET → 401 Unauthorized (empty body)
curl -i http://169.254.169.254/latest/meta-data/instance-id
```

### IMDS — Supported Paths

| Path | Method | Source | Status |
|------|--------|--------|--------|
| `/latest/api/token` | PUT | Issues an ENI-bound IMDSv2 token; `X-aws-ec2-metadata-token-ttl-seconds` ∈ [1, 21600] required | **DONE** |
| `/` | GET | Supported API-version list (`2021-07-15`, `latest`); token-gated | **DONE** |
| `/latest` | GET | Top-level tree listing (`dynamic`, `meta-data`, `user-data`) | **DONE** |
| `/<date>/...` | GET/PUT | Any dated API version aliases to `/latest` (cloud-init parity) | **DONE** |
| `/latest/meta-data/` | GET | Directory listing of supported children | **DONE** |
| `/latest/meta-data/instance-id` | GET | `vm.ID` | **DONE** |
| `/latest/meta-data/instance-type` | GET | `vm.InstanceType` | **DONE** |
| `/latest/meta-data/ami-id` | GET | launch `ImageId` | **DONE** |
| `/latest/meta-data/ami-launch-index` | GET | Per-instance launch index (`0..n-1`), contiguous on partial failure | **DONE** |
| `/latest/meta-data/reservation-id` | GET | `DescribeInstances` `Reservation.ReservationId` | **DONE** |
| `/latest/meta-data/instance-life-cycle` | GET | `spot` for a spot-launched instance, else `on-demand`; defaults to `on-demand` on a resolution miss (never 404, the leaf is always advertised) | **DONE** |
| `/latest/meta-data/local-ipv4` | GET | `ENIRecord.PrivateIpAddress` (== request source IP) | **DONE** |
| `/latest/meta-data/public-ipv4` | GET | EIP, else instance public IP; empty body if none | **DONE** |
| `/latest/meta-data/public-hostname` | GET | Mirrors `public-ipv4`; 404 when no public IP | **DONE** |
| `/latest/meta-data/mac` | GET | `ENIRecord.MacAddress` | **DONE** |
| `/latest/meta-data/security-groups` | GET | `ENIRecord.SecurityGroupIds`, newline-separated | **DONE** |
| `/latest/meta-data/hostname`, `/local-hostname` | GET | Synthesised `ip-<dashed-ip>.<region>.compute.internal` | **DONE** |
| `/latest/meta-data/placement/availability-zone` | GET | `ENIRecord.AvailabilityZone` | **DONE** |
| `/latest/meta-data/placement/region` | GET | Derived from AZ (trailing letter stripped) | **DONE** |
| `/latest/meta-data/services/{domain,partition}` | GET | Static: `amazonaws.com` / `aws` | **DONE** |
| `/latest/meta-data/iam/info` | GET | `{InstanceProfileArn, InstanceProfileId}`; 404 if no profile | **DONE** |
| `/latest/meta-data/iam/security-credentials/` | GET | Role name(s) under the profile, one per line; empty body if none | **DONE** |
| `/latest/meta-data/iam/security-credentials/<role>` | GET | STS `AssumeRoleForInstance` → ASIA-prefixed temporary credential JSON | **DONE** |
| `/latest/meta-data/public-keys/` | GET | `0=<keyName>` from the launch key pair; 404 if none | **DONE** |
| `/latest/meta-data/public-keys/0/` | GET | `openssh-key` (format list for index 0) | **DONE** |
| `/latest/meta-data/public-keys/0/openssh-key` | GET | Launch SSH public key, live-fetched from the key store; 404 if the key was deleted, 500 on backend fault | **DONE** |
| `/latest/user-data` | GET | `vm.UserData`; 404 if none | **DONE** |
| `/latest/dynamic` | GET | Lists `instance-identity/` | **DONE** |
| `/latest/dynamic/instance-identity` | GET | Lists `document` (signed forms listed when the signing key lands) | **DONE** |
| `/latest/dynamic/instance-identity/document` | GET | Unsigned identity document from resolved ENI + instance facts | **DONE** |
| `/latest/dynamic/instance-identity/{signature,pkcs7,rsa2048}` | GET | Signed forms; need a per-cluster signing key | **NOT STARTED** (404; lands with EKS IRSA) |
| `/latest/meta-data/network/interfaces/macs/<mac>/...` | GET | Primary ENI subtree: `mac`, `device-number`, `interface-id`, `owner-id`, `subnet-id`, `vpc-id`, `local-ipv4s`, `local-hostname`, `security-group-ids`, `security-groups`, `subnet-ipv4-cidr-block`, `vpc-ipv4-cidr-block(s)`, and `public-ipv4s`/`public-hostname` when an EIP is attached | **DONE** (single-NIC; multi-ENI deferred) |
| `/latest/meta-data/tags/instance/<key>` | GET | Instance-tag metadata; gated on `InstanceMetadataTags` enablement | **NOT STARTED** (404) |
| `/latest/meta-data/block-device-mapping/...` | GET | `ami`/`root`/`ebsN`/`ephemeralN` device map | **NOT STARTED** (404) |
| `/latest/meta-data/placement/{group-name,partition-number,availability-zone-id,host-id}` | GET | Placement extras beyond `availability-zone`/`region` | **NOT STARTED** (404) |
| `/latest/meta-data/instance-action` | GET | `none` unless interruptible instances ship | **NOT STARTED** (404) |
| `/latest/meta-data/spot/{instance-action,termination-time}` | GET | 404 is the faithful steady state for a never-interrupted spot instance ("no action scheduled"); a 200 body would trigger interruption handling in pollers (AWS Node Termination Handler / Karpenter). Not advertised in the `spot/` listing | **DONE** (404 by contract) |

---

## ELBv2 (Application & Network Load Balancer)

The data plane uses a system-managed LB VM, launched automatically during `create-load-balancer`. Application Load Balancers run **HAProxy** (L7: rules, fixed-response, redirect, HTTP/HTTPS). Network Load Balancers run **nginx `stream`** (L4: TCP, UDP, TLS, TCP_UDP) — HAProxy cannot load-balance UDP. The agent selects the engine from the `Engine` field on the config-delivery response.

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
| `create-listener` | `--load-balancer-arn`, `--default-actions` (Type=forward, TargetGroupArn), `--protocol` (HTTP/HTTPS/TLS), `--port`, `--certificates` (HTTPS/TLS), `--ssl-policy` | `--alpn-policy`, `--mutual-authentication`, `--dry-run` | **DONE** |
| `modify-listener` | `--listener-arn`, `--protocol`, `--port`, `--default-actions`, `--certificates`, `--ssl-policy` | `--alpn-policy`, `--mutual-authentication`, `--dry-run` | **DONE** |
| `delete-listener` | `--listener-arn` | `--dry-run` | **DONE** |
| `describe-listeners` | `--load-balancer-arn`, `--listener-arns` | `--page-size`, `--marker`, `--dry-run` | **DONE** |

### ELBv2 — Listener Rules

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `create-rule` | `--listener-arn`, `--priority` (1–50000), `--conditions` (host-header, path-pattern, http-header, http-request-method, query-string, source-ip), `--actions` (forward, redirect, fixed-response), `--tags` | `--dry-run` | **DONE** |
| `modify-rule` | `--rule-arn`, `--conditions`, `--actions` | `--dry-run` | **DONE** |
| `delete-rule` | `--rule-arn` | `--dry-run` | **DONE** |
| `describe-rules` | `--listener-arn`, `--rule-arns` | `--page-size`, `--marker` (parsed, not enforced) | **DONE** |
| `set-rule-priorities` | `--rule-priorities` (RuleArn, Priority) | — | **DONE** |

A synthetic `default` rule is derived from the listener's `DefaultActions`.

### ELBv2 — Listener Certificates & SSL Policies

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `add-listener-certificates` | `--listener-arn`, `--certificates` | `--dry-run` | **DONE** |
| `remove-listener-certificates` | `--listener-arn`, `--certificates` | `--dry-run` | **DONE** |
| `describe-listener-certificates` | `--listener-arn` | `--page-size`, `--marker` | **DONE** |
| `describe-ssl-policies` | `--names` | `--load-balancer-type`, `--page-size`, `--marker` | **DONE** (static catalog: `ELBSecurityPolicy-FS-1-2-Res-2019-08`, `ELBSecurityPolicy-TLS13-1-2-2021-06` — metadata only, no in-platform TLS termination) |

The default certificate cannot be added/removed via these calls — set it on the listener.

### ELBv2 — Attributes & Modify

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-load-balancer-attributes` | `--load-balancer-arn` | — | **DONE** (stored values over per-type defaults) |
| `modify-load-balancer-attributes` | `--load-balancer-arn`, `--attributes` | — | **DONE** (unknown keys → `ValidationError`) |
| `describe-target-group-attributes` | `--target-group-arn` | — | **DONE** |
| `modify-target-group-attributes` | `--target-group-arn`, `--attributes` (incl. `deregistration_delay.timeout_seconds`, `stickiness.*`) | — | **DONE** |
| `modify-target-group` | `--target-group-arn`, `--health-check-*`, `--matcher` | `--target-type`/`--protocol`/`--vpc-id` (immutable) | **DONE** |
| `describe-listener-attributes` | `--listener-arn` | — | **DONE** (stub — returns empty; not persisted) |
| `modify-listener-attributes` | `--listener-arn`, `--attributes` | — | **DONE** (stub — echoes input; not persisted) |

### ELBv2 — Network & Security

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `set-security-groups` | `--load-balancer-arn`, `--security-groups` | `--enforce-security-group-inbound-rules-on-private-link-traffic` | **DONE** (ALB only — NLB → `InvalidConfigurationRequest`) |
| `set-subnets` | `--load-balancer-arn`, `--subnets`, `--subnet-mappings` | `--ip-address-type` | **DONE** (live ENI add/remove with rollback) |
| `set-ip-address-type` | `--load-balancer-arn`, `--ip-address-type` (ipv4) | dualstack/IPv6 (rejected) | **DONE** |

### ELBv2 — Tags

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `describe-tags` | `--resource-arns` (loadbalancer, targetgroup, listener) | — | **DONE** |

### ELBv2 — Not Yet Implemented

| Feature | Priority | Status |
|---------|----------|--------|
| In-platform HTTPS/TLS termination (cert + SSL-policy APIs exist; data-plane TLS not terminated) | High | **NOT STARTED** |
| ALPN policy, mutual TLS (mTLS) | Medium | **NOT STARTED** |
| Listener attribute persistence (`Describe/ModifyListenerAttributes` are stubs) | Medium | **NOT STARTED** |
| Active health checking (API-driven, vs. HAProxy/nginx-only today) | Medium | **NOT STARTED** |
| IP and Lambda target types | Low | **NOT STARTED** |
| S3 access log delivery | Low | **NOT STARTED** |
| WAF integration | Low | **NOT STARTED** |

---

## ACM (AWS Certificate Manager)

Import-only — Spinifex stores externally-issued certificates for ELBv2 listener references; it does not issue certificates or validate domains (`RequestCertificate`). Certs are account-scoped; `describe`/`delete` enforce ownership.

| Command | Implemented Flags | Missing Flags | Status |
|---------|-------------------|---------------|--------|
| `import-certificate` | `--certificate`, `--private-key`, `--certificate-chain`, `--certificate-arn` (re-import) | `--tags` | **DONE** |
| `describe-certificate` | `--certificate-arn` | — | **DONE** |
| `list-certificates` | — | `--certificate-statuses`, `--includes`, `--max-items`, `--next-token` | **DONE** |
| `delete-certificate` | `--certificate-arn` | — | **DONE** |
| `request-certificate` | — | `--domain-name`, `--validation-method`, `--subject-alternative-names`, `--tags` | **NOT STARTED** |
| `add-tags-to-certificate` / `list-tags-for-certificate` / `remove-tags-from-certificate` | — | `--certificate-arn`, `--tags`/`--tag-keys` | **NOT STARTED** |
| `export-certificate` | — | `--certificate-arn`, `--passphrase` | **NOT STARTED** |

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
