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

The gateway (`spinifex/services/awsgw/awsgw.go`) is the entry point:

```go
// Connect to NATS (retries while the local broker comes up)
natsConn, err := utils.ConnectNATSWithRetry(...)

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
3. **Service Detection**: Extracts service name (ec2, iam, account, elasticloadbalancing, spinifex) from the Authorization header
4. **Action Dispatch**: Routes to service-specific handler

```go
switch svc {
case "ec2":
    err = gw.EC2_Request(w, r)
case "account":
    err = gw.Account_Request(w, r)
case "iam":
    err = gw.IAM_Request(w, r)
case "elasticloadbalancing":
    err = gw.ELBv2_Request(w, r)
case "spinifex":
    err = gw.Spinifex_Request(w, r)
}
```

### 3. EC2 Handler

The EC2 handler (`spinifex/gateway/ec2.go`) parses the `Action` parameter and delegates to specific handlers via a generic `ec2Handler` wrapper. The account ID resolved by SigV4 auth is threaded into every handler so requests are scoped per IAM principal:

```go
"RunInstances": ec2Handler(func(input *ec2.RunInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
    return gateway_ec2_instance.RunInstances(input, gw.NATSConn, accountID)
}),
"DescribeInstances": ec2Handler(func(input *ec2.DescribeInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
    return gateway_ec2_instance.DescribeInstances(input, gw.NATSConn, gw.DiscoverActiveNodes(), accountID)
}),
// ... + volumes, snapshots, VPCs, subnets, route tables, IGWs, NAT gateways,
// security groups, network interfaces, placement groups, key pairs, images, tags
```

### 4. NATS Messaging

The gateway communicates with daemons via NATS request/response. Most calls go through `utils.NATSRequest`, which marshals the input, attaches the account ID as a NATS header, and unmarshals the typed response:

```go
func (s *NATSInstanceService) RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
    topic := fmt.Sprintf("ec2.RunInstances.%s", aws.StringValue(input.InstanceType))
    return utils.NATSRequest[ec2.Reservation](s.natsConn, topic, input, 5*time.Minute, accountID)
}
```

`RunInstances` uses a per-instance-type subject so NATS only delivers the request to a node with spare capacity for that type — no application-level reject-and-retry.

### 5. Daemon Processing

Daemons (`spinifex/daemon/daemon.go`) subscribe to NATS topics and handle requests. A table-driven `subscribeAll()` registers the static EC2/ELBv2 surface at startup:

```go
// Queue group → load-balanced (one daemon handles each request)
{"ec2.CreateKeyPair", d.handleEC2CreateKeyPair, "spinifex-workers"},

// No queue group → fan-out (every daemon responds)
{"ec2.DescribeInstanceTypes", d.handleEC2DescribeInstanceTypes, ""},
```

`ec2.RunInstances` is dynamic: `ResourceManager` subscribes to `ec2.RunInstances.{type}` (and `ec2.RunInstances.{type}.{nodeId}` for targeted dispatch) only while the node has capacity for that type, and unsubscribes when full. Per-instance commands flow over `ec2.cmd.{instanceID}`, subscribed only by the owning node.

**Queue Groups**: topics with the `spinifex-workers` queue group are load-balanced — only one daemon handles each request. Topics without a queue group fan out to all daemons.

### 6. VM Launch

When a daemon handles `RunInstances`:

1. **Resource Check**: Validates CPU/memory availability via `ResourceManager`
2. **Volume Generation**: Creates boot and EFI volumes via Viperblock
3. **Volume Mount**: Sends `ebs.mount` request to Viperblock, receives NBD URI
4. **QEMU Launch**: Builds and executes QEMU command with NBD-backed disks
5. **QMP Monitoring**: Establishes QEMU Machine Protocol connection for VM management
6. **Command subscription**: Subscribes to `ec2.cmd.{instanceID}` for subsequent commands on this VM

```go
// Volume mounting via NATS
msg, err := d.natsConn.Request("ebs.mount", ebsMountRequest, 10*time.Second)

// QEMU launch with NBD storage
cmd := exec.Command("qemu-system-x86_64",
    "-drive", fmt.Sprintf("file=nbd:%s,format=raw", nbdURI),
    // ... additional QEMU args
)
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
    Instances     vm.Instances            // Local VMs
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
}
```

### Multi-Node Aggregation

For operations that need data from all nodes (like `DescribeInstances`), the gateway uses inbox-based fan-out (`spinifex/gateway/ec2/instance/DescribeInstances.go`):

```go
func DescribeInstances(...) {
    // Create unique inbox for collecting responses
    inbox := nats.NewInbox()
    sub, _ := natsConn.SubscribeSync(inbox)

    // Publish to all nodes (no queue group = all daemons respond).
    // Account ID is propagated as a NATS header so each daemon scopes its response.
    pubMsg := nats.NewMsg("ec2.DescribeInstances")
    pubMsg.Reply = inbox
    pubMsg.Data = jsonData
    pubMsg.Header.Set(utils.AccountIDHeader, accountID)
    _ = natsConn.PublishMsg(pubMsg)

    // Collect responses, returning early once expectedNodes have replied
    // or the 3s timeout is hit.
    for {
        msg, err := sub.NextMsg(3 * time.Second)
        allReservations = append(allReservations, nodeOutput.Reservations...)
    }

    return allReservations
}
```

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
}
```

Viperblock responds with an NBD URI that QEMU uses to access the block device.

### Predastore (S3)

Object storage used for:
- AMI image storage
- SSH public key storage (`/bucket/ec2/{key-name}.pub`)
- Volume metadata and configuration

## Configuration

Cluster configuration (`spinifex/config/config.go`):

```go
type ClusterConfig struct {
    Epoch   uint64            // Incremented on config changes
    Node    string            // This node's identifier
    Version string            // Spinifex version
    Nodes   map[string]Config // Configuration for all nodes
}

type Config struct {
    Node, Host, Region, AZ string
    BaseDir, DataDir       string
    Daemon     DaemonConfig
    NATS       NATSConfig       // Broker address, JetStream, ACL token, CA cert
    AWSGW      AWSGWConfig      // Gateway host, TLS certs, throttle config
    Predastore PredastoreConfig // S3 endpoint + service credentials
    Viperblock ViperblockConfig
    VPCD       VPCDConfig       // OVN/OVS endpoints
    Network    NetworkConfig
}
```

Configs are generated by `spx admin init` (leader) or `spx admin join` (followers).

## File Structure

```
spinifex/
├── spinifex/
│   ├── services/                # Long-running services (awsgw, spinifex, viperblockd,
│   │                            #   predastore, nats, vpcd, spinifexui)
│   ├── gateway/                 # AWS API surface
│   │   ├── gateway.go           # chi router, SigV4 auth, throttling
│   │   ├── auth.go              # SigV4 verification + IAM lookup
│   │   ├── ec2.go               # EC2 action dispatcher
│   │   ├── ec2/                 # Per-resource handlers (instance, volume, vpc, ...)
│   │   ├── elbv2/, iam/, policy/
│   ├── handlers/                # NATS-side service implementations (ec2, elbv2, iam)
│   ├── daemon/
│   │   ├── daemon.go            # Daemon entry, NATS subscription table
│   │   └── daemon_handlers_*.go # One file per resource family
│   ├── admin/                   # Cluster bootstrap (init/join, master.key, CA, tokens)
│   ├── config/                  # Configuration structures
│   ├── instancetypes/           # t3.*, m5.*, sys.* definitions
│   ├── lbagent/                 # In-VM agent for ELB target health
│   ├── vm/                      # VM instance representation
│   └── qmp/                     # QEMU Machine Protocol client
└── cmd/
    ├── spinifex/                # spx CLI
    ├── lb-agent/                # ELB target agent binary
    └── installer/               # Platform installer
```

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
