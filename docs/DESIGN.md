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

// Load IAM (master key + JetStream KV), then compose the gateway: composeGateway
// sets every production hook and wires the service registry from each domain's
// awsapi registration.
gw, err := composeGateway(gatewayInputs{
    NATS:        natsConn,
    IAM:         iamService,
    Credentials: credentials,
    Admission:   admission,
    // ...
}, ecrControlPlane, acmControlPlane, ec2ControlPlane /* ... */)

// Serve over TLS
server.ListenAndServeTLS("", "")
```

Request routing (`spinifex/ingress/aws/pipeline/gateway.go`):

1. **Public routes**: `/oidc/eks/...` and `/v2` (the ECR registry) are mounted ahead of SigV4 by `runtime/roles/awsgw`
2. **Authentication**: SigV4 middleware validates AWS credentials and resolves the account ID
3. **Throttling**: Per-account+action token bucket rejects bursts post-auth
4. **Service Detection**: Extracts service name from the Authorization header
5. **Action Dispatch**: Every service is a registration (`ingress/aws/dispatch`) built by its domain's `awsapi.NewRegistration` and wired by `runtime/roles/awsgw`. The pipeline dispatches by registered service name and holds no per-service code. The private `/admin/<Method>` surface dispatches to the registration `awsgw` names as `Admin.Service`.

### 3. EC2 Handler

The EC2 registration (`spinifex/domains/ec2/awsapi`, `NewRegistration`) parses the `Action` parameter and looks the handler up in its action table. The account ID resolved by SigV4 auth is threaded into every handler so requests are scoped per IAM principal. Each action's handler is built from the owning EC2 resource package (`domains/ec2/instance`, `volume`, `vpc` and so on), with PassRole, quota and client-token idempotency wrapped on at the action table rather than inside the resource.

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

One instance vCPU is one CPU the kernel has online: a thread with SMT on, a core with SMT off or on ARM, as AWS counts a vCPU.
The OS and Spinifex's own services keep 1 CPU on a host with up to 8, 2 up to 16, 4 up to 63 and 8 from 64, and every other CPU is schedulable one-to-one with no overcommit.
[ADR-0015](https://github.com/mulgadc/mulga/blob/main/docs/adr/0015-host-cpu-capacity-and-reservation.md) fixes this rule.


```go
type ResourceManager struct {
    mu            sync.RWMutex
    hostVCPU      int                                // online CPUs (ADR-0015:S1)
    hostMemGB     float64                            // raw /proc/meminfo MemTotal
    reservedVCPU  int                                // ADR-0015:S2 tier: 1, 2, 4 or 8
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
The microVM boots with `pcie=on`, so its NICs are `virtio-net-pci` devices with MSI-X interrupts instead of `virtio-net-device` MMIO devices.
Each data volume becomes a `virtio-blk-pci` device on the default PCIe bus with the same id, node name, I/O thread, serial and `werror=stop,rerror=stop` policy as the q35 device, and takes no PCIe hot-plug port.
A microVM started without `pcie=on` keeps the `virtio-*-device` MMIO transports.

Boot and EFI requests are rejected on this path, and a direct-boot instance with no data requests launches its base unchanged.
Disk-boot (q35) instances keep the existing PCI hot-plug path.

Hot-attaching a volume to a running direct-boot microVM is not supported, because the microVM has no PCIe hot-plug root port to plug into.
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

#### Unsealed writes and start elsewhere (interim)

A volume is unsealed while its node holds writes the backend has not confirmed; the node records this in a dirty marker naming itself as owner (`providers/ebs/viperblock/volume_dirty.go`).
Opening such a volume on another node loses those writes, so two guards stop it while the previous owner may still hold them.
This is an interim guard until the instance lifecycle work defines ownership, and it is not a fence.

When `ec2.start` is forwarded to the node that last ran a stopped instance and that node refuses for capacity, the forwarding node normally starts the instance locally.
If the refusing node still holds unsealed writes for any of the instance's volumes, it sets the `X-Unsealed-Writes` NATS reply header and the forwarder relays `InsufficientInstanceCapacity` instead of starting elsewhere.
The check asks the owner's viperblockd over the per-node `unsealed` verb with a 5 second bound, and a check that fails or times out counts as holding, so the refusal is kept.

Independently, a node that finds another node's dirty marker on a volume refuses to open it when that node's daemon heartbeat (`heartbeat.<node>` in the cluster-state bucket) is under 60 seconds old.
It also refuses when the heartbeat cannot be read or no heartbeat source is wired, and the refusal maps to the held-elsewhere error that recovery retries.
A previous owner that has stopped heartbeating is opened over with a warning, as before.
The heartbeat age is compared across node clocks, so the 60 second window is also the clock-skew allowance.

EKS control-plane recovery treats both refusals, capacity and held-elsewhere, as retryable: it keeps retrying the stopped member and does not count them against its restart attempts.

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
It is used only by the SQS worker inside the SQS system instance and does not itself deliver an SQS API.
Spinifex ships no host service, unit, user or firewall rule for the dedicated JetStream server; the system instance that runs it is described under *SQS system instance (operator-launched)*.

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

### SQS system instance (operator-launched)

One system-owned microVM per Region runs both the SQS store and the SQS worker as separate processes, on one encrypted data volume pinned to one node (ADR-0011 Amendment 5, ADR-0012 Amendment 6).
Only an operator activates it with the super-admin `EnsureSQSWorker` or `EnableSQS` actions; nothing starts it at daemon startup, and platform NATS is neither used nor changed for SQS data.
This first profile is fresh single-node only: a host cluster of more than one node is refused before anything is recorded or allocated, and there is no disable.

`make build-sqs-worker-microvm-image` builds the one `sqs-worker` image: the static `sqs-worker` binary, the pinned `nats-server`, the ext4 tools and a network `/init`; releases ship it as `microvm-sqs-worker`, which `setup.sh` installs to `/usr/share/spinifex/microvm/sqs-worker`.
At boot the guest reads three firmware config values under `opt/spinifex/sqs-worker/` (the data disk's serial, its Region and the gateway URL on this node's mgmt bridge) and refuses before touching any disk if one is missing or malformed.
It selects exactly one disk with that serial, runs `e2fsck -p`, mounts it at `/data` and confirms from `/proc/self/mountinfo` that the whole disk is what is mounted; the guest never formats.

The store is a dedicated `nats-server` listening only on `127.0.0.1`, with its JetStream directory at `/data/jetstream` and `GOMEMLIMIT=512MiB` (ADR-0011:S11), so nothing outside the VM can reach it and it needs no network identity of its own.
The store starts only after the receipt key is settled, and the worker connects only after the store answers JetStream account info, so an unavailable store never appears ready.

The receipt-signing key lives on the data disk under `/data/sqs-worker-key`, written to a temporary file, synced, renamed and its directory synced before its fingerprint is recorded.
`domains/sqs/workerident` holds the platform's receipt-key state on the worker's identity record, beside its generation, claim, pinned node and volume: `pending` is set only when the record first creates its volume, and `recorded` holds the fingerprint.
Under `pending` the guest may create a key, or reuse a valid key it already wrote before a crash, only while no `/data/jetstream` directory exists; any store data under `pending` refuses, because it proves the volume is not new.
Under `recorded` a missing, unreadable or different key refuses the boot and is never regenerated; a record with no receipt-key state is refused as not proven new.
`RecordReceiptKey` writes the fingerprint by compare-and-swap that re-checks the current instance, generation, claim and volume, and a lost reply is answered by reading the record again, never by a new key.
A replacement worker reuses the pinned volume and keeps its predecessor's receipt-key state.

The worker reaches the gateway over TLS 1.3 trusting only the cluster CA, signing with instance-role credentials from the metadata service only.
The internal `spinifex-sqs` actions are `ClaimReceiptKey`, `RecordReceiptKey`, `PollWork` and `CompleteWork`; the gateway admits only an assumed-role session of `sqsWorkerRole` in the system account and resolves the instance through `workerident.Authorize`, a point-in-time check against the current bound record.
The worker pulls tenant work from its own node's gateway relay and posts results back, so no SQS payload crosses platform NATS; the relay holds a bounded, in-memory set of waiting requests and answers `ServiceUnavailable` when it is full or no worker is polling.

`EnsureSQSWorker` takes a `NodeId`, an `InstanceType` and a `VolumeSizeGiB`; the instance type's memory must exceed the store's 512 MiB limit, and a later request for a different type or size is refused as `IncorrectState`.
`EnableSQS` records the Region's desired state, pinned to one node, and then ensures the worker; `status` reports it ready only when the worker is bound, polling and the tenant API is registered.
A store launch record or VM from the earlier separate-store design, or a worker record without a supported launch contract, is refused with its volume kept: enable stops before recording anything, and a relaunch of an old store VM is marked failed rather than started.

Evidence is local: unit tests, the guest shell test and an AWS SDK test that creates a queue and sends, receives and deletes a message through the real worker loop against a real local JetStream store and a restart.
A real guest boot, its metadata route on a real ENI and the signed path on a live node are unproven, Viperblock's at-rest key is held by the platform, and SQS-managed message encryption is not implemented, so the profile must not hold tenant production data.

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
