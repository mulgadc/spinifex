package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	"github.com/mulgadc/spinifex/spinifex/gpu"
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
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
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
	mu sync.RWMutex
	// hostVCPU / hostMemGB are the raw figures reported by the host
	// (runtime.NumCPU, /proc/meminfo). Schedulable capacity for guest VMs
	// is host - reserved - allocated.
	hostVCPU  int
	hostMemGB float64
	// reservedVCPU / reservedMem are held back from guest scheduling for
	// the spinifex daemon and co-located services (NATS, predastore,
	// viperblock, vpcd, awsgw, ui). See hostReserve / defaultHostReserve.
	reservedVCPU  int
	reservedMem   float64
	allocatedVCPU int
	allocatedMem  float64
	instanceTypes map[string]*ec2.InstanceTypeInfo
	gpuManager    *gpu.Manager // nil if GPU passthrough is disabled or no GPUs present

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

	// vmMgr owns the in-memory map of VMs running on this node.
	vmMgr *vm.Manager

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

	// stateStore is the vm.StateStore-shaped view over jsManager. Both the
	// vm.Manager and daemon-side handlers route VM-instance state I/O
	// through it. Initialized after initJetStream succeeds.
	stateStore vm.StateStore

	// Delay after QMP device_del before blockdev-del (default 1s, 0 in tests)
	detachDelay time.Duration

	// NATS connect retry options (nil uses defaults: 5min max, 500ms initial delay)
	natsRetryOpts []utils.RetryOption

	// networkPlumber handles tap device lifecycle for VPC and management networking
	networkPlumber vm.NetworkPlumber

	// Management NIC infrastructure: bridge IP + IP allocator for system instances.
	// Populated at startup when br-mgmt is detected; nil/empty otherwise.
	mgmtBridgeIP    string
	mgmtIPAllocator *MgmtIPAllocator
	// mgmtRouteVia is the AWSGW bind IP that system instances must route via the
	// management NIC. Set when AWSGW binds to a specific IP (multi-node).
	mgmtRouteVia string

	// gpuProbe holds the result of the always-on startup GPU hardware probe.
	// Populated regardless of whether gpu_passthrough is enabled in config.
	gpuProbe gpuProbeResult

	// gpuManager handles VFIO bind/unbind lifecycle for GPU passthrough.
	// Nil when GPUPassthrough is disabled in config or no recognised GPUs are found.
	gpuManager *gpu.Manager

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
// Also returns an error if the host is too small to satisfy the daemon's
// reserve — clamping silently would defeat the reserve and look like a
// runtime bug.
// gpuModels is the list of recognised GPU models present on the host; pass nil if
// GPU passthrough is disabled or no GPUs were found.
func NewResourceManager(gpuModels []instancetypes.GPUModel, gpuMgr *gpu.Manager) (*ResourceManager, error) {
	// Get system CPU cores
	numCPU := runtime.NumCPU()

	// Get system memory (in GB)
	totalMemGB, err := getSystemMemory()
	if err != nil {
		return nil, fmt.Errorf("detect system memory: %w", err)
	}

	reservedVCPU, reservedMem, err := applyHostReserve(defaultHostReserve, numCPU, totalMemGB)
	if err != nil {
		slog.Error("host below minimum reserve — daemon refuses to start",
			"err", err, "hostVCPU", numCPU, "hostMemGB", totalMemGB,
			"reserveVCPU", defaultHostReserve.vCPU, "reserveMemGB", defaultHostReserve.memGB)
		return nil, fmt.Errorf("validate host reserve: %w", err)
	}

	// Determine architecture
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}

	// Detect CPU generation and generate matching instance types (including GPU
	// families when gpuModels is non-nil).
	instanceTypes := instancetypes.DetectAndGenerate(instancetypes.HostCPU{}, arch, gpuModels)

	slog.Info("System resources detected",
		"hostVCPU", numCPU, "hostMemGB", totalMemGB,
		"reservedVCPU", reservedVCPU, "reservedMemGB", reservedMem,
		"schedulableVCPU", numCPU-reservedVCPU, "schedulableMemGB", totalMemGB-reservedMem,
		"instanceTypes", len(instanceTypes))

	return &ResourceManager{
		hostVCPU:      numCPU,
		hostMemGB:     totalMemGB,
		reservedVCPU:  reservedVCPU,
		reservedMem:   reservedMem,
		instanceTypes: instanceTypes,
		gpuManager:    gpuMgr,
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

		requiresGPU := instancetypes.IsGPUType(it)
		availGPU := 0
		if rm.gpuManager != nil && requiresGPU {
			availGPU = rm.gpuManager.Available()
		}
		count := canAllocateCount(
			rm.hostVCPU-rm.reservedVCPU, rm.allocatedVCPU,
			rm.hostMemGB-rm.reservedMem, rm.allocatedMem,
			vCPUs, memMiB,
			1<<30, // effectively unlimited — let resources be the constraint
			availGPU, requiresGPU,
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
		"hostVCPU", rm.hostVCPU, "hostMem", rm.hostMemGB,
		"reservedVCPU", rm.reservedVCPU, "reservedMem", rm.reservedMem,
		"showCapacity", showCapacity)

	return infos
}

// GetResourceStats returns current resource allocation stats for the node status response.
// totalVCPU / totalMemGB are the raw host figures; reservedVCPU / reservedMemGB are
// held back from guest scheduling. Per-type caps reflect host - reserved - allocated,
// matching what the admission path will actually permit.
func (rm *ResourceManager) GetResourceStats() (totalVCPU int, totalMemGB float64, reservedVCPU int, reservedMemGB float64, allocVCPU int, allocMemGB float64, caps []types.InstanceTypeCap) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	totalVCPU = rm.hostVCPU
	totalMemGB = rm.hostMemGB
	reservedVCPU = rm.reservedVCPU
	reservedMemGB = rm.reservedMem
	allocVCPU = rm.allocatedVCPU
	allocMemGB = rm.allocatedMem

	remainingVCPU := rm.hostVCPU - rm.reservedVCPU - rm.allocatedVCPU
	remainingMem := rm.hostMemGB - rm.reservedMem - rm.allocatedMem
	if remainingVCPU < 0 || remainingMem < 0 {
		slog.Error("schedulable capacity negative — reserve misconfigured or allocation drift",
			"hostVCPU", rm.hostVCPU, "reservedVCPU", rm.reservedVCPU, "allocatedVCPU", rm.allocatedVCPU,
			"hostMemGB", rm.hostMemGB, "reservedMem", rm.reservedMem, "allocatedMem", rm.allocatedMem,
			"remainingVCPU", remainingVCPU, "remainingMem", remainingMem)
	}

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
	return totalVCPU, totalMemGB, reservedVCPU, reservedMemGB, allocVCPU, allocMemGB, caps
}

// SetConfigPath sets the configuration file path for cluster management
func (d *Daemon) SetConfigPath(path string) {
	d.configPath = path
}

// NewDaemon creates a new daemon instance
func NewDaemon(cfg *config.ClusterConfig) (*Daemon, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// If WalDir is not set, use BaseDir
	nodeCfg := cfg.Nodes[cfg.Node]
	if cfg.Nodes[cfg.Node].WalDir == "" {
		nodeCfg.WalDir = nodeCfg.BaseDir
		cfg.Nodes[cfg.Node] = nodeCfg
	}

	// Phase 1: always probe GPU hardware (no side effects, no config required).
	gpuProbe := probeGPU()

	// Phase 2: activate GPU passthrough only when the operator has opted in.
	var gpuModels []instancetypes.GPUModel
	var gpuMgr *gpu.Manager
	if nodeCfg.Daemon.GPUPassthrough {
		if !gpuProbe.Capable {
			slog.Warn("GPU passthrough enabled in config but prerequisites not met",
				"iommu", gpuProbe.IOMMUActive, "vfio", gpuProbe.VFIOPresent,
				"gpus", len(gpuProbe.Devices))
		} else {
			for _, dev := range gpuProbe.Devices {
				gpuModels = append(gpuModels, resolveGPUModel(dev, nodeCfg.Daemon.GPUModelOverrides))
			}
			gpuMgr = gpu.NewManager(gpuProbe.Devices)
			slog.Info("GPU passthrough enabled", "gpus", len(gpuProbe.Devices), "knownModels", len(gpuModels))
		}
	} else if gpuProbe.Capable {
		slog.Info("GPU hardware detected, passthrough not enabled",
			"gpus", len(gpuProbe.Devices), "hint", "run 'spx admin gpu enable' to activate")
	}

	rm, err := NewResourceManager(gpuModels, gpuMgr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize resource manager: %w", err)
	}

	return &Daemon{
		node:              cfg.Node,
		clusterConfig:     cfg,
		config:            &nodeCfg,
		resourceMgr:       rm,
		gpuProbe:          gpuProbe,
		gpuManager:        gpuMgr,
		ctx:               ctx,
		cancel:            cancel,
		vmMgr:             vm.NewManager(),
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
		{"ec2.DescribeVolumesModifications", d.handleEC2DescribeVolumesModifications, "spinifex-workers"},
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
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(d.config, d.resourceMgr.instanceTypes, d.natsConn, store)
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
			gwMAC := ""
			if d.clusterConfig.Bootstrap.VpcId != "" {
				gwMAC = utils.HashMAC("gw-" + d.clusterConfig.Bootstrap.VpcId)
			}
			for _, p := range d.clusterConfig.Network.ExternalPools {
				pools = append(pools, handlers_ec2_vpc.ExternalPoolConfig{
					Name:            p.Name,
					Source:          p.Source,
					RangeStart:      p.RangeStart,
					RangeEnd:        p.RangeEnd,
					Gateway:         p.Gateway,
					GatewayIP:       p.GatewayIP,
					PrefixLen:       p.PrefixLen,
					Region:          p.Region,
					AZ:              p.AZ,
					DhcpBindBridge:  dhcpBindBridge,
					GatewayMAC:      gwMAC,
					GwLrpRangeStart: p.GwLrpRangeStart,
					GwLrpRangeEnd:   p.GwLrpRangeEnd,
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

	// Wire vm.Manager collaborators now that NATS, JetStream, network plumber,
	// volume service, and resource manager are all ready.
	d.vmMgr.SetDeps(d.buildVMManagerDeps())

	// Protect daemon from OOM killer (prefer killing QEMU VMs instead)
	if err := utils.SetOOMScore(os.Getpid(), -500); err != nil {
		slog.Warn("Failed to set daemon OOM score", "err", err)
	}

	d.waitForClusterReady()
	d.upgradeJetStreamReplicas()
	d.vmMgr.Restore()

	// Rebuild mgmt IP allocator from restored VMs so we don't re-allocate IPs
	// that are already in use by running system instances.
	if d.mgmtIPAllocator != nil {
		d.mgmtIPAllocator.Rebuild(d.vmMgr.SnapshotMap())
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
	d.vmMgr.StartPendingWatchdog(d.ctx)

	d.ready.Store(true)
	slog.Info("Daemon fully initialized", "node", d.node, "startupTime", time.Since(d.startTime).Round(time.Second))

	d.setupReload()
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

	d.stateStore = newStateStoreAdapter(d.jsManager)

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

	// Load TLS certificate.
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
// The marshal+put runs under the manager lock so VM fields can't change
// mid-encode. Lock-across-Put is a known limitation; splitting marshal from
// put requires a JetStreamManager API change and is deferred.
func (d *Daemon) WriteState() error {
	if d.jsManager == nil {
		return fmt.Errorf("JetStream manager not initialized - cannot write state")
	}
	var writeErr error
	d.vmMgr.View(func(vms map[string]*vm.VM) {
		writeErr = d.jsManager.WriteState(d.node, vms)
	})
	if writeErr != nil {
		slog.Error("JetStream write failed", "error", writeErr)
		return fmt.Errorf("failed to write state to JetStream: %w", writeErr)
	}
	return nil
}

// setupReload registers a SIGHUP handler that reloads GPU config without restarting.
func (d *Daemon) setupReload() {
	d.shutdownWg.Go(func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGHUP)
		defer signal.Stop(sigChan)
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-sigChan:
				slog.Info("SIGHUP received — reloading GPU config")
				d.reloadConfig()
			}
		}
	})
}

// reloadConfig re-reads spinifex.toml and applies any GPU passthrough changes.
func (d *Daemon) reloadConfig() {
	if d.configPath == "" {
		slog.Warn("SIGHUP: no config path set, cannot reload")
		return
	}
	newCfg, err := config.LoadConfig(d.configPath)
	if err != nil {
		slog.Error("SIGHUP: config reload failed", "err", err)
		return
	}
	newNodeCfg := newCfg.Nodes[d.node]
	d.applyGPUConfig(newNodeCfg.Daemon.GPUPassthrough)
}

// applyGPUConfig activates or deactivates GPU passthrough at runtime.
// Transition false→true: re-probes hardware, initialises gpuManager, adds g5 types.
// Transition true→false: refused when GPU instances are running; otherwise tears down.
func (d *Daemon) applyGPUConfig(enabled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	wasEnabled := d.gpuManager != nil
	if enabled == wasEnabled {
		slog.Debug("GPU passthrough state unchanged on reload", "passthrough", enabled)
		return
	}

	if enabled {
		probe := probeGPU()
		d.gpuProbe = probe
		if !probe.Capable {
			slog.Warn("GPU passthrough enable failed: prerequisites not met",
				"iommu", probe.IOMMUActive, "vfio", probe.VFIOPresent, "gpus", len(probe.Devices))
			return
		}
		var models []instancetypes.GPUModel
		for _, dev := range probe.Devices {
			models = append(models, resolveGPUModel(dev, d.config.Daemon.GPUModelOverrides))
		}
		mgr := gpu.NewManager(probe.Devices)
		d.gpuManager = mgr
		d.resourceMgr.reloadGPUTypes(models, mgr)
		slog.Info("GPU passthrough enabled via config reload", "gpus", len(probe.Devices))
		return
	}

	// true → false: refuse if instances are running
	if d.gpuManager.AllocatedCount() > 0 {
		slog.Warn("GPU passthrough disable refused: GPU instances are running",
			"allocated", d.gpuManager.AllocatedCount())
		return
	}
	d.gpuManager = nil
	d.resourceMgr.reloadGPUTypes(nil, nil)
	slog.Info("GPU passthrough disabled via config reload")
}

func (d *Daemon) setupShutdown() {
	d.shutdownWg.Go(func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		<-sigChan
		slog.Info("Received shutdown signal, cleaning up...")

		// Cancel context to stop heartbeat and other goroutines
		d.cancel()

		// If coordinated shutdown already handled VMs (DRAIN phase), skip the
		// per-instance teardown. Otherwise, set the flag now so crash handlers
		// and restart schedulers know to bail out during SIGTERM-based shutdown.
		if d.shuttingDown.Load() {
			slog.Info("Coordinated shutdown in progress, skipping VM stop (already handled by DRAIN phase)")
		} else {
			d.shuttingDown.Store(true)
			if err := d.vmMgr.StopAll(); err != nil {
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

// canAllocate checks how many instances of the given type can be allocated.
// Returns the count that can actually be allocated (0 to count).
func (rm *ResourceManager) canAllocate(instanceType *ec2.InstanceTypeInfo, count int) int {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	requiresGPU := instancetypes.IsGPUType(instanceType)
	availGPU := 0
	if rm.gpuManager != nil && requiresGPU {
		availGPU = rm.gpuManager.Available()
	}

	return canAllocateCount(
		rm.hostVCPU-rm.reservedVCPU, rm.allocatedVCPU,
		rm.hostMemGB-rm.reservedMem, rm.allocatedMem,
		instanceTypeVCPUs(instanceType),
		instanceTypeMemoryMiB(instanceType),
		count,
		availGPU, requiresGPU,
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
// reloadGPUTypes replaces GPU instance types in-place and updates NATS subscriptions.
// Called on SIGHUP when gpu_passthrough is toggled. Mutates the existing map so that
// all holders of the map reference (e.g. instanceService) see the updated types.
func (rm *ResourceManager) reloadGPUTypes(models []instancetypes.GPUModel, mgr *gpu.Manager) {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}

	rm.mu.Lock()
	for name, it := range rm.instanceTypes {
		if instancetypes.IsGPUType(it) {
			delete(rm.instanceTypes, name)
		}
	}
	if len(models) > 0 {
		maps.Copy(rm.instanceTypes, instancetypes.GenerateGPUTypes(models, arch))
	}
	rm.gpuManager = mgr
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
}

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
	//   1. br-mgmt present + AWSGW on a dedicated IP distinct from AdvertiseIP
	//      (multi-node: AWSGW on a mgmt-only IP, VPC path can't reach it) →
	//      gateway URL is the AWSGW bind IP and lb-agent gets a bootcmd host
	//      route via br-mgmt.
	//   2. AdvertiseIP set (single-node, or multi-node where AWSGW binds to
	//      the advertised IP) → AdvertiseIP. VMs reach it via VPC → external
	//      (OVN's own dnat_and_snat SNATs their reply back to the ALB EIP).
	//      Critically, we do NOT add the mgmt host route here: when host IPs
	//      on the WAN share the advertiseIP, the /32 route would steal the
	//      return path for host-initiated ALB connections — replies would
	//      egress via mgmt with the VM's 10.x source IP, bypass OVN's SNAT,
	//      and arrive at the host with a source that doesn't match the open
	//      TCP socket (the client dialed the EIP, not the VM IP).
	//   3. br-mgmt present + AWSGW on 0.0.0.0 → br-mgmt IP (both LB flavours
	//      reach the daemon via mgmt).
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
	case d.mgmtBridgeIP != "" && awsgwBindIP != "" && awsgwBindIP != "0.0.0.0" &&
		!net.ParseIP(awsgwBindIP).IsLoopback() && awsgwBindIP != advertiseIP:
		// Multi-node: AWSGW on a dedicated mgmt IP. VMs can't reach it via
		// VPC → external, so add a bootcmd host route via br-mgmt.
		gatewayHost = awsgwBindIP
		d.mgmtRouteVia = awsgwBindIP
	case advertiseIP != "" && advertiseIP != "0.0.0.0":
		// Single-node, or multi-node where AWSGW binds to AdvertiseIP: VMs
		// reach AWSGW via the normal VPC → external path. No mgmt host route.
		gatewayHost = advertiseIP
	case d.mgmtBridgeIP != "":
		// br-mgmt present + AWSGW on 0.0.0.0 and no advertiseIP — br-mgmt IP
		// is the only reachable address.
		gatewayHost = d.mgmtBridgeIP
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

	// Always expose mgmtBridgeIP and advertiseIP so lbVMUserData can synthesize
	// a mgmt-NIC fallback route for internal-scheme LBs on single-node setups
	// (where MgmtRoute{Gateway,Target} stay empty because internet-facing LBs
	// reach AWSGW via VPC + EIP SNAT). Internal LBs have no EIP, so without
	// this fallback the agent has no return path and the LB stays in
	// provisioning forever.
	d.elbv2Service.MgmtBridgeIP = d.mgmtBridgeIP
	d.elbv2Service.AdvertiseIP = advertiseIP
}

// resolveGPUModel maps a discovered GPU to an instance type model.
// Overrides take priority, then the production model list, then a g5 default.
// Any GPU device that reaches the default path is treated as a g5 instance,
// so consumer GPUs used for testing work without explicit config entries.
func resolveGPUModel(dev gpu.GPUDevice, overrides []config.GPUModelOverride) instancetypes.GPUModel {
	for i := range overrides {
		o := &overrides[i]
		if o.VendorID == dev.VendorID && o.DeviceID == dev.DeviceID {
			return instancetypes.GPUModel{
				VendorID:     o.VendorID,
				DeviceID:     o.DeviceID,
				Family:       o.Family,
				Manufacturer: o.Manufacturer,
				Name:         o.Name,
				MemoryMiB:    o.MemoryMiB,
			}
		}
	}
	if m := instancetypes.GPUModelForVendorDevice(dev.VendorID, dev.DeviceID); m != nil {
		return *m
	}
	// Default: any discovered GPU device maps to g5 using its detected specs.
	name := dev.Model
	if name == "" {
		name = fmt.Sprintf("GPU %s:%s", dev.VendorID, dev.DeviceID)
	}
	return instancetypes.GPUModel{
		VendorID:     dev.VendorID,
		DeviceID:     dev.DeviceID,
		Family:       "g5",
		Manufacturer: gpuVendorDisplayName(dev.Vendor),
		Name:         name,
		MemoryMiB:    dev.MemoryMiB,
	}
}

// gpuXVGAEnabled returns true when the QEMU device should include x-vga=on.
// Consumer GPUs default to true; known datacenter/compute cards default to false.
// A GPUModelOverride with XVGAOff=true forces false regardless of the model table.
func gpuXVGAEnabled(dev *gpu.GPUDevice, overrides []config.GPUModelOverride) bool {
	for _, o := range overrides {
		if o.VendorID == dev.VendorID && o.DeviceID == dev.DeviceID {
			return !o.XVGAOff
		}
	}
	return !gpu.IsComputeGPU(dev.VendorID, dev.DeviceID)
}

func gpuVendorDisplayName(v gpu.Vendor) string {
	switch v {
	case gpu.VendorNVIDIA:
		return "NVIDIA"
	case gpu.VendorAMD:
		return "AMD"
	case gpu.VendorIntel:
		return "Intel"
	default:
		return "Unknown"
	}
}

// gpuProbeResult holds the outcome of the always-on startup hardware probe.
// Populated before any config-gated activation logic runs.
type gpuProbeResult struct {
	Capable     bool // true when Devices, IOMMUActive, and VFIOPresent are all satisfied
	IOMMUActive bool
	VFIOPresent bool
	Devices     []gpu.GPUDevice
}

// probeGPU discovers GPU hardware and checks passthrough prerequisites.
// It is read-only and has no side effects.
func probeGPU() gpuProbeResult {
	var r gpuProbeResult

	devices, err := gpu.Discover()
	if err != nil {
		slog.Debug("GPU probe: discover failed", "err", err)
	}
	r.Devices = devices

	// IOMMU is active when the kernel has populated iommu_groups in sysfs.
	groups, err := os.ReadDir("/sys/kernel/iommu_groups")
	r.IOMMUActive = err == nil && len(groups) > 0

	// vfio_pci module is present when its sysfs module directory exists.
	_, err = os.Stat("/sys/module/vfio_pci")
	r.VFIOPresent = err == nil

	r.Capable = len(r.Devices) > 0 && r.IOMMUActive && r.VFIOPresent
	return r
}
