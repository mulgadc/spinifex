package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
)

type BlockDeviceMapping struct {
	DeviceName string `json:"DeviceName"`
	EBS        EBS    `json:"EBS"`
}

type EBS struct {
	DeleteOnTermination      bool
	Encrypted                bool
	Iops                     int
	KmsKeyId                 string
	OutpostArn               string
	SnapshotId               string
	Throughput               int
	VolumeInitializationRate int
	VolumeSize               int
	VolumeType               string
}

// ResourceManager handles the allocation and tracking of system resources.
// It dynamically manages per-instance-type NATS subscriptions: when capacity
// is available for a type, the node subscribes to ec2.RunInstances.{type};
// when full, it unsubscribes so NATS routes requests to other nodes.
type ResourceManager struct {
	mu            sync.RWMutex
	availableVCPU int
	availableMem  float64
	allocatedVCPU int
	allocatedMem  float64
	instanceTypes map[string]*ec2.InstanceTypeInfo

	// Dynamic instance-type subscription management
	subsMu       sync.Mutex
	natsConn     *nats.Conn
	instanceSubs map[string]*nats.Subscription
	handler      nats.MsgHandler
	nodeID       string // node identifier for node-specific topic subscriptions
}

// Daemon represents the main daemon service
type Daemon struct {
	node                  string
	clusterConfig         *config.ClusterConfig
	config                *config.Config
	natsConn              *nats.Conn
	resourceMgr           *ResourceManager
	instanceService       *handlers_ec2_instance.InstanceServiceImpl
	keyService            *handlers_ec2_key.KeyServiceImpl
	imageService          *handlers_ec2_image.ImageServiceImpl
	volumeService         *handlers_ec2_volume.VolumeServiceImpl
	accountService        *handlers_ec2_account.AccountSettingsServiceImpl
	snapshotService       *handlers_ec2_snapshot.SnapshotServiceImpl
	tagsService           *handlers_ec2_tags.TagsServiceImpl
	eigwService           *handlers_ec2_eigw.EgressOnlyIGWServiceImpl
	igwService            *handlers_ec2_igw.IGWServiceImpl
	placementGroupService *handlers_ec2_placementgroup.PlacementGroupServiceImpl
	vpcService            *handlers_ec2_vpc.VPCServiceImpl
	eipService            *handlers_ec2_eip.EIPServiceImpl
	elbv2Service          *handlers_elbv2.ELBv2ServiceImpl
	routeTableService     *handlers_ec2_routetable.RouteTableServiceImpl
	natGatewayService     *handlers_ec2_natgw.NatGatewayServiceImpl
	externalIPAM          *handlers_ec2_vpc.ExternalIPAM
	ctx                   context.Context
	cancel                context.CancelFunc
	shutdownWg            sync.WaitGroup

	// Local VM Instances
	Instances vm.Instances

	// NAT Subscriptions
	natsSubscriptions map[string]*nats.Subscription

	// Cluster manager
	clusterServer *http.Server
	startTime     time.Time
	configPath    string

	// System credentials for ALB agent SigV4 auth (loaded from system-credentials.json)
	systemAccessKey string
	systemSecretKey string

	// JetStream manager for KV state storage (nil if JetStream disabled)
	jsManager *JetStreamManager

	// Delay after QMP device_del before blockdev-del (default 1s, 0 in tests)
	detachDelay time.Duration

	// NATS connect retry options (nil uses defaults: 5min max, 500ms initial delay)
	natsRetryOpts []utils.RetryOption

	// NetworkPlumber handles tap device lifecycle for VPC networking
	networkPlumber NetworkPlumber

	// Management NIC infrastructure: bridge IP + IP allocator for system instances.
	// Populated at startup when br-mgmt is detected; nil/empty otherwise.
	mgmtBridgeIP    string
	mgmtIPAllocator *MgmtIPAllocator
	// mgmtRouteVia is the AWSGW bind IP that system instances must route via the
	// management NIC. Set when AWSGW binds to a specific IP (multi-node).
	mgmtRouteVia string

	// shuttingDown is set to true during coordinated cluster shutdown (GATE phase)
	// or during SIGTERM-based shutdown. When true, the daemon rejects new work,
	// crash handlers bail out, and setupShutdown skips redundant VM stops.
	shuttingDown atomic.Bool

	// ready is set to true once NATS connection, JetStream, and all services
	// are fully initialized. The health endpoint reports "starting" until ready.
	ready atomic.Bool

	mu sync.Mutex
}

// getSystemMemory returns the total system memory in GB
func getSystemMemory() (float64, error) {
	switch runtime.GOOS {
	case "darwin":
		// macOS: use sysctl
		cmd := exec.Command("sysctl", "-n", "hw.memsize")
		output, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("failed to get system memory on macOS: %w", err)
		}
		memBytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse memory size on macOS: %w", err)
		}
		return float64(memBytes) / (1024 * 1024 * 1024), nil

	case "linux":
		// Linux: read from /proc/meminfo
		cmd := exec.Command("grep", "MemTotal", "/proc/meminfo")
		output, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("failed to read /proc/meminfo: %w", err)
		}

		// Parse the output (format: "MemTotal:       16384 kB")
		fields := strings.Fields(string(output))
		if len(fields) < 3 {
			return 0, fmt.Errorf("unexpected /proc/meminfo format")
		}

		memKB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse memory size from /proc/meminfo: %w", err)
		}

		// Convert KB to GB
		return float64(memKB) / (1024 * 1024), nil

	default:
		return 0, fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

// NewResourceManager creates a new resource manager with system capabilities.
// Returns an error if system memory cannot be detected, since an incorrect
// default would either under-provision (large servers) or over-commit (small devices).
func NewResourceManager() (*ResourceManager, error) {
	// Get system CPU cores
	numCPU := runtime.NumCPU()

	// Get system memory (in GB)
	totalMemGB, err := getSystemMemory()
	if err != nil {
		return nil, fmt.Errorf("detect system memory: %w", err)
	}

	// Determine architecture
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}

	// Detect CPU generation and generate matching instance types
	instanceTypes := instancetypes.DetectAndGenerate(instancetypes.HostCPU{}, arch)

	slog.Info("System resources detected",
		"vCPUs", numCPU, "memGB", totalMemGB,
		"instanceTypes", len(instanceTypes))

	return &ResourceManager{
		availableVCPU: numCPU,
		availableMem:  totalMemGB,
		instanceTypes: instanceTypes,
	}, nil
}

// instanceTypeVCPUs returns the default vCPU count for an instance type, or 0 if unavailable.
func instanceTypeVCPUs(it *ec2.InstanceTypeInfo) int64 {
	if it.VCpuInfo != nil && it.VCpuInfo.DefaultVCpus != nil {
		return *it.VCpuInfo.DefaultVCpus
	}
	return 0
}

// instanceTypeMemoryMiB returns the memory in MiB for an instance type, or 0 if unavailable.
func instanceTypeMemoryMiB(it *ec2.InstanceTypeInfo) int64 {
	if it.MemoryInfo != nil && it.MemoryInfo.SizeInMiB != nil {
		return *it.MemoryInfo.SizeInMiB
	}
	return 0
}

// GetInstanceTypeInfos returns all instance types as ec2.InstanceTypeInfo for AWS API compatibility
func (rm *ResourceManager) GetInstanceTypeInfos() []*ec2.InstanceTypeInfo {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var infos []*ec2.InstanceTypeInfo
	for name, it := range rm.instanceTypes {
		if instancetypes.IsSystemType(name) {
			continue
		}
		infos = append(infos, it)
	}
	return infos
}

// GetAvailableInstanceTypeInfos returns instance types based on total host capacity.
// If showCapacity is true, it returns multiple entries representing available slots.
// If showCapacity is false, it returns each supported type only once.
func (rm *ResourceManager) GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var infos []*ec2.InstanceTypeInfo

	for name, it := range rm.instanceTypes {
		if instancetypes.IsSystemType(name) {
			continue
		}

		vCPUs := instanceTypeVCPUs(it)
		memMiB := instanceTypeMemoryMiB(it)

		if vCPUs == 0 || memMiB == 0 {
			continue
		}

		count := canAllocateCount(
			rm.availableVCPU, rm.allocatedVCPU,
			rm.availableMem, rm.allocatedMem,
			vCPUs, memMiB,
			1<<30, // effectively unlimited — let resources be the constraint
		)

		if showCapacity {
			for range count {
				infos = append(infos, it)
			}
		} else if count > 0 {
			infos = append(infos, it)
		}
	}

	slog.Info("GetAvailableInstanceTypeInfos", "total_types", len(rm.instanceTypes), "total_available_slots", len(infos),
		"hostVCPU", rm.availableVCPU, "hostMem", rm.availableMem, "showCapacity", showCapacity)

	return infos
}

// GetResourceStats returns current resource allocation stats for the node status response.
func (rm *ResourceManager) GetResourceStats() (totalVCPU int, totalMemGB float64, allocVCPU int, allocMemGB float64, caps []types.InstanceTypeCap) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	totalVCPU = rm.availableVCPU
	totalMemGB = rm.availableMem
	allocVCPU = rm.allocatedVCPU
	allocMemGB = rm.allocatedMem

	remainingVCPU := rm.availableVCPU - rm.allocatedVCPU
	remainingMem := rm.availableMem - rm.allocatedMem

	for name, it := range rm.instanceTypes {
		if instancetypes.IsSystemType(name) {
			continue
		}
		typeCap := resourceStatsForType(remainingVCPU, remainingMem, it)
		if typeCap.VCPU == 0 || typeCap.MemoryGB == 0 {
			continue
		}
		caps = append(caps, typeCap)
	}
	return totalVCPU, totalMemGB, allocVCPU, allocMemGB, caps
}

// SetConfigPath sets the configuration file path for cluster management
func (d *Daemon) SetConfigPath(path string) {
	d.configPath = path
}

// NewDaemon creates a new daemon instance
func NewDaemon(cfg *config.ClusterConfig) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// If WalDir is not set, use BaseDir
	config := cfg.Nodes[cfg.Node]
	if cfg.Nodes[cfg.Node].WalDir == "" {
		config.WalDir = config.BaseDir

		cfg.Nodes[cfg.Node] = config
	}

	rm, err := NewResourceManager()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize resource manager: %w", err)
	}

	return &Daemon{
		node:              cfg.Node,
		clusterConfig:     cfg,
		config:            &config,
		resourceMgr:       rm,
		ctx:               ctx,
		cancel:            cancel,
		Instances:         vm.Instances{VMS: make(map[string]*vm.VM)},
		natsSubscriptions: make(map[string]*nats.Subscription),
		startTime:         time.Now(),
		detachDelay:       1 * time.Second,
	}, nil
}

// natsSub defines a single NATS subscription entry for the table-driven setup.
type natsSub struct {
	topic      string
	handler    nats.MsgHandler
	queueGroup string // empty = plain Subscribe (fan-out)
}

// subscribeAll registers all NATS subscriptions using a table-driven approach.
func (d *Daemon) subscribeAll() error {
	subs := []natsSub{
		// ec2.RunInstances is handled by dynamic per-instance-type subscriptions
		// managed by ResourceManager.initSubscriptions()
		{"ec2.CreateKeyPair", d.handleEC2CreateKeyPair, "spinifex-workers"},
		{"ec2.DeleteKeyPair", d.handleEC2DeleteKeyPair, "spinifex-workers"},
		{"ec2.DescribeKeyPairs", d.handleEC2DescribeKeyPairs, "spinifex-workers"},
		{"ec2.ImportKeyPair", d.handleEC2ImportKeyPair, "spinifex-workers"},
		{"ec2.DescribeImages", d.handleEC2DescribeImages, "spinifex-workers"},
		{"ec2.CreateImage", d.handleEC2CreateImage, ""},
		{"ec2.DeregisterImage", d.handleEC2DeregisterImage, "spinifex-workers"},
		{"ec2.RegisterImage", d.handleEC2RegisterImage, "spinifex-workers"},
		{"ec2.CopyImage", d.handleEC2CopyImage, "spinifex-workers"},
		{"ec2.DescribeImageAttribute", d.handleEC2DescribeImageAttribute, "spinifex-workers"},
		{"ec2.ModifyImageAttribute", d.handleEC2ModifyImageAttribute, "spinifex-workers"},
		{"ec2.ResetImageAttribute", d.handleEC2ResetImageAttribute, "spinifex-workers"},
		{"ec2.CreateVolume", d.handleEC2CreateVolume, "spinifex-workers"},
		{"ec2.DescribeVolumes", d.handleEC2DescribeVolumes, "spinifex-workers"},
		{"ec2.ModifyVolume", d.handleEC2ModifyVolume, "spinifex-workers"},
		{"ec2.DeleteVolume", d.handleEC2DeleteVolume, "spinifex-workers"},
		{"ec2.DescribeVolumeStatus", d.handleEC2DescribeVolumeStatus, "spinifex-workers"},
		{"ec2.CreateSnapshot", d.handleEC2CreateSnapshot, "spinifex-workers"},
		{"ec2.DescribeSnapshots", d.handleEC2DescribeSnapshots, "spinifex-workers"},
		{"ec2.DeleteSnapshot", d.handleEC2DeleteSnapshot, "spinifex-workers"},
		{"ec2.CopySnapshot", d.handleEC2CopySnapshot, "spinifex-workers"},
		{"ec2.CreateTags", d.handleEC2CreateTags, "spinifex-workers"},
		{"ec2.DeleteTags", d.handleEC2DeleteTags, "spinifex-workers"},
		{"ec2.DescribeTags", d.handleEC2DescribeTags, "spinifex-workers"},
		{"ec2.CreateEgressOnlyInternetGateway", d.handleEC2CreateEgressOnlyInternetGateway, "spinifex-workers"},
		{"ec2.DeleteEgressOnlyInternetGateway", d.handleEC2DeleteEgressOnlyInternetGateway, "spinifex-workers"},
		{"ec2.DescribeEgressOnlyInternetGateways", d.handleEC2DescribeEgressOnlyInternetGateways, "spinifex-workers"},
		{"ec2.CreateInternetGateway", d.handleEC2CreateInternetGateway, "spinifex-workers"},
		{"ec2.DeleteInternetGateway", d.handleEC2DeleteInternetGateway, "spinifex-workers"},
		{"ec2.DescribeInternetGateways", d.handleEC2DescribeInternetGateways, "spinifex-workers"},
		{"ec2.AttachInternetGateway", d.handleEC2AttachInternetGateway, "spinifex-workers"},
		{"ec2.DetachInternetGateway", d.handleEC2DetachInternetGateway, "spinifex-workers"},
		{"ec2.CreatePlacementGroup", d.handleEC2CreatePlacementGroup, "spinifex-workers"},
		{"ec2.DeletePlacementGroup", d.handleEC2DeletePlacementGroup, "spinifex-workers"},
		{"ec2.DescribePlacementGroups", d.handleEC2DescribePlacementGroups, "spinifex-workers"},
		{"ec2.ReserveSpreadNodes", d.handleEC2ReserveSpreadNodes, "spinifex-workers"},
		{"ec2.FinalizeSpreadInstances", d.handleEC2FinalizeSpreadInstances, "spinifex-workers"},
		{"ec2.ReleaseSpreadNodes", d.handleEC2ReleaseSpreadNodes, "spinifex-workers"},
		{"ec2.RemoveInstanceFromPlacementGroup", d.handleEC2RemoveInstanceFromPlacementGroup, "spinifex-workers"},
		{"ec2.ReserveClusterNode", d.handleEC2ReserveClusterNode, "spinifex-workers"},
		{"ec2.FinalizeClusterInstances", d.handleEC2FinalizeClusterInstances, "spinifex-workers"},
		{"ec2.CreateNatGateway", d.handleEC2CreateNatGateway, "spinifex-workers"},
		{"ec2.DeleteNatGateway", d.handleEC2DeleteNatGateway, "spinifex-workers"},
		{"ec2.DescribeNatGateways", d.handleEC2DescribeNatGateways, "spinifex-workers"},
		{"ec2.CreateRouteTable", d.handleEC2CreateRouteTable, "spinifex-workers"},
		{"ec2.DeleteRouteTable", d.handleEC2DeleteRouteTable, "spinifex-workers"},
		{"ec2.DescribeRouteTables", d.handleEC2DescribeRouteTables, "spinifex-workers"},
		{"ec2.CreateRoute", d.handleEC2CreateRoute, "spinifex-workers"},
		{"ec2.DeleteRoute", d.handleEC2DeleteRoute, "spinifex-workers"},
		{"ec2.ReplaceRoute", d.handleEC2ReplaceRoute, "spinifex-workers"},
		{"ec2.AssociateRouteTable", d.handleEC2AssociateRouteTable, "spinifex-workers"},
		{"ec2.DisassociateRouteTable", d.handleEC2DisassociateRouteTable, "spinifex-workers"},
		{"ec2.ReplaceRouteTableAssociation", d.handleEC2ReplaceRouteTableAssociation, "spinifex-workers"},
		{"ec2.CreateVpc", d.handleEC2CreateVpc, "spinifex-workers"},
		{"ec2.DeleteVpc", d.handleEC2DeleteVpc, "spinifex-workers"},
		{"ec2.DescribeVpcs", d.handleEC2DescribeVpcs, "spinifex-workers"},
		{"ec2.CreateSubnet", d.handleEC2CreateSubnet, "spinifex-workers"},
		{"ec2.DeleteSubnet", d.handleEC2DeleteSubnet, "spinifex-workers"},
		{"ec2.DescribeSubnets", d.handleEC2DescribeSubnets, "spinifex-workers"},
		{"ec2.ModifySubnetAttribute", d.handleEC2ModifySubnetAttribute, "spinifex-workers"},
		{"ec2.ModifyVpcAttribute", d.handleEC2ModifyVpcAttribute, "spinifex-workers"},
		{"ec2.DescribeVpcAttribute", d.handleEC2DescribeVpcAttribute, "spinifex-workers"},
		{"ec2.CreateNetworkInterface", d.handleEC2CreateNetworkInterface, "spinifex-workers"},
		{"ec2.DeleteNetworkInterface", d.handleEC2DeleteNetworkInterface, "spinifex-workers"},
		{"ec2.DescribeNetworkInterfaces", d.handleEC2DescribeNetworkInterfaces, "spinifex-workers"},
		{"ec2.ModifyNetworkInterfaceAttribute", d.handleEC2ModifyNetworkInterfaceAttribute, "spinifex-workers"},
		{"ec2.CreateSecurityGroup", d.handleEC2CreateSecurityGroup, "spinifex-workers"},
		{"ec2.DeleteSecurityGroup", d.handleEC2DeleteSecurityGroup, "spinifex-workers"},
		{"ec2.DescribeSecurityGroups", d.handleEC2DescribeSecurityGroups, "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupIngress", d.handleEC2AuthorizeSecurityGroupIngress, "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupEgress", d.handleEC2AuthorizeSecurityGroupEgress, "spinifex-workers"},
		{"ec2.RevokeSecurityGroupIngress", d.handleEC2RevokeSecurityGroupIngress, "spinifex-workers"},
		{"ec2.RevokeSecurityGroupEgress", d.handleEC2RevokeSecurityGroupEgress, "spinifex-workers"},
		{"ec2.ModifyInstanceAttribute", d.handleEC2ModifyInstanceAttribute, "spinifex-workers"},
		{"ec2.DescribeInstanceAttribute", d.handleEC2DescribeInstanceAttribute, "spinifex-workers"},
		{"ec2.start", d.handleEC2StartStoppedInstance, "spinifex-workers"},
		{"ec2.terminate", d.handleEC2TerminateStoppedInstance, "spinifex-workers"},
		{"ec2.DescribeStoppedInstances", d.handleEC2DescribeStoppedInstances, "spinifex-workers"},
		{"ec2.DescribeTerminatedInstances", d.handleEC2DescribeTerminatedInstances, "spinifex-workers"},
		// these 2 fan out to all nodes and gateway aggregates the results
		{"ec2.DescribeInstances", d.handleEC2DescribeInstances, ""},
		{"ec2.DescribeInstanceTypes", d.handleEC2DescribeInstanceTypes, ""},
		{"ec2.EnableEbsEncryptionByDefault", d.handleEC2EnableEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.DisableEbsEncryptionByDefault", d.handleEC2DisableEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.GetEbsEncryptionByDefault", d.handleEC2GetEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.GetSerialConsoleAccessStatus", d.handleEC2GetSerialConsoleAccessStatus, "spinifex-workers"},
		{"ec2.EnableSerialConsoleAccess", d.handleEC2EnableSerialConsoleAccess, "spinifex-workers"},
		{"ec2.DisableSerialConsoleAccess", d.handleEC2DisableSerialConsoleAccess, "spinifex-workers"},
		// ELBv2 operations
		{"elbv2.CreateLoadBalancer", d.handleELBv2CreateLoadBalancer, "spinifex-workers"},
		{"elbv2.DeleteLoadBalancer", d.handleELBv2DeleteLoadBalancer, "spinifex-workers"},
		{"elbv2.DescribeLoadBalancers", d.handleELBv2DescribeLoadBalancers, "spinifex-workers"},
		{"elbv2.CreateTargetGroup", d.handleELBv2CreateTargetGroup, "spinifex-workers"},
		{"elbv2.DeleteTargetGroup", d.handleELBv2DeleteTargetGroup, "spinifex-workers"},
		{"elbv2.DescribeTargetGroups", d.handleELBv2DescribeTargetGroups, "spinifex-workers"},
		{"elbv2.RegisterTargets", d.handleELBv2RegisterTargets, "spinifex-workers"},
		{"elbv2.DeregisterTargets", d.handleELBv2DeregisterTargets, "spinifex-workers"},
		{"elbv2.DescribeTargetHealth", d.handleELBv2DescribeTargetHealth, "spinifex-workers"},
		{"elbv2.CreateListener", d.handleELBv2CreateListener, "spinifex-workers"},
		{"elbv2.DeleteListener", d.handleELBv2DeleteListener, "spinifex-workers"},
		{"elbv2.DescribeListeners", d.handleELBv2DescribeListeners, "spinifex-workers"},
		{"elbv2.DescribeTags", d.handleELBv2DescribeTags, "spinifex-workers"},
		{"elbv2.LBAgentHeartbeat", d.handleELBv2LBAgentHeartbeat, "spinifex-workers"},
		{"elbv2.GetLBConfig", d.handleELBv2GetLBConfig, "spinifex-workers"},
		{"elbv2.ModifyTargetGroupAttributes", d.handleELBv2ModifyTargetGroupAttributes, "spinifex-workers"},
		{"elbv2.DescribeTargetGroupAttributes", d.handleELBv2DescribeTargetGroupAttributes, "spinifex-workers"},
		{"elbv2.ModifyLoadBalancerAttributes", d.handleELBv2ModifyLoadBalancerAttributes, "spinifex-workers"},
		{"elbv2.DescribeLoadBalancerAttributes", d.handleELBv2DescribeLoadBalancerAttributes, "spinifex-workers"},
		{fmt.Sprintf("spinifex.admin.%s.health", d.node), d.handleHealthCheck, ""},
		{"spinifex.nodes.discover", d.handleNodeDiscover, ""},
		{"spinifex.node.status", d.handleNodeStatus, ""},
		{"spinifex.node.vms", d.handleNodeVMs, ""},
		{"spinifex.storage.config", d.handleStorageConfig, ""},
		// Account creation → create default VPC for new account
		{"iam.account.created", d.handleAccountCreated, "spinifex-workers"},
		// Coordinated cluster shutdown phases (fan-out, no queue group)
		{"spinifex.cluster.shutdown.gate", d.handleShutdownGate, ""},
		{"spinifex.cluster.shutdown.drain", d.handleShutdownDrain, ""},
		{"spinifex.cluster.shutdown.storage", d.handleShutdownStorage, ""},
		{"spinifex.cluster.shutdown.persist", d.handleShutdownPersist, ""},
		{"spinifex.cluster.shutdown.infra", d.handleShutdownInfra, ""},
	}

	// EIP operations require external IPAM (pool mode). Only subscribe when available;
	// without a subscriber the gateway returns a NATS timeout → clean error to the client.
	if d.eipService != nil {
		subs = append(subs,
			natsSub{"ec2.AllocateAddress", d.handleEC2AllocateAddress, "spinifex-workers"},
			natsSub{"ec2.ReleaseAddress", d.handleEC2ReleaseAddress, "spinifex-workers"},
			natsSub{"ec2.AssociateAddress", d.handleEC2AssociateAddress, "spinifex-workers"},
			natsSub{"ec2.DisassociateAddress", d.handleEC2DisassociateAddress, "spinifex-workers"},
			natsSub{"ec2.DescribeAddresses", d.handleEC2DescribeAddresses, "spinifex-workers"},
			natsSub{"ec2.DescribeAddressesAttribute", d.handleEC2DescribeAddressesAttribute, "spinifex-workers"},
		)
	}

	for _, s := range subs {
		var sub *nats.Subscription
		var err error
		if s.queueGroup != "" {
			sub, err = d.natsConn.QueueSubscribe(s.topic, s.queueGroup, s.handler)
		} else {
			sub, err = d.natsConn.Subscribe(s.topic, s.handler)
		}
		if err != nil {
			return fmt.Errorf("failed to subscribe to %s: %w", s.topic, err)
		}
		d.natsSubscriptions[s.topic] = sub
		slog.Info("Subscribed to NATS topic", "topic", s.topic, "queue", s.queueGroup)
	}
	return nil
}

// Start initializes and starts the daemon
func (d *Daemon) Start() error {
	if err := d.connectNATS(); err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	// ClusterManager must start before JetStream init so peers can reach
	// /health during bootstrap.
	if err := d.ClusterManager(); err != nil {
		return fmt.Errorf("failed to start cluster manager: %w", err)
	}

	if err := d.initJetStream(); err != nil {
		return fmt.Errorf("failed to initialize JetStream: %w", err)
	}

	// Write service manifest so other nodes know what this node runs
	if d.jsManager != nil {
		if err := d.jsManager.WriteServiceManifest(
			d.node,
			d.config.GetServices(),
			admin.DialTarget(d.config.NATS.Host),
			admin.DialTarget(d.config.Predastore.Host),
		); err != nil {
			slog.Warn("Failed to write service manifest", "error", err)
		}
	}

	// Create services before loading/launching instances, since LaunchInstance depends on them
	store := objectstore.NewS3ObjectStoreFromConfig(admin.DialTarget(d.config.Predastore.Host), d.config.Predastore.Region, d.config.Predastore.AccessKey, d.config.Predastore.SecretKey)
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(d.config, d.resourceMgr.instanceTypes, d.natsConn, &d.Instances, store)
	d.keyService = handlers_ec2_key.NewKeyServiceImpl(d.config)
	d.imageService = handlers_ec2_image.NewImageServiceImpl(d.config, d.natsConn)

	type snapResult struct {
		svc *handlers_ec2_snapshot.SnapshotServiceImpl
		kv  nats.KeyValue
	}
	snap, err := initServiceWithRetry("snapshot service", func() (snapResult, error) {
		svc, kv, err := handlers_ec2_snapshot.NewSnapshotServiceImplWithNATS(d.config, d.natsConn)
		return snapResult{svc, kv}, err
	})
	if err != nil {
		return fmt.Errorf("failed to initialize snapshot service: %w", err)
	}
	d.snapshotService = snap.svc

	d.volumeService = handlers_ec2_volume.NewVolumeServiceImpl(d.config, d.natsConn, snap.kv)
	d.tagsService = handlers_ec2_tags.NewTagsServiceImpl(d.config)

	d.eigwService, err = initServiceWithRetry("EIGW service", func() (*handlers_ec2_eigw.EgressOnlyIGWServiceImpl, error) {
		return handlers_ec2_eigw.NewEgressOnlyIGWServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize EIGW service: %w", err)
	}

	d.igwService, err = initServiceWithRetry("IGW service", func() (*handlers_ec2_igw.IGWServiceImpl, error) {
		return handlers_ec2_igw.NewIGWServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize IGW service: %w", err)
	}

	d.placementGroupService, err = initServiceWithRetry("placement group service", func() (*handlers_ec2_placementgroup.PlacementGroupServiceImpl, error) {
		return handlers_ec2_placementgroup.NewPlacementGroupServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize placement group service: %w", err)
	}

	d.vpcService, err = initServiceWithRetry("VPC service", func() (*handlers_ec2_vpc.VPCServiceImpl, error) {
		return handlers_ec2_vpc.NewVPCServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize VPC service: %w", err)
	}

	d.routeTableService, err = initServiceWithRetry("RouteTable service", func() (*handlers_ec2_routetable.RouteTableServiceImpl, error) {
		return handlers_ec2_routetable.NewRouteTableServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize RouteTable service: %w", err)
	}

	d.natGatewayService, err = initServiceWithRetry("NatGateway service", func() (*handlers_ec2_natgw.NatGatewayServiceImpl, error) {
		return handlers_ec2_natgw.NewNatGatewayServiceImplWithNATS(d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize NatGateway service: %w", err)
	}

	// Initialize external IPAM if pool mode is configured (per-VM public IPs).
	// NAT mode only uses SNAT via a shared gateway IP — no per-VM allocation needed.
	if d.clusterConfig != nil && d.clusterConfig.Network.ExternalMode == "pool" {
		js, jsErr := d.natsConn.JetStream()
		if jsErr != nil {
			slog.Warn("Failed to get JetStream for external IPAM", "err", jsErr)
		} else {
			var pools []handlers_ec2_vpc.ExternalPoolConfig
			// Resolve DHCP bind bridge name for DHCP pools (where AF_PACKET binds).
			dhcpBindBridge := ""
			if node, ok := d.clusterConfig.Nodes[d.clusterConfig.Node]; ok {
				dhcpBindBridge = node.VPCD.DhcpBindBridge
			}
			for _, p := range d.clusterConfig.Network.ExternalPools {
				pools = append(pools, handlers_ec2_vpc.ExternalPoolConfig{
					Name:           p.Name,
					Source:         p.Source,
					RangeStart:     p.RangeStart,
					RangeEnd:       p.RangeEnd,
					Gateway:        p.Gateway,
					GatewayIP:      p.GatewayIP,
					PrefixLen:      p.PrefixLen,
					Region:         p.Region,
					AZ:             p.AZ,
					DhcpBindBridge: dhcpBindBridge,
				})
			}
			d.externalIPAM, err = handlers_ec2_vpc.NewExternalIPAM(d.natsConn, js, pools)
			if err != nil {
				slog.Warn("Failed to initialize external IPAM", "err", err)
			} else {
				slog.Info("External IPAM initialized", "mode", d.clusterConfig.Network.ExternalMode, "pools", len(pools))
			}
		}
	}

	// Initialize EIP service if external IPAM is available
	if d.externalIPAM != nil && d.vpcService != nil {
		eipSvc, eipErr := handlers_ec2_eip.NewEIPServiceImpl(d.natsConn, d.externalIPAM, d.vpcService)
		if eipErr != nil {
			slog.Warn("Failed to initialize EIP service", "err", eipErr)
		} else {
			d.eipService = eipSvc
			slog.Info("EIP service initialized")
		}

		// Inject external IPAM + EIP KV into VPC service so DeleteNetworkInterface
		// can release auto-assigned public IPs and NAT rules.
		eipJS, eipJSErr := d.natsConn.JetStream()
		if eipJSErr != nil {
			slog.Warn("Failed to get JetStream for VPC external IPAM injection", "err", eipJSErr)
		} else {
			eipKV, eipKVErr := utils.GetOrCreateKVBucket(eipJS, handlers_ec2_eip.KVBucketEIPs, 10)
			if eipKVErr != nil {
				slog.Warn("Failed to get EIP KV bucket for VPC service", "err", eipKVErr)
			} else {
				d.vpcService.SetExternalIPAM(d.externalIPAM, eipKV)
			}
		}
	}

	d.accountService, err = initServiceWithRetry("account settings service", func() (*handlers_ec2_account.AccountSettingsServiceImpl, error) {
		return handlers_ec2_account.NewAccountSettingsServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize account settings service: %w", err)
	}

	d.elbv2Service, err = initServiceWithRetry("ELBv2 service", func() (*handlers_elbv2.ELBv2ServiceImpl, error) {
		return handlers_elbv2.NewELBv2ServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize ELBv2 service: %w", err)
	}
	if d.vpcService != nil {
		d.elbv2Service.VPCService = d.vpcService
	}

	// Wire LB VM lifecycle: instance launcher for system VMs.
	d.elbv2Service.InstanceLauncher = d

	// Detect management bridge for system instance control plane NICs.
	// Must run before wireLBAgentConfig so the gateway URL uses br-mgmt IP.
	mgmtBridge := "br-mgmt"
	if d.config.Daemon.MgmtBridge != "" {
		mgmtBridge = d.config.Daemon.MgmtBridge
	}
	bridgeIP, bridgeErr := GetBridgeIPv4(mgmtBridge)
	if bridgeErr != nil {
		slog.Warn("Management bridge not detected, system instances will not get mgmt NIC", "bridge", mgmtBridge, "err", bridgeErr)
	} else if bridgeIP == "" {
		slog.Warn("Management bridge not found, system instances will not get mgmt NIC", "bridge", mgmtBridge)
	} else {
		d.mgmtBridgeIP = bridgeIP
		alloc, allocErr := NewMgmtIPAllocator(bridgeIP)
		if allocErr != nil {
			slog.Error("Failed to create mgmt IP allocator", "bridgeIP", bridgeIP, "err", allocErr)
		} else {
			d.mgmtIPAllocator = alloc
			slog.Info("Management bridge detected", "bridge", mgmtBridge, "ip", bridgeIP)
		}
	}

	// Wire system credentials + gateway URL for LB agent SigV4 auth.
	d.wireLBAgentConfig()

	// Set up lazy system AMI discovery for LB VMs. The image may not exist
	// at daemon startup (imported later), so we resolve it at request time.
	if d.imageService != nil {
		imgSvc := d.imageService
		d.elbv2Service.SetSystemAMIFunc(func() (string, error) {
			imagesOut, imgErr := imgSvc.DescribeImages(&ec2.DescribeImagesInput{
				Filters: []*ec2.Filter{{
					Name:   aws.String("tag:" + tags.ManagedByKey),
					Values: []*string{aws.String(tags.ManagedByELBv2)},
				}},
			}, utils.GlobalAccountID)
			if imgErr != nil {
				return "", fmt.Errorf("describe LB system images: %w", imgErr)
			}
			if len(imagesOut.Images) == 0 {
				return "", errors.New("LB system image not imported; run: spx admin images import --name lb-alpine-3.21.6-x86_64")
			}
			amiID := aws.StringValue(imagesOut.Images[0].ImageId)
			slog.Info("System AMI resolved for LB VMs", "amiId", amiID, "name", aws.StringValue(imagesOut.Images[0].Name))
			return amiID, nil
		})
	}

	// System VMs (LB, NAT GW) use the dedicated sys.micro instance type.
	d.elbv2Service.SetSystemInstanceTypeFunc(func() string {
		return "sys.micro"
	})

	// Ensure default VPC exists for system and admin accounts
	// (matches AWS: every account has a default VPC with IGW + default SG)
	if d.vpcService != nil {
		for _, accountID := range []string{utils.GlobalAccountID, admin.DefaultAccountID()} {
			// Pass bootstrap IDs for the admin account so EnsureDefaultVPC uses
			// the same IDs that admin init wrote to [bootstrap] in spinifex.toml.
			var opts []handlers_ec2_vpc.BootstrapIDs
			if accountID == admin.DefaultAccountID() && d.clusterConfig != nil && d.clusterConfig.Bootstrap.VpcId != "" {
				opts = append(opts, handlers_ec2_vpc.BootstrapIDs{
					VpcId:    d.clusterConfig.Bootstrap.VpcId,
					SubnetId: d.clusterConfig.Bootstrap.SubnetId,
				})
			}
			if _, err := d.vpcService.EnsureDefaultVPC(accountID, opts...); err != nil {
				slog.Error("Failed to ensure default VPC", "accountID", accountID, "error", err)
			}
		}
		// Ensure default VPC has an IGW and default security group
		d.ensureDefaultVPCInfrastructure()
	}

	// Initialize network plumber for VPC tap device management
	if d.networkPlumber == nil {
		d.networkPlumber = &OVSNetworkPlumber{}
	}

	// Protect daemon from OOM killer (prefer killing QEMU VMs instead)
	if err := utils.SetOOMScore(os.Getpid(), -500); err != nil {
		slog.Warn("Failed to set daemon OOM score", "err", err)
	}

	d.waitForClusterReady()
	d.upgradeJetStreamReplicas()
	d.restoreInstances()

	// Rebuild mgmt IP allocator from restored VMs so we don't re-allocate IPs
	// that are already in use by running system instances.
	if d.mgmtIPAllocator != nil {
		d.Instances.Mu.Lock()
		d.mgmtIPAllocator.Rebuild(d.Instances.VMS)
		d.Instances.Mu.Unlock()
		slog.Info("Rebuilt mgmt IP allocator from restored instances", "allocated", d.mgmtIPAllocator.AllocatedCount())
	}

	if err := d.subscribeAll(); err != nil {
		return fmt.Errorf("failed to subscribe to NATS topics: %w", err)
	}

	// Initialize dynamic per-instance-type subscriptions for capacity-aware routing.
	// Each instance type gets its own NATS topic (ec2.RunInstances.{type}) so requests
	// are only routed to nodes with available capacity.
	d.resourceMgr.initSubscriptions(d.natsConn, d.handleEC2RunInstances, d.node)

	d.startHeartbeat()
	d.startPendingWatchdog()

	d.ready.Store(true)
	slog.Info("Daemon fully initialized", "node", d.node, "startupTime", time.Since(d.startTime).Round(time.Second))

	d.setupShutdown()
	d.awaitShutdown()

	return nil
}

// connectNATS establishes a connection to the NATS server with retry and
// exponential backoff. On multi-node clusters, the local NATS server may not
// be ready immediately after daemon start (e.g. if start-dev.sh is still
// launching services). This retries for up to 5 minutes before giving up.
func (d *Daemon) connectNATS() error {
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(d.config.NATS.Host), d.config.NATS.ACL.Token, d.config.NATS.CACert, d.natsRetryOpts...)
	if err != nil {
		return err
	}
	d.natsConn = nc
	return nil
}

// initJetStream initializes JetStream with retry/backoff and upgrades replicas
// for multi-node clusters. On multi-node clusters, JetStream requires NATS
// cluster quorum which may take several minutes if nodes start at different
// times. This retries for up to 5 minutes to allow late-joining nodes.
func (d *Daemon) initJetStream() error {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		var err error
		d.jsManager, err = NewJetStreamManager(d.natsConn, 1)
		if err == nil {
			err = d.jsManager.InitKVBucket()
		}

		if err == nil {
			err = d.jsManager.InitClusterStateBucket()
		}

		if err == nil {
			err = d.jsManager.InitTerminatedInstanceBucket()
		}

		if err == nil {
			slog.Info("JetStream KV stores initialized successfully", "replicas", 1, "attempts", attempt, "elapsed", time.Since(start).Round(time.Second))
			break
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			return fmt.Errorf("failed to initialize JetStream after %s (%d attempts): %w", elapsed.Round(time.Second), attempt, err)
		}

		slog.Warn("JetStream not ready (waiting for cluster quorum)", "error", err, "attempt", attempt, "elapsed", elapsed.Round(time.Second), "retryIn", retryDelay)
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}

	// Replica upgrade is deferred to after all services have created their
	// KV buckets and the cluster is ready (see upgradeJetStreamReplicas).

	return nil
}

// upgradeJetStreamReplicas bumps the replication factor on ALL KV_* streams
// to match the cluster size. This runs after all services have created their
// buckets and after waitForClusterReady so that enough NATS peers are online
// to accept the new replica count.
func (d *Daemon) upgradeJetStreamReplicas() {
	clusterSize := len(d.clusterConfig.Nodes)
	if clusterSize <= 1 || d.jsManager == nil {
		return
	}
	if err := d.jsManager.UpdateReplicas(clusterSize); err != nil {
		slog.Warn("Failed to upgrade JetStream replicas", "targetReplicas", clusterSize, "error", err)
	}
}

// initServiceWithRetry initializes a service using the provided init function,
// retrying with exponential backoff (500ms→10s) for up to 5 minutes. During
// cluster restarts, JetStream KV may be temporarily unavailable while NATS
// routes re-establish and the cluster forms quorum.
func initServiceWithRetry[T any](name string, initFn func() (T, error)) (T, error) {
	const maxWait = 5 * time.Minute
	retryDelay := 500 * time.Millisecond
	start := time.Now()
	attempt := 0

	for {
		attempt++
		result, err := initFn()
		if err == nil {
			if attempt > 1 {
				slog.Info(name+" initialized successfully", "attempts", attempt, "elapsed", time.Since(start).Round(time.Second))
			}
			return result, nil
		}

		elapsed := time.Since(start)
		if elapsed >= maxWait {
			var zero T
			return zero, fmt.Errorf("%s unavailable after %s (%d attempts): %w", name, elapsed.Round(time.Second), attempt, err)
		}

		slog.Warn("Failed to init "+name, "error", err, "attempt", attempt, "elapsed", elapsed.Round(time.Second))
		time.Sleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}

// waitForClusterReady waits until dependent infrastructure services are reachable
// before starting VM recovery. This prevents races where VMs try to mount volumes
// before viperblock/predastore are ready.
func (d *Daemon) waitForClusterReady() {
	slog.Info("Waiting for cluster readiness...")
	maxWait := 2 * time.Minute
	start := time.Now()
	interval := 2 * time.Second

	for time.Since(start) < maxWait {
		ready := true
		var reason string

		// Viperblock must be reachable (local or remote)
		if ready && !d.checkViperblockReady() {
			ready = false
			reason = "viperblock not ready"
		}

		// Predastore must be reachable (local or remote)
		if ready && !d.checkPredastoreReady() {
			ready = false
			reason = "predastore not ready"
		}

		if ready {
			slog.Info("Cluster readiness check passed", "elapsed", time.Since(start))
			return
		}

		slog.Debug("Cluster not ready, waiting...", "reason", reason, "elapsed", time.Since(start))
		time.Sleep(interval)
	}

	slog.Warn("Cluster readiness timeout, proceeding with recovery anyway", "maxWait", maxWait)
}

// checkViperblockReady checks if viperblock is reachable by verifying
// the NATS connection is up (viperblock subscribes to ebs topics on NATS).
func (d *Daemon) checkViperblockReady() bool {
	if d.natsConn == nil {
		return false
	}
	return d.natsConn.IsConnected()
}

// checkPredastoreReady checks if predastore is reachable via TCP.
func (d *Daemon) checkPredastoreReady() bool {
	host := admin.DialTarget(d.config.Predastore.Host)
	if host == "" {
		return true // no predastore configured, skip check
	}
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// migrateInstanceToKV writes an instance to the given KV write function and removes
// it from the local instance map. Returns true if migration succeeded.
func (d *Daemon) migrateInstanceToKV(instance *vm.VM, writeFn func(string, *vm.VM) error, label string) bool {
	if d.jsManager == nil {
		return false
	}
	instance.LastNode = d.node
	if err := writeFn(instance.ID, instance); err != nil {
		slog.Error("Failed to migrate instance to KV",
			"instance", instance.ID, "bucket", label, "err", err)
		return false
	}
	delete(d.Instances.VMS, instance.ID)
	slog.Info("Migrated instance to KV", "instance", instance.ID, "bucket", label)
	return true
}

func (d *Daemon) migrateStoppedToSharedKV(instance *vm.VM) bool {
	return d.migrateInstanceToKV(instance, d.jsManager.WriteStoppedInstance, "stopped")
}

func (d *Daemon) migrateTerminatedToKV(instance *vm.VM) bool {
	return d.migrateInstanceToKV(instance, d.jsManager.WriteTerminatedInstance, "terminated")
}

// maxConcurrentRecovery limits how many VMs are relaunched in parallel during recovery.
const maxConcurrentRecovery = 2

// restoreInstances loads persisted VM state and re-launches instances that are
// neither terminated nor flagged as user-stopped.
func (d *Daemon) restoreInstances() {
	// Check for clean shutdown marker
	cleanShutdown := false
	if d.jsManager != nil {
		if marker, err := d.jsManager.ReadShutdownMarker(d.node); err == nil {
			cleanShutdown = marker
			if marker {
				slog.Info("Clean shutdown marker found, trusting KV state")
				_ = d.jsManager.DeleteShutdownMarker(d.node)
			}
		}
	}

	if !cleanShutdown {
		slog.Warn("No clean shutdown marker — possible crash recovery, validating QEMU PIDs carefully")
		time.Sleep(3 * time.Second)
	}

	err := d.LoadState()
	if err != nil {
		slog.Warn("Failed to load state, continuing with empty state", "error", err)
		return
	}

	slog.Info("Loaded state", "instance count", len(d.Instances.VMS))

	// Ensure mutexes and QMP clients are usable after deserialization
	d.Instances.Mu = sync.Mutex{}

	// Phase 1: Reconnect running QEMU, finalize transitional states, collect VMs to relaunch
	var toLaunch []*vm.VM

	for i := range d.Instances.VMS {
		d.Instances.VMS[i].EBSRequests.Mu = sync.Mutex{}
		d.Instances.VMS[i].QMPClient = &qmp.QMPClient{}

		instance := d.Instances.VMS[i]

		if instance.Status == vm.StateTerminated {
			if !d.migrateTerminatedToKV(instance) {
				// KV write failed — keep in local state so the next restart
				// retries the migration. Deleting here would create a "void":
				// the instance disappears from both local state and the
				// terminated KV, making it invisible to DescribeInstances.
				slog.Warn("Terminated instance KV migration failed, will retry on next restart",
					"instance", instance.ID)
			}
			continue
		}

		if instance.Status == vm.StateStopped {
			d.migrateStoppedToSharedKV(instance)
			continue
		}

		instanceType, ok := d.resourceMgr.instanceTypes[instance.InstanceType]
		if !ok && instance.InstanceType != "" {
			slog.Warn("Instance type not available on this node, moving to stopped",
				"instanceId", instance.ID, "instanceType", instance.InstanceType)
			instance.Status = vm.StateStopped
			if instance.Instance != nil {
				instance.Instance.StateReason = &ec2.StateReason{}
				instance.Instance.StateReason.SetCode("Server.InsufficientInstanceCapacity")
				instance.Instance.StateReason.SetMessage(
					fmt.Sprintf("instance type %s is not available on this node", instance.InstanceType))
			}
			d.migrateStoppedToSharedKV(instance)
			continue
		}

		if ok {
			slog.Info("Re-allocating resources for instance", "instanceId", instance.ID, "type", instance.InstanceType)
			if err := d.resourceMgr.allocate(instanceType); err != nil {
				slog.Error("Failed to re-allocate resources for instance on startup, moving to stopped",
					"instanceId", instance.ID, "err", err)
				instance.Status = vm.StateStopped
				if instance.Instance != nil {
					instance.Instance.StateReason = &ec2.StateReason{}
					instance.Instance.StateReason.SetCode("Server.InsufficientInstanceCapacity")
					instance.Instance.StateReason.SetMessage(
						fmt.Sprintf("insufficient resources to restore instance: %v", err))
				}
				d.migrateStoppedToSharedKV(instance)
				continue
			}
		}

		// Check if QEMU process is still alive from before the restart
		if d.isInstanceProcessRunning(instance) {
			// Verify NBD sockets are still valid. After a viperblock restart,
			// old sockets are gone and QEMU's block devices are broken. Kill
			// the orphaned QEMU and relaunch from scratch instead of
			// reconnecting to an instance with dead storage.
			if !d.areVolumeSocketsValid(instance) {
				slog.Warn("QEMU alive but NBD sockets are stale, killing orphaned process for relaunch",
					"instance", instance.ID)
				pid, pidErr := utils.ReadPidFile(instance.ID)
				if pidErr != nil || pid <= 0 {
					slog.Error("Cannot read PID for orphaned QEMU, skipping relaunch",
						"instanceId", instance.ID, "err", pidErr)
					continue
				}
				// SIGKILL directly — orphaned QEMU with dead storage has no
				// state worth a graceful shutdown, and KillProcess's 120s
				// SIGTERM timeout would block daemon startup.
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Signal(syscall.SIGKILL)
				}
				// Wait for the process to actually die, then remove the PID
				// file ourselves. SIGKILL cannot be caught, so QEMU never
				// runs its cleanup handler and the PID file stays on disk.
				if err := utils.WaitForProcessExit(pid, 10*time.Second); err != nil {
					slog.Error("Orphaned QEMU did not exit after SIGKILL, skipping relaunch",
						"instanceId", instance.ID, "pid", pid, "err", err)
					continue
				}
				_ = utils.RemovePidFile(instance.ID)
			} else {
				slog.Info("Instance QEMU process still alive, reconnecting", "instance", instance.ID)
				if err := d.reconnectInstance(instance); err != nil {
					slog.Error("Failed to reconnect to running instance", "instanceId", instance.ID, "err", err)
				}
				continue
			}
		}

		// QEMU is not running -- resolve transitional states from interrupted operations
		switch instance.Status {
		case vm.StateStopping, vm.StateShuttingDown:
			prevStatus := instance.Status
			if instance.Status == vm.StateStopping {
				instance.Status = vm.StateStopped
			} else {
				instance.Status = vm.StateTerminated
			}
			slog.Info("QEMU exited during transition, finalizing state",
				"instance", instance.ID, "from", prevStatus, "to", instance.Status)

			if instance.Status == vm.StateStopped && d.migrateStoppedToSharedKV(instance) {
				continue
			}

			if instance.Status == vm.StateTerminated && d.migrateTerminatedToKV(instance) {
				continue
			}

			if err := d.WriteState(); err != nil {
				slog.Error("Failed to persist state, will retry on next restart",
					"instance", instance.ID, "error", err)
				instance.Status = prevStatus // revert so next restart retries
			}
			continue
		case vm.StateRunning:
			// Was running but QEMU died - reset to pending so LaunchInstance can transition to running
			instance.Status = vm.StatePending
			slog.Info("Instance was running but QEMU exited, relaunching", "instance", instance.ID)
		}

		// Reset LaunchTime so the pending watchdog gives a fresh timeout window.
		// Without this, the stale LaunchTime from the original launch causes the
		// watchdog to immediately mark the instance as failed after a prolonged outage.
		now := time.Now()
		if instance.Instance != nil {
			instance.Instance.LaunchTime = &now
		}
		toLaunch = append(toLaunch, instance)
	}

	// Phase 2: Relaunch crashed VMs with semaphore-based throttling
	if len(toLaunch) > 0 {
		// Subscribe to per-instance NATS topics before launching so that
		// terminate/stop commands can reach this daemon while instances are
		// still being relaunched. Without this, pending instances are
		// unreachable via ec2.cmd.<id> and TerminateInstances fails.
		d.mu.Lock()
		for _, instance := range toLaunch {
			sub, subErr := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
			if subErr != nil {
				slog.Error("Failed to early-subscribe during recovery", "instanceId", instance.ID, "err", subErr)
			} else {
				d.natsSubscriptions[instance.ID] = sub
			}
		}
		d.mu.Unlock()

		slog.Info("Launching instances (recovery)", "count", len(toLaunch), "maxConcurrent", maxConcurrentRecovery)
		sem := make(chan struct{}, maxConcurrentRecovery)
		var wg sync.WaitGroup

		for _, instance := range toLaunch {
			sem <- struct{}{} // acquire
			wg.Add(1)
			go func(inst *vm.VM) {
				defer wg.Done()
				defer func() { <-sem }() // release
				defer func() {
					if r := recover(); r != nil {
						slog.Error("Panic during instance recovery", "instanceId", inst.ID, "panic", r, "stack", string(debug.Stack()))
					}
				}()
				// Skip if instance was terminated while waiting for semaphore
				d.Instances.Mu.Lock()
				status := inst.Status
				d.Instances.Mu.Unlock()
				if status != vm.StatePending && status != vm.StateProvisioning {
					slog.Info("Instance state changed during recovery, skipping launch",
						"instanceId", inst.ID, "status", string(status))
					return
				}
				slog.Info("Launching instance (recovery)", "instance", inst.ID)
				if err := d.LaunchInstance(inst); err != nil {
					slog.Error("Failed to launch instance during recovery", "instanceId", inst.ID, "err", err)
					d.markInstanceFailed(inst, "recovery_launch_failed")
				}
			}(instance)
		}
		wg.Wait()
	}

	// Persist state after any migrations/removals during restore
	if err := d.WriteState(); err != nil {
		slog.Error("Failed to persist state after restore", "error", err)
	}
}

// isInstanceProcessRunning checks if the QEMU process for an instance is still alive.
func (d *Daemon) isInstanceProcessRunning(instance *vm.VM) bool {
	pid, err := utils.ReadPidFile(instance.ID)
	if err != nil || pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// areVolumeSocketsValid checks whether the NBD Unix sockets backing an
// instance's volumes are reachable. A dial probe (not just os.Stat) is used
// because viperblock may restart with sockets at the same paths — the file
// exists but no process is listening on the old fd that QEMU holds.
func (d *Daemon) areVolumeSocketsValid(instance *vm.VM) bool {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	for _, req := range instance.EBSRequests.Requests {
		if req.NBDURI == "" {
			continue
		}
		serverType, sockPath, _, _, err := utils.ParseNBDURI(req.NBDURI)
		if err != nil || serverType != "unix" {
			continue // TCP or unparseable — can't validate locally
		}
		conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
		if err != nil {
			slog.Debug("NBD socket unreachable", "volume", req.Name, "socket", sockPath, "err", err)
			return false
		}
		_ = conn.Close()
	}
	return true
}

// reconnectInstance re-establishes QMP and NATS connections to a running QEMU instance
// after a daemon restart. This bypasses the state machine since recovery is not a
// normal state transition.
func (d *Daemon) reconnectInstance(instance *vm.VM) error {
	if err := d.CreateQMPClient(instance); err != nil {
		return fmt.Errorf("failed to reconnect QMP: %w", err)
	}

	d.mu.Lock()
	sub, err := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
	if err != nil {
		d.mu.Unlock()
		if instance.QMPClient != nil && instance.QMPClient.Conn != nil {
			_ = instance.QMPClient.Conn.Close()
			instance.QMPClient = nil
		}
		return fmt.Errorf("failed to subscribe to NATS: %w", err)
	}
	d.natsSubscriptions[instance.ID] = sub

	consoleSub, err := d.natsConn.Subscribe(fmt.Sprintf("ec2.%s.GetConsoleOutput", instance.ID), d.handleEC2GetConsoleOutput)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("failed to subscribe to console output NATS: %w", err)
	}
	d.natsSubscriptions[instance.ID+".console"] = consoleSub
	d.mu.Unlock()

	instance.Status = vm.StateRunning

	if err := d.WriteState(); err != nil {
		return fmt.Errorf("failed to persist reconnected instance state: %w", err)
	}

	slog.Info("Successfully reconnected to running instance", "instance", instance.ID)
	return nil
}

// awaitShutdown blocks until the daemon's shutdown wait group completes.
func (d *Daemon) awaitShutdown() {
	done := make(chan struct{})
	go func() {
		d.shutdownWg.Wait()
		close(done)
	}()
	<-done
}

// computeConfigHash computes SHA256 hash of the shared cluster config (excluding node-specific fields)
func (d *Daemon) computeConfigHash() (string, error) {
	// Only hash the shared cluster data, not the node-specific top-level field
	sharedData := types.SharedClusterData{
		Epoch:   d.clusterConfig.Epoch,
		Version: d.clusterConfig.Version,
		Nodes:   d.clusterConfig.Nodes,
	}

	configJSON, err := json.Marshal(sharedData)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(configJSON)
	return hex.EncodeToString(hash[:]), nil
}

// ClusterManager starts the HTTP cluster management server
func (d *Daemon) ClusterManager() error {
	// Get daemon host from config
	daemonHost := d.config.Daemon.Host
	if daemonHost == "" {
		return fmt.Errorf("daemon.host not configured")
	}

	r := chi.NewRouter()

	// Health endpoint - responds to HTTP and NATS
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		configHash, err := d.computeConfigHash()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"failed to compute config hash"}`))
			return
		}

		serviceHealth := make(map[string]string)
		for _, svc := range d.config.GetServices() {
			serviceHealth[svc] = "ok"
		}
		// For remote dependencies, check connectivity
		if !d.config.HasService("nats") {
			if d.natsConn != nil && d.natsConn.IsConnected() {
				serviceHealth["nats"] = "remote_ok"
			} else {
				serviceHealth["nats"] = "remote_unreachable"
			}
		}

		// Check OVN networking readiness
		ovnHealth := CheckOVNHealth()
		if ovnHealth.BrIntExists {
			serviceHealth["br-int"] = "ok"
		} else {
			serviceHealth["br-int"] = "missing"
		}
		if ovnHealth.OVNControllerUp {
			serviceHealth["ovn-controller"] = "ok"
		} else {
			serviceHealth["ovn-controller"] = "not_running"
		}

		status := "running"
		if !d.ready.Load() {
			status = "starting"
		}

		response := types.NodeHealthResponse{
			Node:          d.node,
			Status:        status,
			ConfigHash:    configHash,
			Epoch:         d.clusterConfig.Epoch,
			Uptime:        int64(time.Since(d.startTime).Seconds()),
			Services:      d.config.GetServices(),
			ServiceHealth: serviceHealth,
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			slog.Error("Failed to encode health response", "error", err)
		}
	})

	// Load TLS certificate (C-5: serve over HTTPS instead of plaintext HTTP)
	// Resolve relative cert paths against config directory (cert lives alongside spinifex.toml).
	// For binary installs, systemd sets absolute paths via env vars; for dev, the config
	// stores relative paths like "config/server.pem" which need resolution.
	tlsCert := d.config.Daemon.TLSCert
	tlsKey := d.config.Daemon.TLSKey
	if tlsCert == "" || tlsKey == "" {
		return fmt.Errorf("cluster manager TLS not configured: set daemon.tlscert and daemon.tlskey in config")
	}
	if d.configPath != "" {
		configDir := filepath.Dir(d.configPath)
		if !filepath.IsAbs(tlsCert) {
			tlsCert = filepath.Join(configDir, filepath.Base(tlsCert))
		}
		if !filepath.IsAbs(tlsKey) {
			tlsKey = filepath.Join(configDir, filepath.Base(tlsKey))
		}
	}
	cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	if err != nil {
		return fmt.Errorf("cluster manager load TLS cert: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	d.clusterServer = &http.Server{
		Addr:              daemonHost,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConfig,
	}

	ln, err := tls.Listen("tcp", daemonHost, tlsConfig)
	if err != nil {
		return fmt.Errorf("cluster manager listen on %s: %w", daemonHost, err)
	}

	go func() {
		slog.Info("Starting cluster manager (TLS)", "host", daemonHost)
		if err := d.clusterServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("Cluster manager failed", "error", err)
		}
	}()

	return nil
}

// WriteState writes the instance state to JetStream KV store (required).
// It acquires d.Instances.Mu internally.
func (d *Daemon) WriteState() error {
	if d.jsManager == nil {
		return fmt.Errorf("JetStream manager not initialized - cannot write state")
	}
	if err := d.jsManager.WriteState(d.node, &d.Instances); err != nil {
		slog.Error("JetStream write failed", "error", err)
		return fmt.Errorf("failed to write state to JetStream: %w", err)
	}
	return nil
}

// LoadState loads the instance state from JetStream KV store (required)
func (d *Daemon) LoadState() error {
	if d.jsManager == nil {
		return fmt.Errorf("JetStream manager not initialized - cannot load state")
	}

	instances, err := d.jsManager.LoadState(d.node)
	if err != nil {
		slog.Error("JetStream load failed", "error", err)
		return fmt.Errorf("failed to load state from JetStream: %w", err)
	}

	// Copy only the VMS map, not the mutex
	d.Instances.VMS = instances.VMS
	return nil
}

func (d *Daemon) SendQMPCommand(q *qmp.QMPClient, cmd qmp.QMPCommand, instanceId string) (*qmp.QMPResponse, error) {
	// Confirm QMP client is initialized
	if q == nil || q.Encoder == nil || q.Decoder == nil {
		return nil, fmt.Errorf("QMP client is not initialized")
	}

	// Lock the QMP client
	q.Mu.Lock()
	defer q.Mu.Unlock()

	// Set a read deadline so we don't block forever on a hung QEMU process
	if err := q.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = q.Conn.SetReadDeadline(time.Time{}) }() // clear deadline after command

	if err := q.Encoder.Encode(cmd); err != nil {
		return nil, fmt.Errorf("encode error: %w", err)
	}

	for {
		var msg map[string]any
		if err := q.Decoder.Decode(&msg); err != nil {
			return nil, fmt.Errorf("decode error: %w", err)
		}

		if _, ok := msg["event"]; ok {
			// QMP events are informational only — state transitions are driven
			// by the command handlers that initiate the action, avoiding races
			// between event-driven and command-driven transitions.
			slog.Info("QMP event", "event", msg["event"], "instanceId", instanceId)
			// Extend deadline after receiving an event (QEMU is alive, just chatty)
			if err := q.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				return nil, fmt.Errorf("set read deadline: %w", err)
			}
			continue
		}
		if errObj, ok := msg["error"].(map[string]any); ok {
			return nil, fmt.Errorf("QMP error: %s: %s", errObj["class"], errObj["desc"])
		}
		if _, ok := msg["return"]; ok {
			respBytes, err := json.Marshal(msg)
			if err != nil {
				return nil, fmt.Errorf("marshal QMP response: %w", err)
			}
			var resp qmp.QMPResponse
			if err := json.Unmarshal(respBytes, &resp); err != nil {
				return nil, fmt.Errorf("unmarshal error: %w", err)
			}
			return &resp, nil
		}
	}
}

func (d *Daemon) stopInstance(instances map[string]*vm.VM, deleteVolume bool) error {
	// Signal to shutdown each VM
	var wg sync.WaitGroup

	// Run asynchronously within a worker group
	for _, instance := range instances {
		wg.Go(func() {
			// Send shutdown command - if it fails, VM may already be dead, continue with cleanup
			_, err := d.SendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "system_powerdown"}, instance.ID)
			if err != nil {
				slog.Warn("QMP system_powerdown failed (VM may already be stopped)", "id", instance.ID, "err", err)
				// Don't return - continue with cleanup
			}

			// Wait for PID file removal (or check if already gone).
			// 20s is enough for a graceful ACPI shutdown — if the guest hasn't
			// responded to system_powerdown by then, it won't (e.g. still booting,
			// no ACPI handler). Force-kill at that point rather than wasting 60s.
			err = utils.WaitForPidFileRemoval(instance.ID, 20*time.Second)
			if err != nil {
				slog.Warn("Timeout waiting for PID file removal", "id", instance.ID, "err", err)

				// Try force killing the process if it's still running
				pid, readErr := utils.ReadPidFile(instance.ID)
				if readErr != nil {
					slog.Debug("No PID file found (VM likely already stopped)", "id", instance.ID)
				} else {
					slog.Info("Force killing process", "pid", pid, "id", instance.ID)
					if err := utils.KillProcess(pid); err != nil {
						slog.Error("Failed to kill process", "pid", pid, "id", instance.ID, "err", err)
					}
				}
			}

			// Unmount all EBS volumes
			instance.EBSRequests.Mu.Lock()
			defer instance.EBSRequests.Mu.Unlock()

			for _, ebsRequest := range instance.EBSRequests.Requests {
				// Send the volume payload as JSON
				ebsUnMountRequest, err := json.Marshal(ebsRequest)

				if err != nil {
					slog.Error("Failed to marshal volume payload", "err", err)
					continue
				}

				msg, err := d.natsConn.Request(d.ebsTopic("unmount"), ebsUnMountRequest, 30*time.Second)
				if err != nil {
					slog.Error("Failed to unmount volume", "name", ebsRequest.Name, "id", instance.ID, "err", err)
				} else {
					slog.Info("Unmounted Viperblock volume", "id", instance.ID, "data", string(msg.Data))
				}

				// Update volume state to "available" for all user-visible volumes (boot + hot-attached)
				if !ebsRequest.EFI && !ebsRequest.CloudInit {
					if err := d.volumeService.UpdateVolumeState(ebsRequest.Name, "available", "", ""); err != nil {
						slog.Error("Failed to update volume state to available", "volumeId", ebsRequest.Name, "err", err)
					}
				}
			}

			// If flagged for termination, clean up volumes
			if deleteVolume {
				for _, ebsRequest := range instance.EBSRequests.Requests {
					// Internal volumes (EFI, cloud-init) are always cleaned up via ebs.delete
					// to stop viperblockd processes. S3 data cleanup happens via DeleteVolume
					// on the parent root volume (which deletes -efi/ and -cloudinit/ prefixes).
					if ebsRequest.EFI || ebsRequest.CloudInit {
						ebsDeleteData, err := json.Marshal(types.EBSDeleteRequest{Volume: ebsRequest.Name})
						if err != nil {
							slog.Error("Failed to marshal ebs.delete request for internal volume", "name", ebsRequest.Name, "err", err)
							continue
						}
						deleteMsg, err := d.natsConn.Request("ebs.delete", ebsDeleteData, 30*time.Second)
						if err != nil {
							slog.Warn("Failed to send ebs.delete for internal volume", "name", ebsRequest.Name, "id", instance.ID, "err", err)
						} else {
							slog.Info("Sent ebs.delete for internal volume", "name", ebsRequest.Name, "id", instance.ID, "data", string(deleteMsg.Data))
						}
						continue
					}

					// User-visible volumes: respect DeleteOnTermination flag
					if !ebsRequest.DeleteOnTermination {
						slog.Info("Volume has DeleteOnTermination=false, skipping deletion", "name", ebsRequest.Name, "id", instance.ID)
						continue
					}

					// DeleteVolume handles: NATS ebs.delete notification + S3 cleanup
					// (including -efi/ and -cloudinit/ sub-prefixes)
					slog.Info("Deleting volume with DeleteOnTermination=true", "name", ebsRequest.Name, "id", instance.ID)
					_, err := d.volumeService.DeleteVolume(&ec2.DeleteVolumeInput{
						VolumeId: &ebsRequest.Name,
					}, instance.AccountID)
					if err != nil {
						slog.Error("Failed to delete volume on termination", "name", ebsRequest.Name, "id", instance.ID, "err", err)
					} else {
						slog.Info("Deleted volume on termination", "name", ebsRequest.Name, "id", instance.ID)
					}
				}
			}

			// Clean up VPC tap device if present
			if instance.ENIId != "" && d.networkPlumber != nil {
				if err := d.networkPlumber.CleanupTapDevice(instance.ENIId); err != nil {
					slog.Warn("Failed to clean up tap device", "eni", instance.ENIId, "err", err)
				}
				// Clean up any extra ENI tap devices (multi-subnet ALB VMs).
				d.cleanupExtraENITaps(instance)
			}

			// Clean up management TAP and release IP
			if instance.MgmtTap != "" {
				if err := CleanupMgmtTapDevice(instance.MgmtTap); err != nil {
					slog.Warn("Failed to clean up mgmt tap device", "tap", instance.MgmtTap, "instanceId", instance.ID, "err", err)
				}
				if d.mgmtIPAllocator != nil {
					d.mgmtIPAllocator.Release(instance.ID)
				}
			}

			// Release public IP before deleting ENI
			if deleteVolume && instance.PublicIP != "" && instance.PublicIPPool != "" && d.externalIPAM != nil {
				// Publish vpc.delete-nat to remove dnat_and_snat rule
				portName := "port-" + instance.ENIId
				vpcId := ""
				logicalIP := ""
				if instance.Instance != nil {
					if instance.Instance.VpcId != nil {
						vpcId = *instance.Instance.VpcId
					}
					if instance.Instance.PrivateIpAddress != nil {
						logicalIP = *instance.Instance.PrivateIpAddress
					}
				}
				d.publishNATEvent("vpc.delete-nat", vpcId, instance.PublicIP, logicalIP, portName, "")

				// Release IP back to pool
				if err := d.externalIPAM.ReleaseIP(instance.PublicIPPool, instance.PublicIP); err != nil {
					slog.Warn("Failed to release public IP on termination", "ip", instance.PublicIP, "pool", instance.PublicIPPool, "err", err)
				} else {
					slog.Info("Released public IP on termination", "ip", instance.PublicIP, "instanceId", instance.ID)
				}
			}

			// On termination, detach and delete the auto-created ENI (releases IP
			// back to IPAM, publishes vpc.delete-port for vpcd). On stop, ENI
			// persists (AWS behavior). Must detach first to clear in-use status.
			// Tolerate NotFound — ENI may have been cleaned up already.
			// Other errors (KV failures, permission issues, in-use) are real
			// failures that could leak IPAM addresses.
			if deleteVolume && instance.ENIId != "" && d.vpcService != nil {
				if detachErr := d.vpcService.DetachENI(instance.AccountID, instance.ENIId); detachErr != nil {
					slog.Warn("Failed to detach ENI on termination", "eni", instance.ENIId, "instanceId", instance.ID, "err", detachErr)
				}
				if _, eniErr := d.vpcService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
					NetworkInterfaceId: &instance.ENIId,
				}, instance.AccountID); eniErr != nil {
					if strings.Contains(eniErr.Error(), awserrors.ErrorInvalidNetworkInterfaceIDNotFound) {
						slog.Debug("ENI already cleaned up on termination", "eni", instance.ENIId)
					} else {
						slog.Error("Failed to delete ENI on termination", "eni", instance.ENIId, "instanceId", instance.ID, "err", eniErr)
					}
				} else {
					slog.Info("Deleted ENI on termination", "eni", instance.ENIId, "instanceId", instance.ID)
				}
				// Extra ENIs (multi-subnet ALB VMs) are cleaned up by
				// DeleteLoadBalancer via its own lb.ENIs loop — the daemon
				// doesn't duplicate that teardown here.
			}

			// Deallocate resources
			instanceType := d.resourceMgr.instanceTypes[instance.InstanceType]
			if instanceType != nil {
				slog.Info("Deallocating resources for stopped instance", "instanceId", instance.ID, "type", instance.InstanceType)
				d.resourceMgr.deallocate(instanceType)
			}
		})
	}

	// Wait for all shutdowns to finish
	wg.Wait()

	// Only unsubscribe from NATS subjects when terminating (deleteVolume=true)
	// For stop operations, keep the subscription so we can receive start commands
	if deleteVolume {
		for _, instance := range instances {
			d.mu.Lock()
			if sub, ok := d.natsSubscriptions[instance.ID]; ok {
				slog.Info("Unsubscribing from NATS subject", "instance", instance.ID)
				if err := sub.Unsubscribe(); err != nil {
					slog.Error("Failed to unsubscribe from NATS subject", "instance", instance.ID, "err", err)
				}
				delete(d.natsSubscriptions, instance.ID)
			}
			consoleSubKey := instance.ID + ".console"
			if sub, ok := d.natsSubscriptions[consoleSubKey]; ok {
				if err := sub.Unsubscribe(); err != nil {
					slog.Error("Failed to unsubscribe from console NATS subject", "instance", instance.ID, "err", err)
				}
				delete(d.natsSubscriptions, consoleSubKey)
			}
			d.mu.Unlock()
		}
	}
	return nil
}

func (d *Daemon) setupShutdown() {
	d.shutdownWg.Go(func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

		<-sigChan
		slog.Info("Received shutdown signal, cleaning up...")

		// Cancel context to stop heartbeat and other goroutines
		d.cancel()

		// If coordinated shutdown already handled VMs (DRAIN phase), skip stopInstance.
		// Otherwise, set the flag now so crash handlers and restart schedulers
		// know to bail out during SIGTERM-based shutdown.
		if d.shuttingDown.Load() {
			slog.Info("Coordinated shutdown in progress, skipping VM stop (already handled by DRAIN phase)")
		} else {
			d.shuttingDown.Store(true)
			// Pass instances to terminate
			if err := d.stopInstance(d.Instances.VMS, false); err != nil {
				slog.Error("Failed to stop instances during shutdown", "err", err)
			}
		}

		// Stop ELBv2 background goroutines
		if d.elbv2Service != nil {
			d.elbv2Service.Close()
		}

		// Final cleanup
		for _, sub := range d.natsSubscriptions {
			// Unsubscribe from each subscription
			slog.Info("Unsubscribing from NATS", "subject", sub.Subject)
			if err := sub.Unsubscribe(); err != nil {
				slog.Error("Error unsubscribing from NATS", "err", err)
			}
		}

		// Write shutdown marker to cluster state KV
		if d.jsManager != nil {
			if err := d.jsManager.WriteShutdownMarker(d.node); err != nil {
				slog.Error("Failed to write shutdown marker", "err", err)
			}
		}

		// Write state to JetStream before closing NATS connection
		err := d.WriteState()
		if err != nil {
			slog.Error("Failed to write state", "err", err)
		}

		// Close NATS connection
		d.natsConn.Close()

		// Shutdown cluster manager
		if d.clusterServer != nil {
			slog.Info("Shutting down cluster manager...")
			if err := d.clusterServer.Shutdown(context.Background()); err != nil {
				slog.Error("Error shutting down cluster manager", "err", err)
			}
		}

		slog.Info("Shutdown complete")
	})
}

func (d *Daemon) CreateQMPClient(instance *vm.VM) (err error) {
	// Create a new QMP client to communicate with the instance
	instance.QMPClient, err = qmp.NewQMPClient(instance.Config.QMPSocket)

	if err != nil {
		slog.Error("Could not connect to QMP")
		return err
	}

	// Send qmp_capabilities handshake to init
	_, err = d.SendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "qmp_capabilities"}, instance.ID)
	if err != nil {
		slog.Error("Failed QMP capabilities handshake", "err", err)
		return err
	}

	// Simple heartbeat to confirm QMP and the instance is running / healthy
	go func() {
		for {
			time.Sleep(30 * time.Second)

			// Check if instance is in a terminal or transitional state - exit heartbeat
			d.Instances.Mu.Lock()
			status := instance.Status
			d.Instances.Mu.Unlock()

			if status == vm.StateStopping || status == vm.StateStopped || status == vm.StateShuttingDown || status == vm.StateTerminated || status == vm.StateError {
				slog.Info("QMP heartbeat exiting - instance not running", "instance", instance.ID, "status", status)

				// Close the QMP client connection if it exists
				if instance.QMPClient != nil && instance.QMPClient.Conn != nil {
					if err := instance.QMPClient.Conn.Close(); err != nil {
						slog.Error("Failed to close QMP connection", "instance", instance.ID, "err", err)
					}
				}
				return
			}

			slog.Debug("QMP heartbeat", "instance", instance.ID)
			qmpStatus, err := d.SendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "query-status"}, instance.ID)

			if err != nil {
				slog.Warn("QMP heartbeat failed", "instance", instance.ID, "err", err)
				// Don't exit on transient errors - let the status check above handle terminal states
				continue
			}

			slog.Debug("QMP status", "instance", instance.ID, "status", string(qmpStatus.Return))
		}
	}()

	return nil
}

func (d *Daemon) LaunchInstance(instance *vm.VM) (err error) {
	// Abort if instance is no longer in a launchable state (e.g., terminated
	// by a concurrent request while waiting in the launch queue).
	d.Instances.Mu.Lock()
	status := instance.Status
	d.Instances.Mu.Unlock()
	if status != vm.StatePending && status != vm.StateStopped && status != vm.StateProvisioning {
		return fmt.Errorf("instance %s in %s state, not launchable", instance.ID, status)
	}

	// First, confirm if the instance is already running
	pid, _ := utils.ReadPidFile(instance.ID)

	if pid > 0 {
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}

		// Send a 0 signal to confirm process is running
		err = process.Signal(syscall.Signal(0))
		if err == nil {
			slog.Error("Instance is already running", "InstanceID", instance.ID, "pid", pid)
			return errors.New("instance is already running")
		}
	}

	// Loop through each volume in volumes
	err = d.MountVolumes(instance)

	if err != nil {
		slog.Error("Failed to mount volumes", "err", err)
		return err
	}

	// Step 6: Launch the instance via QEMU/KVM
	err = d.StartInstance(instance)

	if err != nil {
		slog.Error("Failed to launch instance", "err", err)
		return err
	}

	// Step 7: Create QMP client to communicate with the instance
	err = d.CreateQMPClient(instance)

	if err != nil {
		slog.Error("Failed to create QMP client", "err", err)
		return err
	}

	// Step 8: Subscribe to start/stop/shutdown events
	d.mu.Lock()
	defer d.mu.Unlock()

	// Unsubscribe any existing subscriptions (e.g. from restoreInstances for stopped instances)
	if existing, ok := d.natsSubscriptions[instance.ID]; ok {
		_ = existing.Unsubscribe()
	}
	consoleSubKey := instance.ID + ".console"
	if existing, ok := d.natsSubscriptions[consoleSubKey]; ok {
		_ = existing.Unsubscribe()
	}

	d.natsSubscriptions[instance.ID], err = d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
	if err != nil {
		slog.Error("failed to subscribe to NATS", "err", err)
		return err
	}

	d.natsSubscriptions[consoleSubKey], err = d.natsConn.Subscribe(fmt.Sprintf("ec2.%s.GetConsoleOutput", instance.ID), d.handleEC2GetConsoleOutput)
	if err != nil {
		slog.Error("failed to subscribe to console output NATS topic", "err", err)
		return err
	}

	// Step 9: Update the instance metadata for running state and volume attached
	d.Instances.Mu.Lock()
	d.Instances.VMS[instance.ID] = instance
	d.Instances.Mu.Unlock()

	if err := d.TransitionState(instance, vm.StateRunning); err != nil {
		slog.Error("Failed to transition instance to running", "instanceId", instance.ID, "err", err)
		return err
	}

	// Step 10: Mark boot volumes as "in-use" now that instance is confirmed running
	instance.EBSRequests.Mu.Lock()
	for _, ebsReq := range instance.EBSRequests.Requests {
		if ebsReq.Boot {
			if err := d.volumeService.UpdateVolumeState(ebsReq.Name, "in-use", instance.ID, ""); err != nil {
				slog.Error("Failed to update volume state to in-use", "volumeId", ebsReq.Name, "err", err)
			}
		}
	}
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// markInstanceFailed updates an instance status to indicate a failure during launch,
// then completes the termination lifecycle in the background so the instance
// reaches terminated state and doesn't get stuck in shutting-down.
func (d *Daemon) markInstanceFailed(instance *vm.VM, reason string) {
	// If the instance is already being cleaned up (e.g., a concurrent terminate
	// request transitioned it to shutting-down while LaunchInstance was running),
	// don't spawn a second finalizeTermination goroutine — the existing cleanup
	// handler owns the lifecycle from here.
	d.Instances.Mu.Lock()
	if instance.Status == vm.StateShuttingDown || instance.Status == vm.StateTerminated {
		d.Instances.Mu.Unlock()
		slog.Info("markInstanceFailed: instance already in cleanup state, skipping",
			"instanceId", instance.ID, "status", string(instance.Status), "reason", reason)
		return
	}

	// Set state reason before transition
	if instance.Instance != nil {
		instance.Instance.StateReason = &ec2.StateReason{}
		instance.Instance.StateReason.SetCode("Server.InternalError")
		instance.Instance.StateReason.SetMessage(reason)
	}
	d.Instances.Mu.Unlock()

	if err := d.TransitionState(instance, vm.StateShuttingDown); err != nil {
		slog.Error("markInstanceFailed transition failed", "instanceId", instance.ID, "err", err)
		// If the error was a write failure, the in-memory state is already
		// shutting-down. Still proceed with finalization to avoid getting stuck.
		if instance.Status != vm.StateShuttingDown {
			return
		}
	}

	slog.Info("Instance marked as failed", "instanceId", instance.ID, "reason", reason)

	// Complete termination in the background — clean up any partially-created
	// resources and transition to terminated so the instance doesn't get stuck
	// in shutting-down indefinitely.
	go d.finalizeTermination(instance)
}

// finalizeTermination completes the termination lifecycle for an instance already
// in shutting-down state. It cleans up resources (processes, volumes, ENIs),
// transitions to terminated, writes to the terminated KV bucket, and removes
// the instance from local state.
func (d *Daemon) finalizeTermination(instance *vm.VM) {
	stopErr := d.stopInstance(map[string]*vm.VM{instance.ID: instance}, true)
	if stopErr != nil {
		slog.Error("Failed to cleanup failed instance", "err", stopErr, "id", instance.ID)
		if err := d.TransitionState(instance, vm.StateError); err != nil {
			slog.Error("Failed to transition to error state", "instanceId", instance.ID, "err", err)
		}
		return
	}

	d.Instances.Mu.Lock()
	instance.LastNode = d.node
	d.Instances.Mu.Unlock()

	if err := d.TransitionState(instance, vm.StateTerminated); err != nil {
		slog.Error("Failed to transition failed instance to terminated", "instanceId", instance.ID, "err", err)
		return
	}
	slog.Info("Instance terminated (failed launch cleanup)", "id", instance.ID)

	if d.jsManager != nil {
		if err := d.jsManager.WriteTerminatedInstance(instance.ID, instance); err != nil {
			slog.Error("Failed to write terminated instance to KV, keeping in local state for retry",
				"instanceId", instance.ID, "err", err)
			return
		}
	}

	// Guard + delete: another handler may have reclaimed this instance.
	d.Instances.Mu.Lock()
	current, exists := d.Instances.VMS[instance.ID]
	if !exists || current != instance {
		d.Instances.Mu.Unlock()
		slog.Info("Instance was reclaimed by another handler, skipping local cleanup",
			"instanceId", instance.ID, "state", "terminated")
		return
	}
	delete(d.Instances.VMS, instance.ID)
	d.Instances.Mu.Unlock()

	if err := d.WriteState(); err != nil {
		slog.Error("Failed to persist state after terminating failed instance, re-adding to local map",
			"instanceId", instance.ID, "err", err)
		d.Instances.Mu.Lock()
		if _, occupied := d.Instances.VMS[instance.ID]; !occupied {
			d.Instances.VMS[instance.ID] = instance
		}
		d.Instances.Mu.Unlock()
	} else {
		slog.Info("Released failed instance ownership to KV",
			"instanceId", instance.ID, "lastNode", d.node)
	}
}

const pendingWatchdogInterval = 60 * time.Second
const pendingWatchdogTimeout = 5 * time.Minute

// startPendingWatchdog runs a background goroutine that periodically checks for
// instances stuck in pending/provisioning beyond a timeout and marks them failed.
func (d *Daemon) startPendingWatchdog() {
	ticker := time.NewTicker(pendingWatchdogInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
				d.Instances.Mu.Lock()
				var stuck []*vm.VM
				for _, instance := range d.Instances.VMS {
					if (instance.Status == vm.StatePending || instance.Status == vm.StateProvisioning) &&
						instance.Instance != nil && instance.Instance.LaunchTime != nil &&
						time.Since(*instance.Instance.LaunchTime) > pendingWatchdogTimeout {
						stuck = append(stuck, instance)
					}
				}
				d.Instances.Mu.Unlock()

				for _, instance := range stuck {
					slog.Warn("Instance stuck in pending, marking failed",
						"instanceId", instance.ID, "status", instance.Status,
						"elapsed", time.Since(*instance.Instance.LaunchTime))
					d.markInstanceFailed(instance, "launch_timeout")
				}
			}
		}
	}()
}

func (d *Daemon) StartInstance(instance *vm.VM) error {
	pidFile, err := utils.GeneratePidFile(instance.ID)

	if err != nil {
		slog.Error("Failed to generate PID file", "err", err)
		return err
	}

	instanceType := d.resourceMgr.instanceTypes[instance.InstanceType]
	if instanceType == nil {
		return fmt.Errorf("instance type %s not found", instance.InstanceType)
	}

	vCPUs := int(instanceTypeVCPUs(instanceType))
	memoryMiB := instanceTypeMemoryMiB(instanceType)
	architecture := "x86_64"
	if instanceType.ProcessorInfo != nil && len(instanceType.ProcessorInfo.SupportedArchitectures) > 0 && instanceType.ProcessorInfo.SupportedArchitectures[0] != nil {
		architecture = *instanceType.ProcessorInfo.SupportedArchitectures[0]
	}

	// Console log + serial socket paths (serial output capture + admin access via socat)
	runtimeDir := utils.RuntimeDir()
	consoleLogPath := filepath.Join(runtimeDir, fmt.Sprintf("console-%s.log", instance.ID))
	serialSocket := filepath.Join(runtimeDir, fmt.Sprintf("serial-%s.sock", instance.ID))

	instance.Config = buildBaseVMConfig(instance.ID, pidFile, consoleLogPath, serialSocket, architecture, vCPUs, int(memoryMiB))

	// Build QEMU drives from EBS volume requests.
	instance.EBSRequests.Mu.Lock()
	drives, iothreads, devices, err := buildDrives(instance.EBSRequests.Requests, vCPUs)
	instance.EBSRequests.Mu.Unlock()
	if err != nil {
		return err
	}
	instance.Config.Drives = append(instance.Config.Drives, drives...)
	instance.Config.IOThreads = append(instance.Config.IOThreads, iothreads...)
	instance.Config.Devices = append(instance.Config.Devices, devices...)

	// VPC tap networking vs user-mode fallback
	if instance.ENIId != "" && d.networkPlumber != nil {
		// VPC mode: create tap device and add to OVS br-int
		if err := d.networkPlumber.SetupTapDevice(instance.ENIId, instance.ENIMac); err != nil {
			slog.Error("Failed to set up tap device", "eni", instance.ENIId, "err", err)
			return fmt.Errorf("setup tap device: %w", err)
		}

		tapName := TapDeviceName(instance.ENIId)
		instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
			Value: fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", tapName),
		})
		instance.Config.Devices = append(instance.Config.Devices, vm.Device{
			Value: fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", instance.ENIMac),
		})

		slog.Info("VPC networking configured", "tap", tapName, "eni", instance.ENIId, "mac", instance.ENIMac)

		// Additional VPC NICs for multi-subnet system VMs (e.g. ALBs with
		// subnets across multiple AZs).
		if err := d.setupExtraENINICs(instance); err != nil {
			return err
		}

		// DEV_NETWORKING: add a second NIC with hostfwd for SSH dev access
		if d.config.Daemon.DevNetworking {
			sshDebugAddr, err := viperblock.FindFreePort()
			if err != nil {
				slog.Warn("DEV_NETWORKING: failed to find free port for dev NIC", "err", err)
			} else {
				_, sshDebugPort, err := net.SplitHostPort(sshDebugAddr)
				if err != nil {
					slog.Warn("DEV_NETWORKING: failed to parse port from address", "addr", sshDebugAddr, "err", err)
				} else {
					bindIP := d.config.Host
					if bindIP == "" || bindIP == "0.0.0.0" {
						bindIP = "127.0.0.1"
					}
					netdevVal := fmt.Sprintf("user,id=dev0,hostfwd=tcp:%s:%s-:22", bindIP, sshDebugPort)
					// Add extra hostfwd rules for system instances (e.g. ALB VMs forwarding HTTP ports)
					if instance.ExtraHostfwd != nil {
						for guestPort := range instance.ExtraHostfwd {
							fwdAddr, fwdErr := viperblock.FindFreePort()
							if fwdErr != nil {
								slog.Warn("DEV_NETWORKING: failed to find free port for extra hostfwd", "guestPort", guestPort, "err", fwdErr)
								continue
							}
							_, hostPort, splitErr := net.SplitHostPort(fwdAddr)
							if splitErr != nil {
								slog.Warn("DEV_NETWORKING: failed to parse extra hostfwd address", "fwdAddr", fwdAddr, "err", splitErr)
								continue
							}
							hostPortInt, convErr := strconv.Atoi(hostPort)
							if convErr != nil {
								slog.Warn("DEV_NETWORKING: failed to convert extra hostfwd port", "hostPort", hostPort, "err", convErr)
								continue
							}
							netdevVal += fmt.Sprintf(",hostfwd=tcp:%s:%s-:%d", bindIP, hostPort, guestPort)
							instance.ExtraHostfwd[guestPort] = hostPortInt
							slog.Info("DEV_NETWORKING: extra hostfwd", "guestPort", guestPort, "hostPort", hostPort, "instanceId", instance.ID)
						}
					}
					instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
						Value: netdevVal,
					})
					devMac := generateDevMAC(instance.ID)
					instance.Config.Devices = append(instance.Config.Devices, vm.Device{
						Value: fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s", devMac),
					})
					slog.Info("DEV_NETWORKING: added dev NIC with SSH hostfwd",
						"bindIP", bindIP, "port", sshDebugPort, "mac", devMac, "instanceId", instance.ID)
				}
			}
		}
	} else {
		// Non-VPC fallback: user-mode networking with SSH port forwarding
		sshDebugAddr, err := viperblock.FindFreePort()
		if err != nil {
			slog.Error("Failed to find free port", "err", err)
			return err
		}
		_, sshDebugPort, err := net.SplitHostPort(sshDebugAddr)
		if err != nil {
			slog.Error("Failed to parse port from address", "addr", sshDebugAddr, "err", err)
			return fmt.Errorf("parse port from %s: %w", sshDebugAddr, err)
		}

		bindIP := d.config.Host
		if bindIP == "" || bindIP == "0.0.0.0" {
			bindIP = "127.0.0.1"
		}
		instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
			Value: fmt.Sprintf("user,id=net0,hostfwd=tcp:%s:%s-:22", bindIP, sshDebugPort),
		})
		instance.Config.Devices = append(instance.Config.Devices, vm.Device{
			Value: "virtio-net-pci,netdev=net0",
		})
	}

	// Management NIC: system instances get a TAP on br-mgmt for control plane traffic.
	if instance.MgmtMAC != "" && instance.MgmtTap != "" {
		instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
			Value: fmt.Sprintf("tap,id=mgmt0,ifname=%s,script=no,downscript=no", instance.MgmtTap),
		})
		instance.Config.Devices = append(instance.Config.Devices, vm.Device{
			Value: fmt.Sprintf("virtio-net-pci,netdev=mgmt0,mac=%s", instance.MgmtMAC),
		})
		slog.Info("Management NIC configured", "tap", instance.MgmtTap, "mac", instance.MgmtMAC, "ip", instance.MgmtIP, "instanceId", instance.ID)
	}

	instance.Config.Devices = append(instance.Config.Devices, vm.Device{
		Value: "virtio-rng-pci",
	})

	// QMP socket
	qmpSocket, err := utils.GenerateSocketFile(fmt.Sprintf("qmp-%s", instance.ID))

	if err != nil {
		slog.Error("Failed to generate QMP socket", "err", err)
		return err
	}

	instance.Config.QMPSocket = qmpSocket

	// Temp, wait for nbdkit to start
	// TODO: Improve, confirm nbdkit started for each volume
	time.Sleep(2 * time.Second)

	// Create a unique error channel for this specific mount request
	processChan := make(chan int, 1)
	exitChan := make(chan int, 1)
	startupConfirmed := make(chan bool, 1)

	go func() {
		cmd, err := instance.Config.Execute()

		if err != nil {
			slog.Error("Failed to execute VM", "err", err)
			processChan <- 0
			return
		}

		VMstdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("Failed to pipe STDERR VM", "err", err)
			processChan <- 0
			return
		}

		VMstderr, err := cmd.StderrPipe()
		if err != nil {
			slog.Error("Failed to pipe STDERR VM", "err", err)
			processChan <- 0
			return
		}

		err = cmd.Start()

		if err != nil {
			slog.Error("Failed to start VM", "err", err)
			processChan <- 0
			return
		}

		slog.Info("VM started successfully", "pid", cmd.Process.Pid)

		// Set OOM score for QEMU process (prefer killing VMs over system services)
		if err := utils.SetOOMScore(cmd.Process.Pid, 500); err != nil {
			slog.Warn("Failed to set QEMU OOM score", "pid", cmd.Process.Pid, "err", err)
		}

		// Log QEMU stdout (serial output is captured via chardev logfile, not stdout)
		go func() {
			scanner := bufio.NewScanner(VMstdout)
			for scanner.Scan() {
				slog.Info("[qemu]", "line", scanner.Text())
			}
		}()

		// --- reader for STDERR ---
		go func() {
			scanner := bufio.NewScanner(VMstderr)
			slog.Info("QEMU stderr reader started")

			for scanner.Scan() {
				line := scanner.Text()
				slog.Error("[qemu-stderr]", "line", line)
			}
		}()

		processChan <- cmd.Process.Pid

		// Block until QEMU exits
		waitErr := cmd.Wait()

		if waitErr != nil {
			slog.Error("VM process exited", "instance", instance.ID, "err", waitErr)
		}

		// Signal startup check (non-blocking)
		select {
		case exitChan <- 1:
		default:
		}

		// Wait for startup phase to complete before deciding on crash handling
		confirmed := <-startupConfirmed
		if !confirmed {
			return // Startup failed, LaunchInstance handles the error
		}

		// Handle exit: crash vs clean shutdown
		if waitErr != nil {
			d.handleInstanceCrash(instance, waitErr)
		} else {
			slog.Info("VM process exited cleanly", "instance", instance.ID)
		}
	}()

	// Wait for startup result
	pid := <-processChan

	if pid == 0 {
		return fmt.Errorf("failed to start qemu")
	}

	// Wait for 1 second to confirm nbdkit is running
	time.Sleep(1 * time.Second)

	// Check if QEMU exited immediately with an error
	select {
	case exitErr := <-exitChan:
		startupConfirmed <- false // tell goroutine not to handle crash
		if exitErr != 0 {
			errorMsg := fmt.Errorf("failed: %v", exitErr)
			slog.Error("Failed to launch qemu", "err", errorMsg)
			return errorMsg
		}
	default:
		startupConfirmed <- true // goroutine will handle future crashes
		slog.Info("QEMU started successfully and is running",
			"console_log", instance.Config.ConsoleLogPath,
			"serial_socket", instance.Config.SerialSocket)
	}

	// Confirm the instance has booted
	_, err = utils.ReadPidFile(instance.ID)

	if err != nil {
		slog.Error("Failed to read PID file", "err", err)
		return err
	}

	return nil
}

// buildBaseVMConfig creates a vm.Config with base QEMU settings and PCIe
// hotplug root ports. Architecture, vCPU, and memory come from the caller
// (resolved from instance type info).
func buildBaseVMConfig(instanceID, pidFile, consoleLogPath, serialSocket, architecture string, vCPUs, memoryMiB int) vm.Config {
	cfg := vm.Config{
		Name:           instanceID,
		PIDFile:        pidFile,
		EnableKVM:      true,
		NoGraphic:      true,
		MachineType:    "q35",
		ConsoleLogPath: consoleLogPath,
		SerialSocket:   serialSocket,
		CPUType:        "host",
		Memory:         memoryMiB,
		CPUCount:       vCPUs,
		Architecture:   architecture,
	}

	// Add PCIe root ports for volume hotplug (Q35 requires explicit root ports).
	// 11 ports for /dev/sd[f-p] hotplug slots, starting at chassis 1.
	for i := 1; i <= 11; i++ {
		cfg.Devices = append(cfg.Devices, vm.Device{
			Value: fmt.Sprintf("pcie-root-port,id=hotplug%d,chassis=%d,slot=0", i, i),
		})
	}

	return cfg
}

// buildDrives converts EBS volume requests into QEMU drive, iothread, and device
// configurations. Returns an error if any non-EFI volume is missing its NBDURI.
func buildDrives(requests []types.EBSRequest, cpuCount int) ([]vm.Drive, []vm.IOThread, []vm.Device, error) {
	var drives []vm.Drive
	var iothreads []vm.IOThread
	var devices []vm.Device

	for _, v := range requests {
		// TODO: Add EFI support
		if v.EFI {
			continue
		}

		if v.NBDURI == "" {
			return nil, nil, nil, fmt.Errorf("NBDURI not set for volume %s - was volume mounted?", v.Name)
		}

		drive := vm.Drive{File: v.NBDURI}

		if v.Boot {
			drive.Format = "raw"
			drive.If = "none"
			drive.Media = "disk"
			drive.ID = "os"
			drive.Cache = "none"

			iothreadID := "ioth-os"
			iothreads = append(iothreads, vm.IOThread{ID: iothreadID})
			devices = append(devices, vm.Device{
				Value: fmt.Sprintf("virtio-blk-pci,drive=%s,iothread=%s,num-queues=%d,bootindex=1",
					drive.ID, iothreadID, cpuCount),
			})
		}

		if v.CloudInit {
			drive.Format = "raw"
			drive.If = "virtio"
			drive.Media = "cdrom"
			drive.ID = "cloudinit"
		}

		slog.Info("Using NBD URI for drive", "volume", v.Name, "uri", v.NBDURI)
		drives = append(drives, drive)
	}

	return drives, iothreads, devices, nil
}

// ebsTopic returns a node-specific EBS NATS topic, e.g. "ebs.node1.mount".
// This ensures mount/unmount requests are routed to the viperblock instance
// running on the same node as the daemon (NBD sockets are local).
func (d *Daemon) ebsTopic(action string) string {
	return fmt.Sprintf("ebs.%s.%s", d.node, action)
}

// MountVolumes mounts the volumes for an instance
func (d *Daemon) MountVolumes(instance *vm.VM) error {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()

	for k, v := range instance.EBSRequests.Requests {
		// Send the volume payload as JSON
		ebsMountRequest, err := json.Marshal(v)

		if err != nil {
			slog.Error("Failed to marshal volume payload", "err", err)
			return err
		}

		reply, err := d.natsConn.Request(d.ebsTopic("mount"), ebsMountRequest, 30*time.Second)

		slog.Info("Mounting volume", "Vol", v.Name, "NBDURI", v.NBDURI)

		// TODO: Improve timeout handling
		if err != nil {
			slog.Error("Failed to request EBS mount", "err", err)
			return err
		}

		// Unmarshal the response
		var ebsMountResponse types.EBSMountResponse
		err = json.Unmarshal(reply.Data, &ebsMountResponse)

		if err != nil {
			slog.Error("Failed to unmarshal volume response:", "err", err)
			return err
		}

		if ebsMountResponse.Error == "" {
			slog.Debug("Mounted volume successfully", "response", ebsMountResponse.URI)

			// Append the NBD URI to the request
			instance.EBSRequests.Requests[k].NBDURI = ebsMountResponse.URI
		} else {
			slog.Error("Failed to mount volume", "error", ebsMountResponse.Error)
			return fmt.Errorf("failed to mount volume: %s", ebsMountResponse.Error)
		}
	}

	return nil
}

// rollbackEBSMount sends an ebs.unmount request to undo a previously successful ebs.mount.
// Rollback failures are logged but not propagated; callers treat this as best-effort cleanup.
func (d *Daemon) rollbackEBSMount(req types.EBSRequest) {
	data, err := json.Marshal(req)
	if err != nil {
		slog.Error("rollbackEBSMount: failed to marshal unmount request", "volume", req.Name, "err", err)
		return
	}
	msg, err := d.natsConn.Request(d.ebsTopic("unmount"), data, 10*time.Second)
	if err != nil {
		slog.Error("rollbackEBSMount: ebs.unmount NATS request failed", "volume", req.Name, "err", err)
		return
	}
	var resp types.EBSUnMountResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		slog.Error("rollbackEBSMount: failed to unmarshal response", "volume", req.Name, "err", err)
		return
	}
	if resp.Error != "" {
		slog.Error("rollbackEBSMount: ebs.unmount returned error", "volume", req.Name, "err", resp.Error)
		return
	}
	if resp.Mounted {
		slog.Error("rollbackEBSMount: volume still mounted after unmount", "volume", req.Name)
		return
	}
	slog.Info("rollbackEBSMount: volume unmounted successfully", "volume", req.Name)
}

// respondWithVolumeAttachment builds an ec2.VolumeAttachment, marshals it to JSON, and
// responds on the NATS message. Used by both AttachVolume and DetachVolume handlers.
func (d *Daemon) respondWithVolumeAttachment(msg *nats.Msg, volumeID, instanceID, device, state string) {
	attachment := ec2.VolumeAttachment{
		VolumeId:            aws.String(volumeID),
		InstanceId:          aws.String(instanceID),
		Device:              aws.String(device),
		State:               aws.String(state),
		AttachTime:          aws.Time(time.Now()),
		DeleteOnTermination: aws.Bool(false),
	}

	jsonResp, err := json.Marshal(attachment)
	if err != nil {
		slog.Error("Failed to marshal VolumeAttachment response", "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	if err := msg.Respond(jsonResp); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
}

// nextAvailableDevice finds the next available /dev/sd[f-p] device name for an instance.
// It checks both EBSRequests and BlockDeviceMappings to avoid conflicts.
func nextAvailableDevice(instance *vm.VM) string {
	usedDevices := make(map[string]bool)

	// Collect devices from existing BlockDeviceMappings
	if instance.Instance != nil {
		for _, bdm := range instance.Instance.BlockDeviceMappings {
			if bdm.DeviceName != nil {
				usedDevices[*bdm.DeviceName] = true
			}
		}
	}

	// Collect devices from EBSRequests (may not yet be in BlockDeviceMappings)
	instance.EBSRequests.Mu.Lock()
	for _, req := range instance.EBSRequests.Requests {
		if req.DeviceName != "" {
			usedDevices[req.DeviceName] = true
		}
	}
	instance.EBSRequests.Mu.Unlock()

	// AWS convention: /dev/sd[f-p] for attached volumes
	for c := 'f'; c <= 'p'; c++ {
		dev := fmt.Sprintf("/dev/sd%c", c)
		if !usedDevices[dev] {
			return dev
		}
	}

	return ""
}

// canAllocate checks how many instances of the given type can be allocated.
// Returns the count that can actually be allocated (0 to count).
func (rm *ResourceManager) canAllocate(instanceType *ec2.InstanceTypeInfo, count int) int {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	return canAllocateCount(
		rm.availableVCPU, rm.allocatedVCPU,
		rm.availableMem, rm.allocatedMem,
		instanceTypeVCPUs(instanceType),
		instanceTypeMemoryMiB(instanceType),
		count,
	)
}

// allocate reserves resources for an instance and updates NATS subscriptions
func (rm *ResourceManager) allocate(instanceType *ec2.InstanceTypeInfo) error {
	if rm.canAllocate(instanceType, 1) < 1 {
		instanceTypeName := ""
		if instanceType.InstanceType != nil {
			instanceTypeName = *instanceType.InstanceType
		}
		return fmt.Errorf("insufficient resources for instance type %s", instanceTypeName)
	}

	rm.mu.Lock()
	vCPUs := instanceTypeVCPUs(instanceType)
	memoryGB := float64(instanceTypeMemoryMiB(instanceType)) / 1024.0
	rm.allocatedVCPU += int(vCPUs)
	rm.allocatedMem += memoryGB
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
	return nil
}

// deallocate releases resources for an instance and updates NATS subscriptions
func (rm *ResourceManager) deallocate(instanceType *ec2.InstanceTypeInfo) {
	rm.mu.Lock()
	vCPUs := instanceTypeVCPUs(instanceType)
	memoryGB := float64(instanceTypeMemoryMiB(instanceType)) / 1024.0
	rm.allocatedVCPU -= int(vCPUs)
	rm.allocatedMem -= memoryGB
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
}

// initSubscriptions sets up dynamic per-instance-type NATS subscriptions.
// Called once during daemon startup after NATS is connected.
func (rm *ResourceManager) initSubscriptions(nc *nats.Conn, handler nats.MsgHandler, nodeID string) {
	rm.natsConn = nc
	rm.handler = handler
	rm.nodeID = nodeID
	rm.instanceSubs = make(map[string]*nats.Subscription)
	rm.updateInstanceSubscriptions()
}

// updateInstanceSubscriptions recalculates which instance types can fit on this
// node and subscribes/unsubscribes from the corresponding NATS topics. Each type
// gets two topics:
//   - ec2.RunInstances.{type} with spinifex-workers queue group (load-balanced, for single-instance launches)
//   - ec2.RunInstances.{type}.{nodeId} without queue group (targeted, for multi-node distribution)
//
// Both use the same handler. NATS only routes requests to nodes with available capacity.
func (rm *ResourceManager) updateInstanceSubscriptions() {
	if rm.natsConn == nil {
		return
	}

	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()

	for typeName, typeInfo := range rm.instanceTypes {
		// System types (sys.micro, etc.) are internal-only — not routable via customer API.
		if instancetypes.IsSystemType(typeName) {
			continue
		}
		queueTopic := fmt.Sprintf("ec2.RunInstances.%s", typeName)
		canFit := rm.canAllocate(typeInfo, 1) >= 1

		// Queue group subscription (load-balanced across nodes)
		_, subscribed := rm.instanceSubs[queueTopic]
		if canFit && !subscribed {
			sub, err := rm.natsConn.QueueSubscribe(queueTopic, "spinifex-workers", rm.handler)
			if err != nil {
				slog.Error("Failed to subscribe to instance type topic", "topic", queueTopic, "err", err)
				continue
			}
			rm.instanceSubs[queueTopic] = sub
			slog.Debug("Subscribed to instance type", "topic", queueTopic)
		} else if !canFit && subscribed {
			if err := rm.instanceSubs[queueTopic].Unsubscribe(); err != nil {
				slog.Error("Failed to unsubscribe from instance type topic", "topic", queueTopic, "err", err)
			}
			delete(rm.instanceSubs, queueTopic)
			slog.Info("Unsubscribed from instance type (capacity full)", "topic", queueTopic)
		}

		// Node-specific subscription (targeted routing for multi-node distribution)
		if rm.nodeID != "" {
			nodeTopic := fmt.Sprintf("ec2.RunInstances.%s.%s", typeName, rm.nodeID)
			_, nodeSubscribed := rm.instanceSubs[nodeTopic]
			if canFit && !nodeSubscribed {
				sub, err := rm.natsConn.Subscribe(nodeTopic, rm.handler)
				if err != nil {
					slog.Error("Failed to subscribe to node-specific topic", "topic", nodeTopic, "err", err)
					continue
				}
				rm.instanceSubs[nodeTopic] = sub
				slog.Debug("Subscribed to node-specific instance type", "topic", nodeTopic)
			} else if !canFit && nodeSubscribed {
				if err := rm.instanceSubs[nodeTopic].Unsubscribe(); err != nil {
					slog.Error("Failed to unsubscribe from node-specific topic", "topic", nodeTopic, "err", err)
				}
				delete(rm.instanceSubs, nodeTopic)
				slog.Info("Unsubscribed from node-specific instance type (capacity full)", "topic", nodeTopic)
			}
		}
	}
}

// wireLBAgentConfig loads system credentials, resolves the gateway URL,
// reads the CA certificate, and wires them into the ELBv2 service so LB
// VMs get SigV4 credentials and gateway URL injected via cloud-init.
func (d *Daemon) wireLBAgentConfig() {
	// Use system credentials from spinifex.toml (predastore section).
	// These are the same service-to-service credentials written by admin init
	// into both spinifex.toml and system-credentials.json. Reading from the
	// config avoids file permission issues with the separate JSON file.
	if d.config.Predastore.AccessKey != "" && d.config.Predastore.SecretKey != "" {
		d.systemAccessKey = d.config.Predastore.AccessKey
		d.systemSecretKey = d.config.Predastore.SecretKey
		d.elbv2Service.SystemAccessKey = d.config.Predastore.AccessKey
		d.elbv2Service.SystemSecretKey = d.config.Predastore.SecretKey
		slog.Info("System credentials loaded for LB agent auth")
	} else {
		slog.Warn("System credentials missing from spinifex.toml predastore section — LB VMs will not have SigV4 credentials for agent auth")
	}

	// Resolve gateway URL — the address LB VMs use to reach the AWS gateway.
	// Precedence:
	//   1. AWSGW listens on a specific WAN IP + br-mgmt present → use WAN IP
	//      and add host route via br-mgmt for internal LBs.
	//   2. br-mgmt present + AWSGW on 0.0.0.0 → br-mgmt IP (both LB flavours
	//      reach the daemon via mgmt).
	//   3. This node's AdvertiseIP (single-node default install) → AdvertiseIP.
	//   4. DevNetworking shim → 10.0.2.2.
	//   5. AWSGW bound to specific IP (no br-mgmt, no advertise) → that IP.
	//   6. Else: error and skip assignment — no silent empty URL.
	var gatewayHost string
	awsgwBindIP := ""
	if d.config.AWSGW.Host != "" {
		if h, _, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil {
			awsgwBindIP = h
		}
	}

	advertiseIP := d.config.AdvertiseIP

	switch {
	case d.mgmtBridgeIP != "" && awsgwBindIP != "" && awsgwBindIP != "0.0.0.0" && !net.ParseIP(awsgwBindIP).IsLoopback():
		// Multi-node: AWSGW listens on a specific WAN IP. Use that IP as the
		// gateway URL; internal LBs get a host route via br-mgmt.
		gatewayHost = awsgwBindIP
		d.mgmtRouteVia = awsgwBindIP
	case d.mgmtBridgeIP != "":
		// Multi-node with AWSGW on 0.0.0.0 — br-mgmt reaches all LBs.
		gatewayHost = d.mgmtBridgeIP
	case advertiseIP != "" && advertiseIP != "0.0.0.0":
		// Single-node default install — off-host clients dial the advertised IP.
		gatewayHost = advertiseIP
	case d.config.Daemon.DevNetworking:
		gatewayHost = "10.0.2.2"
	case awsgwBindIP != "" && awsgwBindIP != "0.0.0.0":
		gatewayHost = awsgwBindIP
	}

	// Extract port from AWSGW host config (e.g. "0.0.0.0:9999" → "9999").
	gatewayPort := "9999"
	if d.config.AWSGW.Host != "" {
		if _, port, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil && port != "" {
			gatewayPort = port
		}
	}

	if gatewayHost != "" {
		gatewayURL := "https://" + net.JoinHostPort(gatewayHost, gatewayPort)
		d.elbv2Service.GatewayURL = gatewayURL
		slog.Info("LB agent gateway URL configured", "url", gatewayURL)
	} else {
		slog.Error("LB agent gateway URL not configured: no reachable host found — CreateLoadBalancer will fail until --advertise or br-mgmt is configured",
			"awsgwBindIP", awsgwBindIP, "mgmtBridgeIP", d.mgmtBridgeIP, "advertiseIP", advertiseIP)
	}

	// Pass mgmt route info so lbVMUserData can add a bootcmd route for
	// internal LBs that reach the AWSGW via the management NIC.
	if d.mgmtRouteVia != "" {
		d.elbv2Service.MgmtRouteGateway = d.mgmtBridgeIP
		d.elbv2Service.MgmtRouteTarget = d.mgmtRouteVia
	}
}
