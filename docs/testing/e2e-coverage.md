# What each E2E suite proves

**This index exists to answer "does anything already test that?" before someone reasons it out from the code.** Comments describe intent when written and can go stale; these suites run against a live cluster and are the current answer on behaviour.

Suites live in `tests/e2e/<name>/` and build to `tests/e2e/_bin/<name>.test`. Every file carries `//go:build e2e`, so a default build skips them. The sets that decide what runs where are in `.github/scripts/suite-sets.sh`, and the nightly permutation matrix is `.github/e2e-cells.json`.

| Set | Suites |
| --- | --- |
| `E2E_SUITES_SINGLE` | `single iam cert eks ecs storagegrowth partialblock rds quota storagefault` |
| `E2E_SUITES_MULTI` | `multinode lb cert quota storagefault` |
| `E2E_SUITES_NIGHTLY_SINGLE` | `single cert iam` |
| `E2E_SUITES_NIGHTLY_MULTI` | `multinode cert lb` |

The nightly permutation sets are deliberately narrower than the full ones: those cells have a ~35 minute budget and exist to prove every install / network / host-OS combination boots and serves. `eks`, `ecs`, `rds` and `storagefault` each get a dedicated cell instead.

## The suites

### `single` — the broad single-node EC2/EBS/VPC surface

The largest suite, 25 top-level tests. Notable entry points:

- **`TestVPCEgressPaths`** — **the authority on subnet egress semantics.** One VPC with a public and a private subnet, sharing two guests across four stages: `PublicSubnetEgress` (IGW egress works), `RouteBeforeSubnet` (a regression guard for an IGW route installed on the main route table before the subnet exists), the NAT Gateway rounds, and `EIPFlip` (associating an Elastic IP onto a running instance).
  - **`NATBaseline` hard-fails if a private-subnet guest reaches the internet with no NAT Gateway.** `NATGatewayUp` then proves egress appears and `NATGatewayDown` that it disappears. `SPINIFEX_VPCEGRESS_NAT_ROUNDS` repeats the up/down cycle for a soak run.
  - **This is AWS parity for private subnets, and it is proven in pool mode only** — the nat-mode cell runs `natuplink`, which never creates a private subnet.
- `TestSGPolicyDatapath`, `TestSGReachabilityPolicy` — security groups as an actual datapath, not just API state.
- `TestWANEgressSMTPBlock` — egress port blocking.
- `TestInstanceMetadata` — IMDS from inside a guest.
- `TestStopStart`, `TestEarlyRebootLiveness`, `TestGuestChurnDurability`, `TestSnapshotLifecycle`, `TestSnapshotBackedLaunch`, `TestCreateImage`, `TestVolumeLifecycle`, `TestSpotInstanceLifecycle`, `TestLaunchTemplateBoot`.
- `TestNegativeErrorPaths` — the `8a`–`8h` AWS error-shape cases (malformed AMI ID, invalid instance type, volume in use, detach-root forbidden, and so on).

### `natuplink` — routed NAT (`external_mode = "nat"`), single node only

Runs **on** the node, shelling out to `ip`/`iptables`/`ovn-nbctl` locally. Skips unless `spinifex.toml` says `external_mode = "nat"` and `env.Mode == ModeSingle`. Nine sequential phases: host wiring (transit veth, `ip_forward`, NAT egress rules), config, the EIP surface (enabled or disabled depending on whether a public pool exists), default-subnet public IP mapping, OVN gateway and SNAT on the transit net, instance egress **proven via the serial console**, host-to-guest Tier 1 ingress, a second VPC getting a unique transit gateway IP, and the EIP ingress lifecycle.

**Its package header says nat mode has "no inbound path to VMs". That is stale** — phases 3 and 9 exercise EIP ingress whenever a public pool is configured, and the nightly cell configures one.

### `multinode` — behaviour that only exists on more than one node

`VPCSetup`, `SpansMultipleNodes`, `SpreadPlacement`, `EveryRunningInstanceReported`, `BastionSSH`, and its own NAT Gateway lane: `PreNATIsolation`, `NATGatewayInternet`, `NATCleanupOrdering`.

### `iam` — everything IAM and STS

Policy evaluation against live requests rather than unit-level: a deny applying to the next request without a new credential, resource-scoped grants naming one key pair, `aws:SourceIp` seeing the real client address, assumed-role sessions carrying role policies, and revocation stopping one key alone.

### `cert` — the cluster CA and TLS identity

CA installed and downloadable, fingerprint matches config, hostnames present in DNS SANs, OpenSSL verification, `curl` without a CA flag, and a shared issuer across nodes.

### `lb` — load balancers (multi-node)

Internal and internet-facing ALB and NLB, HTTPS listeners, listener rules, `ModifyListener`, and NLB UDP.

### `eks`, `ecs`, `ecr` — container services

`eks`: cluster create/delete, access entries, addon delivery, `GetToken`, kubeconfig artifacts, the EBS CSI volume path, and an IRSA pod wired with a projected SA token. `ecs`: clusters, container instances, the three network modes (`awsvpc`, `bridge`, `host`), task-role credentials fetched from the container credentials endpoint, a service behind an ELB, and ENI release on terminate. `ecr`: control-plane CRUD plus a real OCI data plane — docker login/build/push/pull round trips, multi-layer images, and raw blob HTTP.

### `rds` — managed databases

The create/describe/connect path against a live cluster: a DB VM that actually boots, TLS enforced with plaintext refused, clients connecting from the same and another subnet, class changes replacing the VM while keeping overrides, deferred parameter-group attachment windows, and delete-while-creating leaving nothing behind.

### Storage and durability

- **`storagefault`** — what a guest does when its storage backend stops answering, and what stop/start does when the seal failed. **The claim is about corruption, not availability.** SIGSTOPs a cluster-wide service, so it always runs last and in its own cell. Three-node, because the RS(2,1) shard floor and cross-node takeover after a failed seal do not exist on one node.
- **`partialblock`** — concurrent sub-block writes to the same block surviving a cold reread through NBD from a real guest.
- **`storagegrowth`** — volume growth and image pull. Requires `external_mode=pool`; skips on a nat env, which has no EIP and therefore no guest SSH.
- **`reboot`** — a slow guest waited for and keeping what it wrote, and a wedged guest reset once its budget expires. Proves a real reboot by requiring a *new* boot ID.
- **`diskperf`** — guest disk performance gate, pinned fio profile on a dedicated blank volume. Also requires `external_mode=pool` for guest SSH.

### Other

- **`quota`** — every enforced dimension refuses with the same `quotaExceeded` code; a launch is charged for every instance it asks for; one instance wider than the cap is refused.
- **`gpu`** — device visible in the guest, driver access, launch.
- **`bedrock`** — `ListFoundationModels`/`GetFoundationModel` against a running cluster. Wire-format parity is unit-tested, not here.
- **`ddil`** — Denied, Disrupted, Intermittent, Limited network scenarios.
- **`teardown`** — sweeps leftovers.
- **`harness`** — shared primitives, not a suite. `harness/vpc_egress.go` carries the VPC/subnet/IGW/NAT-gateway fixtures the egress scenarios build on.
- **`manifest-check`, `manifest-lint`** — manifest validation, not cluster behaviour.

## Keeping this honest

Add a row when a suite is added, and correct one when a suite's claim changes. An index that has drifted is worse than none, because it will be trusted. If a suite's package comment and this file disagree, check the test bodies — both are prose and either can be stale.
