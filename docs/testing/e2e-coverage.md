# What each E2E suite proves

**This index exists to answer "does anything already test that?" before someone reasons it out from the code.** Comments describe intent when written and can go stale; these suites run against a live cluster and are the current answer on behaviour.

Suites live in `tests/e2e/<name>/` and build to `tests/e2e/_bin/<name>.test`. Every file carries `//go:build e2e`, so a default build skips them. The sets that decide what runs where are in `.github/scripts/suite-sets.sh`, and the nightly permutation matrix is `.github/e2e-cells.json`.

| Set | Suites |
| --- | --- |
| `E2E_SUITES_SINGLE` | `single iam cert eks ecs storagegrowth partialblock rds quota storagefault` |
| `E2E_SUITES_MULTI` | `multinode lb cert quota kvquorum lbrecovery instancerecovery storagefault` |
| `E2E_SUITES_NIGHTLY_SINGLE` | `single cert iam` |
| `E2E_SUITES_NIGHTLY_MULTI` | `multinode cert lb` |

The nightly permutation sets are deliberately narrower than the full ones: those cells have a ~35 minute budget and exist to prove every install / network / host-OS combination boots and serves. `eks`, `ecs`, `rds`, `storagefault`, `instancerecovery`, `lbrecovery` and `kvquorum` each get a dedicated cell instead.

**`multinode` carries the KV replication sweep as well**, which is why the thing `kvquorum` proves is still checked in every multi-node permutation cell even though the suite itself is not. The read-only half costs a NATS connection and a stream listing; only the node-loss half needs a cell of its own.

## The suites

### `single` — the broad single-node EC2/EBS/VPC surface

The largest suite, 25 top-level tests. Notable entry points:

- **`TestVPCEgressPaths`** — **the authority on subnet egress semantics.** One VPC with a public and a private subnet, sharing two guests across four stages: `PublicSubnetEgress` (IGW egress works), `RouteBeforeSubnet` (a regression guard for an IGW route installed on the main route table before the subnet exists), the NAT Gateway rounds, and `EIPFlip` (associating an Elastic IP onto a running instance).
  - **`NATBaseline` hard-fails if a private-subnet guest reaches the internet with no NAT Gateway.** `NATGatewayUp` then proves egress appears and `NATGatewayDown` that it disappears. `SPINIFEX_VPCEGRESS_NAT_ROUNDS` repeats the up/down cycle for a soak run.
  - **This is AWS parity for private subnets.** It ran in pool mode only until 2026-09-26: the fixture gated it on `external_mode = "pool"`, on the stale premise that nat clusters have no EIPs. They do when a public pool is configured, so the gate is now "can this cluster allocate public addresses" and the nat-single cell runs this test beside `natuplink`, which never creates a private subnet.
- `TestSGPolicyDatapath`, `TestSGReachabilityPolicy` — security groups as an actual datapath, not just API state.
- `TestWANEgressSMTPBlock` — egress port blocking.
- `TestInstanceMetadata` — IMDS from inside a guest.
- `TestStopStart`, `TestEarlyRebootLiveness`, `TestGuestChurnDurability`, `TestSnapshotLifecycle`, `TestSnapshotBackedLaunch`, `TestCreateImage`, `TestVolumeLifecycle`, `TestSpotInstanceLifecycle`, `TestLaunchTemplateBoot`.
- `TestNegativeErrorPaths` — the `8a`–`8h` AWS error-shape cases (malformed AMI ID, invalid instance type, volume in use, detach-root forbidden, and so on).

### `natuplink` — routed NAT (`external_mode = "nat"`), one node or three

Runs **on** a node, shelling out to `ip`/`iptables`/`ovn-nbctl` locally. Skips unless `spinifex.toml` says `external_mode = "nat"`. Nine sequential phases: host wiring (transit veth, `ip_forward`, NAT egress rules), config, the EIP surface (enabled or disabled depending on whether a public pool exists), default-subnet public IP mapping, OVN gateway and SNAT on the transit net, instance egress **proven via the serial console**, host-to-guest Tier 1 ingress, a second VPC getting a unique transit gateway IP, and the EIP ingress lifecycle.

**Phase 9 has two shapes.** On one node it is the EIP lifecycle: allocate, associate, host delivery plumbing, a TCP handshake to the address, a vpcd restart that must replay the plumbing, and disassociate tearing it down. On a cluster it is the distributed lane instead — a guest on every node, each public IP plumbed by the node running its guest and by **no other**, every transit veth carrying the one cluster-wide MAC, each `dnat_and_snat` row carrying `external_mac` and `logical_port`, inbound proven by a handshake and outbound by each guest's own console. **That lane is the regression guard for the two defects that made routed NAT work on one node only**, and neither is visible to a single-node run.

Phases 7 and 8 are gated on a cluster by what the datapath can actually do: the transit segment is a localnet, so host-to-guest Tier 1 ingress only exists where the VPC's gateway router port and the guest are both on this host. The VPC ingress route itself is installed on every node, so phase 8 checks it unconditionally.

Two nightly cells run it: `nat-single` (cell 19) and `nat-multi` (cell 30). Cell 20 is the bare-metal job and is not in the permutation matrix at all, which is why the multi-node NAT cell is numbered past the end rather than filling a gap.

### `multinode` — behaviour that only exists on more than one node

`VPCSetup`, `SpansMultipleNodes`, `SpreadPlacement`, `EveryRunningInstanceReported`, `BastionSSH`, and its own NAT Gateway lane: `PreNATIsolation`, `NATGatewayInternet`, `NATCleanupOrdering`.

**`TestMultinodeJetStreamReplicas` is a whole-cluster sweep, not a named list, and it lives here on purpose.** It audits every KV bucket the cluster has and fails any that is replicated across fewer nodes than the cluster has, capped at JetStream's ceiling of five. Naming buckets would only ever cover the ones somebody remembered — a replica count is set where a bucket is created, and there is a bucket-creating call in most services — so the sweep is the assertion and the three daemon-owned buckets are named only so that a run enumerating nothing still fails. Because `multinode` is in `E2E_SUITES_NIGHTLY_MULTI`, this runs in every multi-node permutation cell, and no cell can be added that quietly opts out of it.

### `kvquorum` — the cluster's own state survives losing a node (multi-node, needs three)

**The control plane keeps its state in JetStream KV buckets, and this is the suite that proves those buckets are actually replicated.** A bucket on one replica lives on one JetStream-chosen server recorded in no config, so losing that node makes it return `nats: no responders available for request` from *every* node at once — a cluster-wide outage caused by a single-node event. Leader leases are the worst case: acquiring one is a write, so a lease bucket without a quorum cannot be acquired by anyone, and every reconciler sharing it stops cluster-wide at exactly the moment a node has failed and there is repair work to do.

Three entry points, in the order they run:

- **`TestKVBucketsAreReplicatedAcrossTheCluster`** is read-only and runs first, so a cluster that is already wrong is reported as wrong rather than as a failure to survive a node loss. It sweeps every bucket, then separately requires the leader-lease buckets to be among those checked — they were the worst case of the defect, and a regression reaching only them would otherwise be one line in a long list rather than the headline.
- **`TestKVReplicasCommandReportsHealthy`** runs `spx admin kv replicas --json` on **every** node, not one. The command reads cluster-global Raft metadata, so all nodes must give the same answer; a node that disagrees is reporting on a cluster it is not fully part of, which is worth knowing before a deployment is gated on its exit status.
- **`TestKVBucketsSurviveANodeLoss`** produces the fault rather than inspecting for it. It takes away **the node leading the most buckets** — the worst one to lose, and the one a test picking arbitrarily would usually miss — then requires every survivor to still answer for every bucket, brings the node back, and requires it to carry its share again.

**Configured replicas are checked against placed replicas, which are different facts.** A stream can carry the right replica count while its Raft group is still short of peers, and that reads as healthy right up until the node it is really on goes away. The last assertion is that no bucket came back with *fewer* replicas than it had: a cluster that "repaired" itself by lowering a count would pass every other assertion here and have quietly traded the durability this suite is about for a green run.

**Its own suite and its own cell, for lbrecovery's reasons.** It takes a node away, so anything sharing the environment reports that outage as its own failure, and the question it answers — "is our internal state actually replicated" — has to read as itself rather than as one more red subtest somewhere else. Three nodes is the floor: on two, a majority is two, so losing either leaves no quorum for any stream and there is nothing to assert beyond "Raft needs a majority", which is true of NATS and not a property of ours.

### `instancerecovery` — a guest whose node goes away (multi-node)

**Asserted from both sides, and the pair is the point.** `InstanceAutoRecovery` takes a node down and requires the guest to come back on exactly one survivor, identified by the qemu process rather than by what the API says, then brings the node back and requires it to drop the local copy instead of relaunching it. `InstanceRecoveryRefusesStorageFault` freezes predastore cluster-wide first, so the guest is paused by `werror=stop` when its node goes away, and requires that **nothing moves** — a store that refuses every node cannot be fixed by moving a guest, and the relaunch would cost it the request QEMU is holding. A reconciler that cannot tell a host failure from a storage failure passes the first and fails the second. Neither turns recovery on, because there is no setting for it: both wait for every node to report the reconciler active on a cluster nothing configured, so a build that reintroduced a switch fails here.

**`InstancePartitionRecovery` is the fault this feature exists for, and the only one of the three that produces it rather than approximating it.** It drops NATS on 4222 and 4248 between one node and its peers and nothing else, so that node keeps its services, keeps its QEMU and keeps answering its own local clients while it cannot reach consensus — half healthy, which is what a real network fault looks like and what a service stop cannot imitate. It then requires that the node gives up the volume on a clock it can evaluate without reaching anything, that a survivor takes the guest over, and that the rejoining node forgets its copy rather than relaunching it. **The assertion it exists for is the one no single sample can reach:** a watch samples every node every three seconds for the whole run and fails on any sample naming two hosts, because two guests writing one volume through one object store is data loss already under way rather than a race that resolves. The evidence for *why* the guest stopped is the surrender and the fence in the node's own journal — a guest killed for any other reason satisfies the process count and means the opposite.

**The partition gets its own nft table and a systemd deadman timer, for reasons worth keeping.** The node's real firewall is `inet spinifex_filter`, so a test that flushed or edited it would leave the node unfirewalled with no sign of which rules it lost; a separate table at priority -300 says exactly "these packets, dropped" and heals in one atomic delete. `systemd-run --on-active` removes it independently, so a cancelled workflow or a panicking test does not leave a node cut off from its cluster until somebody notices. It also proves SSH still works and proves a peer port is actually unreachable before asserting anything — a rule set that installed cleanly and dropped nothing would otherwise read as the system tolerating a partition.

**The storage-fault victim launches with cloud-init user-data that writes continuously, and that is load bearing.** An idle guest issues no I/O that reaches the object store, so a version of this test without a workload left three guests running happily through twelve minutes of dead predastore and proved nothing. Driving the load from user-data rather than over SSH is what lets it run on a routed-NAT cell, where guests have no public address. The pause itself takes minutes — 4m47s when measured — so the budget is generous on purpose.

**Its own suite and its own nightly cell, for storagefault's reasons.** It freezes a cluster-wide service and removes a node, so anything sharing the environment reports that outage as its own failure; and it is slow by nature, which is why it is not in a permutation cell budgeted at thirty-five minutes. Both behaviours were first proven by hand against a real hypervisor power-off on the three-node OCI cluster.

### `lbrecovery` — a load balancer whose node goes away (multi-node, needs three)

**The customer-visible half of host failure, asserted from outside the cluster rather than from the records.** `ALBSurvivesItsHostFailing` stands up an internet-facing ALB with two HTTP backends, discovers which node the load balancer's guest landed on, and takes that node away. It then requires the four things a customer of a load balancer actually holds: the name keeps resolving, the address behind it does not move, requests through the name are answered again within a bounded time, and the backends — which never moved — are healthy again from the recovered load balancer's point of view.

**The backends are placed around the load balancer, not beside it.** A load balancer is launched over a queue group and lands wherever a node answers, so its node is discovered and the backends are then launched and relaunched until both are somewhere else. A backend on the node about to be taken away would be recovered too, and the run could not then tell an ALB that came back serving from one that came back with nothing to serve.

**DNS is queried against a node's own northstar, not through the runner's resolver.** Go's resolver dials port 53 on a named node — which is open on every node deliberately, because northstar serves public names — so the assertion is about what this cluster answers rather than about what the machine running the tests was configured to forward to. The same resolver drives the HTTP probes, so traffic goes through the load balancer's name end to end and not through an address resolved once at the start. `dig` is deliberately not used: it is proven present in the guest image and nowhere else.

**The assertion no single sample can reach is the name.** A watch queries every surviving node every five seconds for the whole run, and fails on any sample where the record is missing, where a node refuses the query, or where it answers an address that is not the load balancer's. An answer naming another address is the worst of the three, because the traffic goes there; an empty answer outlives the fix for as long as anything cached it. An address that moves is asserted separately and before any traffic, since a passing probe against a renumbered address would hide it.

**Then the node comes back, and has to give up what it no longer owns.** The returning node must not relaunch its local copy, and must no longer carry the tap that claims the load balancer's logical port — OVN binds a port to one chassis, so a second claimant does not split the traffic, it contends for it and the address answers from whichever won last. That same stale tap is also what keeps the external address's host ingress from being pruned, since the prune waits on evidence the guest sits elsewhere. The last assertion is that the name still serves traffic with the node back in the cluster, which is the end-user form of the same question.

**`DescribeLoadBalancers` is required to keep reporting `active` throughout, and that is AWS parity rather than an oversight.** AWS never marks a load balancer degraded because a host under it failed: the state describes the customer's configuration, not our hardware. The honest signals about the outage are target health and the traffic itself, which is why both are asserted and the state is not trusted as evidence of anything.

**Its own suite and its own cell, for instancerecovery's reasons plus one.** It takes a node away, so anything sharing the environment reports that outage as its own failure; it needs its own VPC, public subnet and a backend placement no shared fixture describes; and a failure here has to read as "the load balancer did not survive its host" rather than as one more red subtest in a suite about load balancer features.

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
