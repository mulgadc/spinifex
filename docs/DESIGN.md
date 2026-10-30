# Spinifex Architecture

Spinifex is an AWS-compatible infrastructure platform for bare-metal, edge, and on-premise deployments. It provides EC2-compatible VM orchestration backed by QEMU/KVM, with storage provided by companion projects Viperblock (EBS) and Predastore (S3).

## High-Level Architecture

<p align="center">
  <img src="../.github/assets/design-architecture.svg" alt="Spinifex high-level architecture — AWS SDK, gateway, NATS broker, per-node daemons, QEMU/KVM VMs" width="900">
</p>

## Request Flow

### 1. AWS SDK Request

Users interact with Spinifex using standard AWS SDKs or the AWS CLI by using the spinifex profile.

```bash
AWS_PROFILE=spinifex aws ec2 run-instances \
    --image-id ami-debian13 \
    --instance-type t3.micro \
    --key-name my-keypair
```

The AWS SDK formats this as an HTTPS POST with:
- AWS SigV4 authentication headers
- EC2 Query Protocol body (`Action=RunInstances&ImageId=ami-debian13&...`)

### 2. AWS Gateway

The gateway (`spinifex/runtime/roles/awsgw/awsgw.go`) is the entry point:

```go
// Connect to NATS (retries while the local broker comes up)
natsConn, err := natsmsg.ConnectNATSWithRetry(...)

// Load IAM (master key + JetStream KV) and create the gateway
gw := gateway.GatewayConfig{
    NATSConn:   natsConn,
    Config:     nodeConfig.AWSGW.Config,
    IAMService: iamService,
    // ...
}

// Serve over TLS
server.ListenAndServeTLS("", "")
```

Request routing (`spinifex/gateway/gateway.go`):

1. **Authentication**: SigV4 middleware validates AWS credentials and resolves the account ID
2. **Throttling**: Per-account+action token bucket rejects bursts post-auth
3. **Service Detection**: Extracts service name from the Authorization header
4. **Action Dispatch**: Routes to service-specific handler. A service registered through the ingress dispatch registry (`ingress/aws/dispatch`, wired by `runtime/roles/awsgw`) is dispatched by its registration; currently those are the ECR and ACM control planes, registered from `domains/ecr/awsapi` and `domains/acm/awsapi`. Every other service uses the legacy switch:

```go
switch svc {
case "ec2":
    err = gw.EC2_Request(w, r)
case "iam":
    err = gw.IAM_Request(w, r)
case "sts":
    err = gw.STS_Request(w, r)
case "elasticloadbalancing":
    err = gw.ELBv2_Request(w, r)
case "eks":
    err = gw.EKS_Request(w, r)
case "bedrock":
    err = gw.Bedrock_Request(w, r)
// ... bedrock-runtime, bedrock-agent, bedrock-agent-runtime
case "ecs":
    err = gw.ECS_Request(w, r)
case "rds":
    err = gw.RDS_Request(w, r)
case "tagging":
    err = gw.Tagging_Request(w, r)
case "spinifex":
    err = gw.Spinifex_Request(w, r)
}
```

### 3. EC2 Handler

The EC2 handler (`spinifex/gateway/ec2.go`) parses the `Action` parameter and delegates to specific handlers via the generic `ec2Handler` and `ec2HandlerWithReq` wrappers. The account ID resolved by SigV4 auth is threaded into every handler so requests are scoped per IAM principal:

```go
"RunInstances": ec2HandlerWithReq(func(ctx context.Context, input *ec2.RunInstancesInput, gw *GatewayConfig, accountID string, r *http.Request) (any, error) {
    // ... PassRole and launch-quota checks, then the vCPU quota charge
    return ec2instanceapi.RunInstances(ctx, input, gw.NATSConn, gw.IAMService, accountID, passRoleCheck, launchQuotaCheck, gw.ExpectedNodes)
}),
"DescribeInstances": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
    return ec2instanceapi.DescribeInstancesChecked(ctx, input, gw.NATSConn, 0, gw.NodeIDs, accountID)
}),
// ... + volumes, snapshots, VPCs, subnets, route tables, IGWs, NAT gateways,
// security groups, network interfaces, elastic IPs, placement groups, key pairs,
// images, tags, capacity reservations, spot instances
```

### 4. NATS Messaging

The gateway communicates with daemons via NATS request/response. Most calls go through `natsmsg.NATSRequest`, which marshals the input, attaches the account ID as a NATS header, and unmarshals the typed response:

```go
func (s *NATSInstanceService) RunInstances(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
    topic := fmt.Sprintf("ec2.RunInstances.%s", aws.StringValue(input.InstanceType))
    return natsmsg.NATSRequest[ec2.Reservation](ctx, s.natsConn, topic, input, 5*time.Minute, accountID)
}
```

The gateway's `RunInstances` places instances itself (`domains/ec2/awsapi/instance/placement.go`): it gathers capacity from every node on `spinifex.node.status`, spreads the launch across eligible nodes, and sends each node its share on the node-targeted subject `ec2.RunInstances.{type}.{nodeId}`, rolling back on partial failure.

### Cross-Process Contract Ownership

NATS subjects and JSON payloads that cross a process boundary are contracts,
not incidental implementation details. A domain owns its named and versioned
contract under `contracts/<domain>/vN`; consumers own only their narrow
in-process capability interfaces, and runtime composition wires the two.

The first realised example is the [EC2 instance-command contract](../contracts/ec2/v1/README.md).
`ec2.cmd.<instance-id>` is deliberately a targeted request/reply route: only
the node that owns the QEMU process may execute a live-instance command. Its
JSON shape and `ec2.cmd.*` subject form are compatibility-tested. The wildcard
matches one final instance-ID token; it is not a broad descendant subscription.

Changing that subject shape or a JSON field is a contract change, not a
refactor. It needs a versioned successor or an explicit compatibility plan.

### 5. Daemon Processing

Daemons (`spinifex/runtime/daemon/daemon.go`) subscribe to NATS topics and handle requests. A table-driven `subscribeAll()` registers the static EC2/ELBv2 surface at startup:

```go
// Queue group → load-balanced (one daemon handles each request)
{"ec2.CreateKeyPair", handleNATSRequest(d.node, d.keyService.CreateKeyPair), "spinifex-workers"},

// No queue group → fan-out (every daemon responds)
{"ec2.DescribeInstanceTypes", handleNATSRequest(d.node, d.instanceService.DescribeInstanceTypes), ""},
```

`ec2.RunInstances` is dynamic: `ResourceManager` subscribes to `ec2.RunInstances.{type}` only while the node has capacity for that type, and unsubscribes when full. The node-targeted subject `ec2.RunInstances.{type}.{nodeId}` stays subscribed even at full capacity — the launch-time `allocate()` enforces limits there. Per-instance commands flow over `ec2.cmd.{instanceID}`, subscribed only by the owning node.

**Queue Groups**: topics with the `spinifex-workers` queue group are load-balanced — only one daemon handles each request. Topics without a queue group fan out to all daemons.

### 6. VM Launch

When a daemon handles `RunInstances`:

1. **Resource Check**: Validates CPU/memory availability via `ResourceManager`
2. **Volume Generation**: Creates boot and EFI volumes via Viperblock
3. **Volume Mount**: Sends a mount request to the colocated Viperblock worker on `ebs.{node}.mount`, receives NBD URI
4. **QEMU Launch**: Builds and executes QEMU command with NBD-backed disks
5. **QMP Monitoring**: Establishes QEMU Machine Protocol connection for VM management
6. **Command subscription**: Subscribes to `ec2.cmd.{instanceID}` for subsequent commands on this VM

```go
// Volume mounting via NATS (runtime/daemon/vm_adapters.go)
reply, err := ebsRequestWithTrace(ctx, a.nc, instance.AccountID, viperblocklegacyv1.MountSubject(a.node), ebsMountRequest, 30*time.Second)

// QEMU launch with NBD-backed drives in args (runtime/compute/vm/vm.go)
cmd := exec.Command(qemuArchitecture, args...) // qemu-system-x86_64 or qemu-system-aarch64
```

## Key Components

### Daemon Structure

```go
type Daemon struct {
    node          string                  // Node identifier
    clusterConfig *config.ClusterConfig   // Cluster-wide configuration
    config        *config.Config          // Node-specific configuration
    natsConn      *nats.Conn              // NATS connection
    resourceMgr   *ResourceManager        // CPU/Memory tracking + dynamic RunInstances subscriptions
    vmMgr         *vm.Manager             // Local VMs
    // ... + one service struct per EC2/ELBv2 resource
    //       (instance, key, image, volume, snapshot, tags, vpc, subnet,
    //        igw, eigw, natgw, routetable, eip, placementgroup, elbv2, ...)
}
```

### Resource Manager

Tracks available and allocated CPU/memory to prevent overcommit, and drives the dynamic `ec2.RunInstances.{type}` subscriptions:

```go
type ResourceManager struct {
    mu            sync.RWMutex
    hostVCPU      int                                // raw runtime.NumCPU
    hostMemGB     float64                            // raw /proc/meminfo MemTotal
    reservedVCPU  int                                // held back for spinifex services
    reservedMem   float64                            // held back for spinifex services
    allocatedVCPU int
    allocatedMem  float64
    instanceTypes map[string]*ec2.InstanceTypeInfo   // t3.micro, t3.small, etc.
    // ... + capacity-reservation accounting, GPU manager, nbdkit memory
    //       overhead tracking, and the dynamic-subscription state
}
```

### Multi-Node Aggregation

For operations that need data from all nodes (like `DescribeInstances`), the gateway uses the scatter-gather helper `natsmsg.Gather` (`spinifex/foundation/messaging/nats/nats.go`), called from `spinifex/domains/ec2/awsapi/instance/DescribeInstances.go`:

```go
// Publish to every node (no queue group = all daemons respond) and collect
// the replies. The account ID travels as a NATS header so each daemon scopes
// its response.
frames, sum, err := natsmsg.Gather(ctx, natsConn, "ec2.DescribeInstances", jsonData, gatherOpts)
```

Given the configured node IDs, `Gather` checks each reply's `X-Node-ID` header against them, and `DescribeInstancesChecked` fails closed rather than returning a partial listing when a node is missing, duplicated or unidentified.

## Storage Integration

### Viperblock (EBS)

Block storage volumes are managed via NATS messages to Viperblock:

```go
// Volume mount request
type EBSRequest struct {
    Name      string  // Volume name (vol-xxxxx)
    VolType   string  // gp2, io1, etc.
    Boot      bool    // Boot volume flag
    EFI       bool    // EFI boot volume
    NBDURI    string  // NBD URI returned from mount
    // ... + DeviceName, DeleteOnTermination, hotplug port and host block-device fields
}
```

Viperblock responds with an NBD URI that QEMU uses to access the block device.

#### Direct-boot microVM data volumes

A direct-boot microVM keeps the launcher's saved `vm.Config` as a clean base, and `startQEMU` adds data volumes only to a launch-only copy of it (`runtime/compute/vm/lifecycle.go`).
At each start the daemon takes one snapshot of the instance's already-mounted data requests, probes each NBD or host block export from that snapshot, and builds the command from the same snapshot, so the launched endpoint is the one that was checked and is never written back into the saved base.
Each data volume becomes a `virtio-blk-device` on the MMIO bus with the same id, node name, I/O thread, serial and `werror=stop,rerror=stop` policy as the PCI device, and takes no PCIe hot-plug port.

Boot and EFI requests are rejected on this path, and a direct-boot instance with no data requests launches its base unchanged.
Disk-boot (q35) instances keep the existing PCI hot-plug path.

Hot-attaching a volume to a running direct-boot microVM is not supported, because the MMIO bus has no PCIe root port to plug into.
`AttachVolume` refuses it before mounting the volume or changing any instance or volume state, and the EC2 API returns `UnsupportedOperation`.

This covers only requests that are already mounted when the instance starts; nothing yet supplies data volumes to a system instance, and the path is verified with fake-QEMU unit tests, not with a real EBS volume in a guest.

#### Volume leases

Before opening an engine on a volume, the Viperblock worker takes a lease: one entry per volume in the `VIPERBLOCK_VOLUME_LEASES` JetStream KV bucket, which expires 45 seconds after its last write (`providers/ebs/viperblock/volume_lease.go`).
A renewal becomes eligible once the last confirmed write is 10 seconds old, and the holder checks for that periodically, so the rewrite lands some time after that point.
Each rewrite is conditioned on the revision the holder last wrote, and a refused write that finds the entry gone or naming another owner or generation fences the export as a takeover.
It may keep writing for 40 seconds after the last write the store acknowledged, measured from when that write was sent, because the server starts the entry's expiry when it applies the write and never earlier.
A slow acknowledgement therefore shortens the remaining validity rather than extending it, and an acquisition acknowledged after its validity has already run out is refused as retryable.

For a holder that cannot renew, a timer is armed for the validity deadline, leaving a nominal 5 seconds before the entry can expire; when it fires, the export's nbdkit is fenced with SIGKILL (`volume_fence.go`).
The fence runs on its own goroutine, so Go scheduling, that goroutine and process termination all delay the kill, and enough delay can use up the whole margin; a kill held in uninterruptible sleep, which the fence waits up to 30 seconds for, is the extreme case.
Once a lease is lost or has lapsed it stays lost: a renewal that succeeds afterwards is ignored, and another open on the same node cannot share it while the fence runs.

These rules bound only the holder's own clock.
The storage backend does not reject a stale writer, so a paused or partitioned host that resumes after its validity can still write; generation enforcement at the write boundary is required by Q-24 in the umbrella repository's `docs/specs/viperblock/rfc3-1_overview.md` and is not implemented.
An export that survives a daemon restart writes without a renewed lease until recovery adopts it, and a survivor that recovery cannot adopt while a guest still uses it is left running without a lease or a fence.

### Predastore (S3)

Object storage used for:
- AMI image storage
- SSH public key storage (`/bucket/ec2/{key-name}.pub`)
- Volume metadata and configuration

Spinifex runs Predastore in-process: `spx service predastore start` loads `/etc/spinifex/predastore/predastore.toml`, selects the `[[host]]` naming this machine, and serves it until the process is signalled. The topology is written by `spx admin init` / `join` from the servers that form the cluster.

One `[[host]]` is one process. The nodes pinned to it under `[[host.node]]` carry the roles:

| Role | Port | Responsibility |
|------|------|----------------|
| `gate` | TCP 8443 | The S3 API. Holds no cluster state; dials the meta and blob nodes to serve a request. |
| `blob` | UDP 6660 | Erasure-coded object shards, encrypted at rest under this host's master key. |
| `meta` | UDP 7660 | Raft replica of global state — buckets and the object index. |

Ports are unique within a host but repeat across the cluster, so a multi-node cluster puts one node of each role on every machine and every machine uses the same three ports. Nodes sharing a host talk over an in-process pipe and bind no socket; nodes on other hosts are reached over QUIC, authenticated by the cluster CA.

A single-node install is the same arrangement with one host: seven nodes — a gate, three blob and three meta — colocated in one process, needing seven distinct ports (8443, 6660–6662, 7660–7662) to stay unique within the host even though only the gate opens a listener. It has no code path of its own.

The daemon reports the topology on `spinifex.storage.config` (the
`contracts/operator/v1` Predastore-topology contract) by parsing the config
file. It does not connect to Predastore to do so, and no Predastore node
exposes a status endpoint. This is an operator status interface, not a generic
storage-provider contract.

### SQS message store (internal)

`domains/sqs/message` defines the store-neutral interface the SQS domain reaches its message store through (ADR-0011:S6), with a JetStream adapter in `domains/sqs/message/jetstream` and a shared conformance suite in `domains/sqs/message/conformance`.
It is an internal primitive: nothing outside the SQS domain uses it, no daemon or service is wired to it, and it does not itself deliver an SQS API.
The dedicated JetStream server ADR-0011 describes is not deployed by this code, and Spinifex ships no host service, unit, user or firewall rule for it; the guest that could run it is described under *SQS store system instance (not wired)*.

The interface covers durable and idempotent append, body and metadata reads, conditional state writes and deletes, per-namespace counts, namespace enumeration, an ordered change feed, a bounded paginated scan, namespace deletion and a sweep of tombstoned bodies.
A namespace carries an immutable message-shard and state-shard allocation chosen when the queue is created, so a later change in shard counts never moves an existing queue.

The adapter provisions a fixed set of shared message streams (`SQS_M_<shard>`, one subject per queue) and shared state buckets (`sqs_state_<shard>`), keeps historical ones, and applies a 14-day-plus-one-hour retention backstop to both.
It supports one node (R1) and three nodes (R3) only and refuses any other declared cluster size before provisioning.

Reads that must observe a completed write or delete go to the stream leader: `GetState`, the state-key pass behind `Count` and `Namespaces`, and the body pass behind `Namespaces` and `Scan`.
This avoids the follower-served direct gets and follower-placed consumers that returned purged data in local three-node fault-injection tests; it is not a general linearizability guarantee.
Each leader read is one request per record, and the body pass transfers each full body internally even though `Scan` returns metadata only, which is a material cost for a large backlog.
A state-key pass is bounded but is not a snapshot when a key is overwritten during it.

The feed reads through an in-memory, headers-only ordered consumer.
If that consumer delivers nothing for about two seconds while the stream holds a change after the feed's cursor, the feed replaces it from the cursor, so a stored change does not wait on a stalled consumer; the cause of those stalls is not established.
`Close` is terminal, waits for consumer opens and deletes in progress and retries failed deletes, but cleanup is best effort for a create whose outcome is unknown: a consumer that lands after its last delete is removed by the server's one-minute inactive threshold.

Coverage is unit and conformance tests on in-process R1 and R3 servers; there is no end-to-end or multi-host validation of this code, and earlier multi-host evidence predates the final feed.

### SQS store system instance (not wired)

Spinifex can build, install, launch, restart and give storage to a dedicated SQS store microVM, but nothing calls any of it.
No SQS API, domain service, admin command or daemon start-up path creates a store, so none of this is in production use.

The guest image is built by `make build-sqs-store-microvm-image`, separate from the load balancer image, and bundles the `nats-server` version pinned in `go.mod` plus the ext4 tools.
Release tarballs carry it as `microvm-sqs-store/`, and `setup.sh` installs it to `/usr/share/spinifex/microvm/sqs-store/`.
A release without a complete image removes any previously installed one, so a launch is then refused rather than booting a stale image.

At boot the guest reads three firmware config values: the data disk's serial, an IPv4 address on one of its interfaces, and one NATS user NKey public key.
It mounts exactly one matching disk at `/data` after `e2fsck -p`, refuses a missing, duplicate, unformatted or unrepairable disk, and never formats.
JetStream keeps its data on `/data` with `sync_interval: always` (ADR-0011:S3), and the server listens only on that address and accepts only that key.
On shutdown the store is stopped before `/data` is unmounted; a store that survives SIGKILL leaves every mount in place.

A launch uses the versioned subject `system.LaunchSQSStore.v1`, which a daemon without this code does not serve, so it fails with no responder instead of booting another guest.
The legacy `system.LaunchInstance` path refuses the store kind on both send and receive.
Every input check runs before capacity is reserved, and the data volume must be an available system-account volume.
It is mounted through the normal EBS path and detached, never deleted, on terminate, which is the protection standard EBS gives today and nothing stronger.
CPU mitigations stay enabled for the store, unlike the load balancer.
A successful launch means the VM is running, not that NATS is ready.

The instance record keeps the store's non-secret guest settings so a restart on the node that launched it rewrites the firmware config files.
A relaunch on any other node is refused, and cross-node recovery never claims a store.
Both checks exist only in daemons with this code: an older daemon can still claim and relaunch a store elsewhere, and an explicit start served by another node claims the instance before its relaunch is refused.

`domains/sqs/storevolume` prepares a fresh data volume, because the guest never formats.
It checks for `mkfs.ext4` and `qemu-img` before allocating, creates the volume in `creating`, writes an empty ext4 image to it over NBD on the local node, unpublishes it, and only then makes it `available`.
While a volume is `creating`, attach, delete, modify, force-detach and snapshot are refused.
A volume is deleted only after a confirmed unpublish; any publish error, an unconfirmed unpublish or a failed release keeps it and reports its ID, with its state reported as unknown after a failed release.

Evidence is local only: unit tests, real QEMU boots against disposable scratch disks, and one run of the preparer against the `qemunbdd` provider whose volume kept acknowledged writes across a reboot.
Viperblock, volume encryption, the EC2 volume record path, multiple nodes and host reachability of the guest address are not tested.
Credential issuance and rotation, store placement and NATS readiness checks do not exist, and there is no failover: a lost node leaves its store unavailable.

### SQS worker guest (not wired)

The shared SQS worker guest holds the SQS store's client credential, so that no platform component does (ADR-0011:S2).
Nothing activates it: no caller invokes the worker launcher below, and no worker service runs after boot.

`make build-sqs-worker-microvm-image` builds it with the store image's script and network `/init` under `SQS_MICROVM_PROFILE=worker`; it carries the static `sqs-worker` binary and the ext4 tools, no `nats-server`, and an empty `/data` mountpoint.
At boot `sqs-worker boot` reads three firmware config values under `opt/spinifex/sqs-worker/`: the worker disk's serial, its Region and the gateway URL; none is a first-store decision, because a static value survives a guest reset.
A missing, empty or malformed value refuses before any disk is touched.

It selects exactly one disk with that serial, runs `e2fsck -p`, mounts it at `/data` and then confirms from `/proc/self/mountinfo` that this disk, as a whole ext4 filesystem, is what is mounted there; an empty mountpoint, a foreign mount or a failed check or mount refuses, and the guest never formats.
Only then does it ask the gateway for the current decision with `ClaimFirstStore`, signed by `internal/sqsworkergw` with credentials from the instance metadata service only (`gwsign.NewInstanceRole`, no environment, profile or static-key fallback), over TLS 1.3 that trusts only the cluster CA from `opt/spinifex/ca-cert` and never follows a redirect.
Under that decision `workerkey.Acquire` creates the key on `/data/sqs-worker-key`, owner-only and synced, records only its public key and then uses it, or loads the recorded key; a missing or different key once one is recorded refuses and is never replaced.
Only a lost reply or a `ServiceUnavailable` or `Throttling` refusal is retried, re-asking the gateway each time; an authentication or other refusal, or an answer that is neither "create" nor one valid public key, refuses the boot.

`domains/sqs/firststore` is the SQS domain's first-store decision: one record per Region in the `spinifex-sqs-first-store` KV bucket moves from `claimed` to `key-recorded` to `launch-intent` under compare-and-swap, bound to the worker's instance ID, claim ID and generation.
A claim authorizes key creation only while no public key is recorded; once one is, every claim returns only that key, and a store launch intent needs it recorded first.
An older generation, a different identity at the same generation or an unreadable record authorizes nothing, and the record holds no private key material.
Reads are stream message gets answered by the stream leader, never direct gets a lagging replica can answer, and every authorizing answer is also confirmed by a compare-and-swap at the revision it read, so a stale or unavailable read authorizes nothing.
The authenticated caller that will carry this decision must derive the worker's instance ID and generation from authoritative instance state; a generation supplied in a request cannot itself authorize a takeover.

`domains/sqs/workerident` is that authoritative instance state: one record per Region in the `spinifex-sqs-worker-identity` KV bucket holds a monotonic generation, claim ID, pinned node and encrypted data volume, and moves from `launching` to `bound` to `stopped`, or from `launching` to `failed`, under compare-and-swap.
A launch intent is refused while the previous attempt is `launching` or `bound`, including one whose outcome is unknown, so only one caller ever holds an intent to start a VM on the pinned volume.
A replacement takes the next generation only after `FailLaunch` or `RecordStopped` records that nothing holds the volume, and each request names the generation it supersedes, so a replayed request cannot take a generation it never observed; there is no automatic takeover.
The launch path then raises the first-store fence to the successor's generation with `firststore.Fence` before `BeginLaunch`, which refuses a replacement until a leader read shows that fence; the fence only rises, and `firststore` refuses every call from a generation below it.
Only the current claim ID is recorded, so claim IDs are minted fresh per attempt and a VM launch must be idempotent on generation and claim together.
Binding writes the instance's index entry first and then binds the record to that instance.
The index is only a lookup: `Authorize` accepts an instance only while the Region's record is `bound` to exactly that instance, generation and claim, confirmed by a compare-and-swap at the leader-read revision, so an old, orphaned, rewritten or unbound entry authorizes nothing.
A same-instance reboot keeps its instance ID and identity; a replacement takes a new generation, and the first cut refuses another node or data volume and an unencrypted volume, although the encryption flag is the caller's claim and the launch wiring must verify it from the volume's metadata.
Both buckets are opened through `OpenDurableKV`, which refuses an existing bucket whose effective stream configuration could expire, evict, lose, lag or refuse records: non-limits retention, an age, per-message or delete-marker TTL, memory storage, asynchronous flush, a size, count or message-size cap, a discard policy other than discard-new, unacknowledged writes, a sealed stream, a mirror or sources, or fewer replicas than the cluster needs.
`workerkey.Acquire` drives that decision: it syncs a new key to disk, records its public key and only then returns it, so a crash before recording reuses the key already on disk.

`domains/sqs/awsapi/worker` carries the decision to the worker as `ClaimFirstStore` and `RecordWorkerPublicKey`, served under the internal `spinifex-sqs` signing service, which no AWS SDK signs for; the public AmazonSQS action inventory is untouched.
The gateway admits only an assumed-role session of `sqsWorkerRole` in the system account, then the role's policy, and only then resolves the session name, the instance ID STS set, through `workerident.Authorize`; the request carries no identity, and any parameter beyond the action's own is refused.
The first-store identity is the current record's instance, claim and generation, and a worker bound to another Region is refused.
A request that passed `Authorize` just before a confirmed stop and replacement still carries the old generation; the fence was raised before the successor began, so `firststore` refuses it whether or not the successor has claimed yet.
The adapter never logs a caller-chosen parameter name, because one could carry a secret.
`RecordWorkerPublicKey` accepts only a valid public user key, so a seed is refused without being recorded, logged or echoed.
The gateway binds both buckets per request and never creates them, so it provisions nothing and answers `ServiceUnavailable` until a worker launch has created them; the worker launch below ensures the role.
The private key stays on the worker disk; the guest logs and publishes only the public key to `/run/sqs-worker/public-key`.

`domains/sqs/workerlaunch` realizes the worker on the node it runs on, its only node: a bound record is returned as is; otherwise it prepares the data volume (first launch only), reads the volume's own record and refuses it unless encrypted, takes the intent, ensures the role, finds or creates the network for the claim and reads the recorded store key, and marks the intent dispatched before launching.
A dispatched launch is never sent again: a lost reply or error leaves the intent `launching` and dispatched, and every later call returns `ErrOutcomeUnknown` until the outcome is reconciled, since the launch path is not idempotent.
A failure before dispatch leaves an undispatched intent that the next call resumes with the same claim; a confirmed stop or failure is replaced only after `firststore.Fence` excludes the old generation, reusing the pinned volume.
The launch passes no store decision or key: the guest asks the gateway at boot.
The first launch refuses before allocating a volume when the cluster storage key is not configured or a claim ID cannot be minted. A volume a call did create but could not confirm recording in a launch intent, including one that loses the intent to another caller, is reported with its ID in an `UnclaimedVolumeError` and left in place for an operator, never deleted. The worker role is created with EC2-only trust, and an existing role is checked for that trust before any policy is granted; the instance profile must carry exactly the worker role, and one holding any other role is refused unchanged.
The daemon's `LaunchSQSWorkerInstance` boots the `sqs-worker` image through the same direct-boot path as the store, with the data volume attached at boot and kept on terminate, a required instance profile, and a required mgmt NIC; it re-reads the volume's own record and refuses one that is not available and encrypted, before anything is allocated.
Its firmware config adds the gateway URL on this node's mgmt bridge and the cluster CA from the daemon's own trust store; the caller cannot supply either, and no value written is secret.
A relaunch rebuilds that config from the VM record on the node that launched it and is refused anywhere else, and membership takeover already skips SQS-managed instances.
`sqsWorkerRole` is ensured in the system account granting exactly `spinifex-sqs:ClaimFirstStore` and `spinifex-sqs:RecordWorkerPublicKey`, from the `domains/sqs/workerrole` contract the gateway gate also reads; an existing role whose trust admits more than the EC2 service is refused.

Evidence is local only: unit tests with a fake firmware config, sysfs and mount table, tests running the boot through the real client against the real `spinifex-sqs` registration and records, and QEMU boots of the built image against disposable scratch disks and a host-side test gateway and metadata service, showing the first boot creates the key, reboots reuse it, a key lost after recording is never replaced, and the seed reaches no log or request.
The worker's network is a private SQS system VPC per Region, built by `systemvpc.EnsurePrivate` from the `[sqs]` supernet (default `10.240.0.0/14`): one VPC and one /24 subnet, with no internet gateway, NAT gateway, EIP, route table or daemon-to-store node port. Each launch claim gets one ENI in that subnet, in the VPC default security group, found again by its claim on a retry; two ENIs for one claim are refused. Worker-to-store TCP and the guest's route to the metadata service are not yet proven by any traffic test.
A caller for the launcher and stop/detach evidence for `RecordStopped` are not wired, the guest's metadata route on a real ENI is unproven, and Viperblock's at-rest key is held by the platform.

## Configuration

Cluster configuration (`spinifex/bootstrap/config/config.go`):

```go
type ClusterConfig struct {
    Epoch     uint64            // Incremented on config changes
    Node      string            // This node's identifier
    Version   string            // Spinifex version
    Network   NetworkConfig     // Cluster-wide external network settings (IP pools, IPsec)
    Bootstrap BootstrapConfig   // Default VPC IDs written by admin init
    AWS       AWSConfig         // Cluster-wide AWS-parity settings (region, endpoint suffix)
    Nodes     map[string]Config // Configuration for all nodes
}

type Config struct {
    Node, Host, AdvertiseIP  string
    Region, AZ               string
    BaseDir, DataDir, WalDir string
    Services   []string         // Which services this node runs locally
    Daemon     DaemonConfig
    NATS       NATSConfig       // Broker address, JetStream, ACL token, CA cert
    AWSGW      AWSGWConfig      // Gateway host, TLS certs, throttle config
    Predastore PredastoreConfig // S3 endpoint + service credentials
    Viperblock ViperblockConfig
    VPCD       VPCDConfig       // OVN/OVS endpoints
    // ... + EBS, Northstar, RDS, ACM, Bedrock, OchreVector
}
```

Configs are generated by `spx admin init` (leader) or `spx admin join` (followers).

## File Structure

The layout follows Mulga ADR-0001 (Spinifex package boundary and layout).
`architecture/layering` enforces its dependency rules in `make preflight`.

```
spinifex/
├── cmd/                     # Thin executable mains: spx, installer, in-guest agents, dev tools
├── contracts/               # Versioned cross-process wire contracts (contracts/<domain>/vN)
├── spinifex/
│   ├── foundation/          # Shared primitives: AWS errors/ARNs/filters/tags, KV state, messaging, telemetry, PKI
│   ├── providers/           # Backend adapters: EBS (Viperblock), object store, cloud (OCI, Exoscale)
│   ├── domains/             # Service domains (ec2, acm, ecr, dns, network, ochre, rds, tagging, ...)
│   │   └── <domain>/awsapi/ # A domain's AWS action adapters
│   ├── ingress/             # AWS protocol ingress: query/REST parsing, dispatch registry, rate limiting
│   ├── runtime/
│   │   ├── roles/           # Long-running service roles (awsgw, spinifex, vpcd, nats, predastore, ...)
│   │   ├── daemon/          # Per-node daemon and its NATS subscription table
│   │   └── compute/         # VM lifecycle, QMP, NBD, GPU
│   ├── bootstrap/           # Cluster and node configuration structures, preflight checks
│   ├── operator/            # spx CLI commands, admin use cases, account lifecycle, installer, upgrade
│   ├── agents/              # In-guest agent libraries (ecs, eks, elbv2, rds)
│   └── gateway/             # Legacy root: HTTP route assembly, SigV4 auth, legacy per-service dispatch
├── internal/                # In-guest SigV4 clients, AWS Smithy models and test kits
├── architecture/            # Executable architecture rules (package layering)
├── build/                   # systemd units, node helper scripts, microVM build
├── scripts/                 # Install, dev-environment, image-build scripts
└── tests/
    ├── integration/         # Gateway router against embedded NATS JetStream
    └── e2e/                 # Live-cluster suites (see docs/testing/e2e-coverage.md)
```

The one remaining legacy root, `gateway`, predates ADR-0001 and is frozen: it may shrink but not gain packages.
[`PACKAGE_BOUNDARY_MIGRATION.md`](PACKAGE_BOUNDARY_MIGRATION.md) records what remains in it and what blocks its move.

## Development

### Running the Stack

```bash
sudo spx admin init --node node1 --nodes 1
sudo systemctl start spinifex.target
```

`spx admin init` generates the cluster CA, IAM credentials, TLS certs, and AWS CLI `spinifex` profile, then writes config to `/etc/spinifex/` and data to `/var/lib/spinifex/`. `spinifex.target` brings up all services (NATS, Predastore, Viperblock, vpcd, the daemon, awsgw, the UI) under systemd. Use `spx admin join` for additional nodes.

### Testing with AWS CLI

```bash
export AWS_PROFILE=spinifex

# Run instance
aws ec2 run-instances --image-id ami-debian13 --instance-type t3.micro

# List instances (queries all nodes)
aws ec2 describe-instances

# Terminate instance
aws ec2 terminate-instances --instance-ids i-xxxxx
```

For cluster-state inspection that bypasses the AWS API surface, use the `spx` CLI directly: `spx get nodes`, `spx get vms`, `spx top nodes`.
