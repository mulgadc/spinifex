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
	"github.com/mulgadc/spinifex/internal/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	handlers_ec2_account "github.com/mulgadc/spinifex/spinifex/handlers/ec2/account"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/handlers/ec2/launchtemplate"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// ResourceManager handles the allocation and tracking of system resources.
// It dynamically manages per-instance-type NATS subscriptions: when capacity
// is available for a type, the node subscribes to ec2.RunInstances.{type};
// when full, it unsubscribes so NATS routes requests to other nodes.
type ResourceManager struct {
	mu sync.RWMutex
	// hostVCPU / hostMemGB: physical cores (not SMT threads) and total RAM.
	// Schedulable capacity = host - reserved - allocated.
	hostVCPU  int
	hostMemGB float64
	// reservedVCPU / reservedMem: resources held back for the daemon and
	// co-located services. See hostReserve / defaultHostReserve.
	reservedVCPU  int
	reservedMem   float64
	allocatedVCPU int
	allocatedMem  float64
	// reservedCRVCPU / reservedCRMem: compute held by capacity reservations
	// pinned to this node. Subtracted from schedulable capacity exactly like
	// the host reserve. In-memory only — lost on daemon restart.
	reservedCRVCPU int
	reservedCRMem  float64
	// reservations: in-memory capacity reservations owned by this node, keyed
	// by id. Mutated together with reservedCR* under mu.
	reservations map[string]*capacityReservation
	// nbdkitMainMiB / nbdkitAuxMiB: per-volume nbdkit memory charged at
	// admission so nbdkit backing a guest's volumes is accounted explicitly.
	nbdkitMainMiB int
	nbdkitAuxMiB  int
	instanceTypes map[string]*ec2.InstanceTypeInfo
	gpuManager    *gpu.Manager // nil if GPU passthrough is disabled or no GPUs present

	// readMemAvailableGB is the live second admission gate (MemAvailable from
	// /proc/meminfo). Catches real overcommit the static -m accounting misses.
	// nil disables it (SPINIFEX_ADMISSION_LIVE_MEM=0); read failure fails open.
	readMemAvailableGB func() (float64, bool)

	// Dynamic instance-type subscription management
	subsMu        sync.Mutex
	natsConn      *nats.Conn
	instanceSubs  map[string]*nats.Subscription
	handler       nats.MsgHandler
	systemHandler nats.MsgHandler // handles system.LaunchInstance.* requests (ALB-VM fan-out)
	nodeID        string          // node identifier for node-specific topic subscriptions
}

// Compile-time guarantee that the RouteTable service satisfies the IGW
// handler's GatePublisher hook — the two services are wired together below.
var _ handlers_ec2_igw.GatePublisher = (*handlers_ec2_routetable.RouteTableServiceImpl)(nil)

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
	launchTemplateService *handlers_ec2_launchtemplate.LaunchTemplateServiceImpl
	spotInstanceService   *handlers_ec2_spotinstance.SpotInstanceServiceImpl
	vpcService            *handlers_ec2_vpc.VPCServiceImpl
	eipService            handlers_ec2_eip.EIPService
	elbv2Service          *handlers_elbv2.ELBv2ServiceImpl
	eksService            *handlers_eks.EKSServiceImpl
	ecsService            *handlers_ecs.Service
	ecsScheduler          *handlers_ecs.Scheduler
	acmService            *handlers_acm.ACMServiceImpl
	ecrMetaService        *handlers_ecr.MetaServiceImpl
	routeTableService     *handlers_ec2_routetable.RouteTableServiceImpl
	natGatewayService     *handlers_ec2_natgw.NatGatewayServiceImpl
	externalIPAM          *handlers_ec2_vpc.ExternalIPAM
	ctx                   context.Context
	cancel                context.CancelFunc
	shutdownWg            sync.WaitGroup

	// systemDispatchWg tracks in-flight system.LaunchInstance / TerminateInstance
	// handlers. Each runs in its own goroutine so a slow VM boot never blocks
	// the NATS subscription. Used by tests to await dispatch completion.
	systemDispatchWg sync.WaitGroup

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

	// stateStore is the vm.StateStore view over jsManager, initialized after
	// initJetStream succeeds.
	stateStore vm.StateStore

	// Delay after QMP device_del before blockdev-del (default 1s, 0 in tests).
	// Only used as a fallback when deviceDeletedTimeout is 0.
	detachDelay time.Duration

	// deviceDeletedTimeout bounds how long DetachVolume waits for QEMU's
	// DEVICE_DELETED event after device_del before falling back to the
	// blockdev-del retry loop (default 15s, 0 disables the wait in tests).
	deviceDeletedTimeout time.Duration

	// NATS connect retry options (nil uses defaults: 5min max, 500ms initial delay)
	natsRetryOpts []utils.RetryOption

	// requireNATSTimeout caps the first connectNATS attempt under
	// SPINIFEX_REQUIRE_NATS=1. Default 30s; tests use a shorter value.
	requireNATSTimeout time.Duration

	// exitFunc is called when SPINIFEX_REQUIRE_NATS=1 and the first connect
	// times out. Defaults to os.Exit; tests override to avoid killing the process.
	exitFunc func(int)

	// networkPlumber handles tap device lifecycle for VPC and management networking
	networkPlumber vm.NetworkPlumber

	// mgmtBridgeIP / mgmtIPAllocator: management NIC infrastructure for system
	// instances. Populated at startup when br-mgmt is detected.
	mgmtBridgeIP    string
	mgmtIPAllocator *MgmtIPAllocator
	// mgmtRouteVia: AWSGW bind IP system instances route via br-mgmt (multi-node).
	mgmtRouteVia string

	// gpuProbe: startup GPU hardware probe result, always populated.
	gpuProbe gpuProbeResult

	// gpuManager: VFIO bind/unbind for GPU passthrough. Nil when disabled or no GPUs found.
	gpuManager *gpu.Manager

	// shuttingDown: set during GATE or SIGTERM; daemon rejects new work and
	// crash handlers bail out.
	shuttingDown atomic.Bool

	// ready: set once NATS, JetStream, and all services are initialized.
	// /health reports "starting" until true.
	ready atomic.Bool

	// natsConnected: true when the NATS TCP connection is live.
	natsConnected atomic.Bool
	// peersReachable: true when at least one peer /health responded in the
	// last probe cycle. Pinned true on single-node clusters.
	peersReachable atomic.Bool

	// natsRetryCount: disconnect→reconnect cycles since process start.
	natsRetryCount atomic.Int64

	// stateRevision: incremented on each successful WriteState.
	stateRevision atomic.Uint64

	// kvSyncFailures: best-effort JetStream KV sync failures since start.
	kvSyncFailures atomic.Int64
	// lastKVSyncAt: unix-nano timestamp of the last successful KV sync.
	lastKVSyncAt atomic.Int64
	// lastKVSyncError: most recent KV sync error message; "" on success.
	lastKVSyncError atomic.Value

	// reconciling: coalesces concurrent reconcileOnHeal calls.
	reconciling atomic.Bool
	// stateWriteMu: serialises WriteState to prevent races on the .tmp staging file.
	stateWriteMu sync.Mutex

	// iamEnsurerMu guards the lazily-built system-role IAM service (systemRoleEnsurer).
	iamEnsurerMu     sync.Mutex
	iamEnsurerCached handlers_iam.SystemInstanceRoleEnsurer

	mu sync.Mutex
}

// Daemon connectivity modes derived by Mode().
const (
	DaemonModeStandalone = "standalone"
	DaemonModeCluster    = "cluster"
)

// Mode returns "cluster" iff both natsConnected and peersReachable are true;
// otherwise "standalone". Two signals are required so a NATS-up partition
// (DDIL Scenario C) still reports standalone when no peer responds.
func (d *Daemon) Mode() string {
	if d.natsConnected.Load() && d.peersReachable.Load() {
		return DaemonModeCluster
	}
	return DaemonModeStandalone
}

// NATSRetryCount returns the number of disconnect→reconnect cycles observed
// since process start.
func (d *Daemon) NATSRetryCount() int64 {
	return d.natsRetryCount.Load()
}

// Revision returns the local-state revision counter. Bumped on every successful
// WriteState; observers can detect changes without diffing the full payload.
func (d *Daemon) Revision() uint64 {
	return d.stateRevision.Load()
}

// KVSyncFailures returns the number of best-effort JetStream KV sync failures
// (timeout or put error) observed since process start.
func (d *Daemon) KVSyncFailures() int64 {
	return d.kvSyncFailures.Load()
}

// LastKVSyncAt returns the timestamp of the most recent successful best-effort
// KV sync. Zero time means "never synced since process start".
func (d *Daemon) LastKVSyncAt() time.Time {
	n := d.lastKVSyncAt.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// LastKVSyncError returns the most recent best-effort KV sync error message.
// Empty string means the last attempt succeeded (or no attempt has been made).
func (d *Daemon) LastKVSyncError() string {
	v := d.lastKVSyncError.Load()
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// RecordKVSyncSuccess implements KVSyncObserver. The JetStream manager calls
// this from its best-effort write path on a successful Put.
func (d *Daemon) RecordKVSyncSuccess(_ string) {
	d.lastKVSyncAt.Store(time.Now().UnixNano())
	d.lastKVSyncError.Store("")
}

// RecordKVSyncFailure implements KVSyncObserver. Bumps the failure counter and
// records the error message for /local/status.
func (d *Daemon) RecordKVSyncFailure(_ string, err error) {
	d.kvSyncFailures.Add(1)
	if err != nil {
		d.lastKVSyncError.Store(err.Error())
	}
}

// onNATSDisconnect runs when the NATS client loses its connection. Flips
// natsConnected to false so Mode() reports standalone and scatter-gather
// bailouts react immediately. Must not block — runs on a NATS client goroutine.
func (d *Daemon) onNATSDisconnect(_ *nats.Conn, _ error) {
	d.natsConnected.Store(false)
}

// onNATSReconnect runs when the NATS client reattaches to a server. Marks
// NATS connected, bumps the retry counter, and dispatches reconcileOnHeal
// in a goroutine to keep this NATS client callback non-blocking.
func (d *Daemon) onNATSReconnect(_ *nats.Conn) {
	d.natsConnected.Store(true)
	d.natsRetryCount.Add(1)

	go d.reconcileOnHeal("nats-reconnect")
}

// execCommand wraps exec.Command so tests can substitute a fake implementation.
var execCommand = exec.Command

// getSystemMemory returns the total system memory in GB
func getSystemMemory() (float64, error) {
	switch runtime.GOOS {
	case "darwin":
		// macOS: use sysctl
		cmd := execCommand("sysctl", "-n", "hw.memsize")
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
		cmd := execCommand("grep", "MemTotal", "/proc/meminfo")
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

// physicalCoreCount returns the number of physical CPU cores by parsing
// distinct (physical id, core id) pairs from /proc/cpuinfo. Falls back to
// runtime.NumCPU() on non-Linux or when topology fields are absent.
func physicalCoreCount() int {
	logical := runtime.NumCPU()
	if runtime.GOOS != "linux" {
		return logical
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		slog.Warn("physical core detection failed, scheduling against logical CPUs",
			"err", err, "logicalCPU", logical)
		return logical
	}
	if n, ok := parsePhysicalCores(data); ok {
		return n
	}
	return logical
}

// parsePhysicalCores counts distinct (physical id, core id) pairs in
// /proc/cpuinfo, collapsing SMT siblings. Returns ok=false when topology
// fields are absent so the caller can fall back to the logical CPU count.
func parsePhysicalCores(data []byte) (int, bool) {
	cores := make(map[string]struct{})
	var phys, core string
	sawCore := false
	flush := func() {
		if core != "" {
			cores[phys+":"+core] = struct{}{}
		}
		phys, core = "", ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "physical id":
			phys = strings.TrimSpace(val)
		case "core id":
			core = strings.TrimSpace(val)
			sawCore = true
		}
	}
	flush()
	if !sawCore || len(cores) == 0 {
		return 0, false
	}
	return len(cores), true
}

// NewResourceManager creates a new ResourceManager. Errors if memory detection
// fails or if the host is too small to satisfy the daemon's reserve.
func NewResourceManager(gpuModels []instancetypes.GPUModel, migProfiles []instancetypes.MIGProfileSpec, gpuMgr *gpu.Manager) (*ResourceManager, error) {
	// Use physical cores (not SMT threads); SPINIFEX_HOST_VCPU overrides.
	hostVCPU := resolveHostVCPU(os.Getenv, physicalCoreCount())

	totalMemGB, err := getSystemMemory()
	if err != nil {
		return nil, fmt.Errorf("detect system memory: %w", err)
	}

	reserve := resolveHostReserve(os.Getenv)
	reservedVCPU, reservedMem, err := applyHostReserve(reserve, hostVCPU, totalMemGB)
	if err != nil {
		slog.Error("host below minimum reserve — daemon refuses to start",
			"err", err, "hostVCPU", hostVCPU, "hostMemGB", totalMemGB,
			"reserveVCPU", reserve.vCPU, "reserveMemGB", reserve.memGB)
		return nil, fmt.Errorf("validate host reserve: %w", err)
	}

	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}

	instanceTypes := instancetypes.DetectAndGenerate(instancetypes.HostCPU{}, arch, gpuModels)
	if len(migProfiles) > 0 {
		maps.Copy(instanceTypes, instancetypes.GenerateMIGTypes(migProfiles, arch))
	}

	slog.Info("System resources detected",
		"hostVCPU", hostVCPU, "logicalCPU", runtime.NumCPU(), "hostMemGB", totalMemGB,
		"reservedVCPU", reservedVCPU, "reservedMemGB", reservedMem,
		"schedulableVCPU", hostVCPU-reservedVCPU, "schedulableMemGB", totalMemGB-reservedMem,
		"instanceTypes", len(instanceTypes))

	var memReader func() (float64, bool)
	if liveMemAdmissionEnabled(os.Getenv) {
		memReader = readMemAvailableGB
	}

	nbdkitMainMiB, nbdkitAuxMiB := resolveNbdkitCharge(os.Getenv)

	return &ResourceManager{
		hostVCPU:           hostVCPU,
		hostMemGB:          totalMemGB,
		reservedVCPU:       reservedVCPU,
		reservedMem:        reservedMem,
		nbdkitMainMiB:      nbdkitMainMiB,
		nbdkitAuxMiB:       nbdkitAuxMiB,
		instanceTypes:      instanceTypes,
		gpuManager:         gpuMgr,
		readMemAvailableGB: memReader,
		reservations:       make(map[string]*capacityReservation),
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

// instanceMemChargeMiB is the full per-instance memory charge: guest -m plus
// nbdkit processes for its volumes. All admission gates use this value.
func (rm *ResourceManager) instanceMemChargeMiB(it *ec2.InstanceTypeInfo) int64 {
	return instanceTypeMemoryMiB(it) +
		nbdkitChargeMiB(defaultMainVolumes, defaultAuxVolumes, rm.nbdkitMainMiB, rm.nbdkitAuxMiB)
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

		// GPU types are capacity-gated by GPU pool size, not host CPU/memory.
		// The GPU is the scarce resource; CPU/memory on GPU-class hardware is abundant.
		if instancetypes.IsGPUType(it) {
			availGPU := 0
			if rm.gpuManager != nil {
				availGPU = rm.gpuManager.Available()
			}
			gpusNeeded := instancetypes.GPUCountForType(name)
			count := 0
			if gpusNeeded > 0 {
				count = availGPU / gpusNeeded
			}
			if showCapacity {
				for range count {
					infos = append(infos, it)
				}
			} else if count > 0 {
				infos = append(infos, it)
			}
			continue
		}

		count := canAllocateCount(
			rm.hostVCPU-rm.reservedVCPU-rm.reservedCRVCPU, rm.allocatedVCPU,
			rm.hostMemGB-rm.reservedMem-rm.reservedCRMem, rm.allocatedMem,
			vCPUs, rm.instanceMemChargeMiB(it),
			1<<30, // effectively unlimited — let resources be the constraint
			0, false,
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
		"reservedCRVCPU", rm.reservedCRVCPU, "reservedCRMem", rm.reservedCRMem,
		"showCapacity", showCapacity)

	return infos
}

// GetSupportedInstanceTypeInfos returns every supported instance type regardless
// of current capacity, mirroring AWS DescribeInstanceTypes semantics. System
// types and entries with incomplete metadata are skipped.
func (rm *ResourceManager) GetSupportedInstanceTypeInfos() []*ec2.InstanceTypeInfo {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var infos []*ec2.InstanceTypeInfo
	for name, it := range rm.instanceTypes {
		if instancetypes.IsSystemType(name) {
			continue
		}
		if instanceTypeVCPUs(it) == 0 || instanceTypeMemoryMiB(it) == 0 {
			continue
		}
		infos = append(infos, it)
	}

	slog.Info("GetSupportedInstanceTypeInfos", "total_types", len(rm.instanceTypes), "supported", len(infos))
	return infos
}

// GetResourceStats returns host resource figures, reservation, allocation, and
// per-type capacity caps for the node status response.
func (rm *ResourceManager) GetResourceStats() (totalVCPU int, totalMemGB float64, reservedVCPU int, reservedMemGB float64, allocVCPU int, allocMemGB float64, caps []types.InstanceTypeCap) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	totalVCPU = rm.hostVCPU
	totalMemGB = rm.hostMemGB
	// Fold the capacity-reservation carve-out into the reported reserve figures;
	// Phase 1 has no separate status field for it.
	reservedVCPU = rm.reservedVCPU + rm.reservedCRVCPU
	reservedMemGB = rm.reservedMem + rm.reservedCRMem
	allocVCPU = rm.allocatedVCPU
	allocMemGB = rm.allocatedMem

	remainingVCPU := rm.hostVCPU - reservedVCPU - rm.allocatedVCPU
	remainingMem := rm.hostMemGB - reservedMemGB - rm.allocatedMem
	if remainingVCPU < 0 || remainingMem < 0 {
		slog.Error("schedulable capacity negative — reserve misconfigured or allocation drift",
			"hostVCPU", rm.hostVCPU, "reservedVCPU", reservedVCPU, "allocatedVCPU", rm.allocatedVCPU,
			"hostMemGB", rm.hostMemGB, "reservedMem", reservedMemGB, "allocatedMem", rm.allocatedMem,
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
	var gpuMigProfiles []instancetypes.MIGProfileSpec
	var gpuMgr *gpu.Manager
	if nodeCfg.Daemon.GPUPassthrough {
		if !gpuProbe.Capable {
			slog.Warn("GPU passthrough enabled in config but prerequisites not met",
				"iommu", gpuProbe.IOMMUActive, "vfio", gpuProbe.VFIOPresent,
				"gpus", len(gpuProbe.Devices))
		} else {
			gpuMgr, gpuModels, gpuMigProfiles = buildGPUPool(gpuProbe.Devices, nodeCfg.Daemon)
		}
	} else if gpuProbe.Capable {
		slog.Info("GPU hardware detected, passthrough not enabled",
			"gpus", len(gpuProbe.Devices), "hint", "run 'spx admin gpu enable' to activate")
	}

	rm, err := NewResourceManager(gpuModels, gpuMigProfiles, gpuMgr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize resource manager: %w", err)
	}

	d := &Daemon{
		node:                 cfg.Node,
		clusterConfig:        cfg,
		config:               &nodeCfg,
		resourceMgr:          rm,
		gpuProbe:             gpuProbe,
		gpuManager:           gpuMgr,
		ctx:                  ctx,
		cancel:               cancel,
		vmMgr:                vm.NewManager(),
		natsSubscriptions:    make(map[string]*nats.Subscription),
		startTime:            time.Now(),
		detachDelay:          1 * time.Second,
		deviceDeletedTimeout: 15 * time.Second,
		requireNATSTimeout:   30 * time.Second,
		exitFunc:             os.Exit,
	}
	// Initialise peersReachable true so the first probe tick never fires a
	// spurious reconcileOnHeal at startup. Mode() still requires natsConnected
	// (starts false), so this can't falsely report cluster mode.
	d.peersReachable.Store(true)
	return d, nil
}

// natsMetricsHandler wraps a NATS handler to record request count and
// duration under the given action. Handler outcome is not observable at
// this chokepoint, so the outcome attribute is omitted.
func natsMetricsHandler(action string, h nats.MsgHandler) nats.MsgHandler {
	return func(msg *nats.Msg) {
		start := time.Now()
		h(msg)
		otelsetup.RecordRequest(context.Background(), action, "", time.Since(start))
	}
}

// natsMetricAction strips the node name from a topic so node-targeted
// subjects share one low-cardinality metric action across the cluster.
func natsMetricAction(topic, node string) string {
	if node == "" {
		return topic
	}
	action := strings.ReplaceAll(topic, "."+node+".", ".")
	return strings.TrimSuffix(action, "."+node)
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
		{"imds.ec2.get_public_key", d.handleIMDSGetPublicKey, "spinifex-workers"},
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
		{"ec2.CreateLaunchTemplate", d.handleEC2CreateLaunchTemplate, "spinifex-workers"},
		{"ec2.CreateLaunchTemplateVersion", d.handleEC2CreateLaunchTemplateVersion, "spinifex-workers"},
		{"ec2.DeleteLaunchTemplate", d.handleEC2DeleteLaunchTemplate, "spinifex-workers"},
		{"ec2.DeleteLaunchTemplateVersions", d.handleEC2DeleteLaunchTemplateVersions, "spinifex-workers"},
		{"ec2.ModifyLaunchTemplate", d.handleEC2ModifyLaunchTemplate, "spinifex-workers"},
		{"ec2.DescribeLaunchTemplates", d.handleEC2DescribeLaunchTemplates, "spinifex-workers"},
		{"ec2.DescribeLaunchTemplateVersions", d.handleEC2DescribeLaunchTemplateVersions, "spinifex-workers"},
		{"ec2.PutSpotInstanceRequests", d.handleEC2PutSpotInstanceRequests, "spinifex-workers"},
		{"ec2.DescribeSpotInstanceRequests", d.handleEC2DescribeSpotInstanceRequests, "spinifex-workers"},
		{"ec2.CancelSpotInstanceRequests", d.handleEC2CancelSpotInstanceRequests, "spinifex-workers"},
		// Capacity reservations: Create is node-targeted (gateway pins one node);
		// Describe fans out and Cancel broadcasts, so both use plain Subscribe.
		{fmt.Sprintf("ec2.CreateCapacityReservation.%s", d.node), d.handleEC2CreateCapacityReservation, ""},
		{"ec2.DescribeCapacityReservations", d.handleEC2DescribeCapacityReservations, ""},
		{"ec2.CancelCapacityReservation", d.handleEC2CancelCapacityReservation, ""},
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
		{"ec2.DescribeSecurityGroupRules", d.handleEC2DescribeSecurityGroupRules, "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupIngress", d.handleEC2AuthorizeSecurityGroupIngress, "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupEgress", d.handleEC2AuthorizeSecurityGroupEgress, "spinifex-workers"},
		{"ec2.RevokeSecurityGroupIngress", d.handleEC2RevokeSecurityGroupIngress, "spinifex-workers"},
		{"ec2.RevokeSecurityGroupEgress", d.handleEC2RevokeSecurityGroupEgress, "spinifex-workers"},
		{"ec2.ModifyInstanceAttribute", d.handleEC2ModifyInstanceAttribute, "spinifex-workers"},
		{"ec2.ModifyInstanceMetadataOptions", d.handleEC2ModifyInstanceMetadataOptions, "spinifex-workers"},
		{"ec2.start", d.handleEC2StartStoppedInstance, "spinifex-workers"},
		{fmt.Sprintf("ec2.start.%s", d.node), d.handleEC2StartStoppedInstanceDirect, ""},
		{"ec2.terminate", d.handleEC2TerminateStoppedInstance, "spinifex-workers"},
		{"ec2.DescribeStoppedInstances", d.handleEC2DescribeStoppedInstances, "spinifex-workers"},
		{"ec2.DescribeTerminatedInstances", d.handleEC2DescribeTerminatedInstances, "spinifex-workers"},
		// these fan out to all nodes and gateway aggregates the results. The
		// handler only sees per-daemon local state (vmMgr/stoppedStore), so
		// any queue-grouped routing produces 1/N false NotFound responses.
		{"ec2.DescribeInstances", d.handleEC2DescribeInstances, ""},
		{"ec2.DescribeInstanceStatus", d.handleEC2DescribeInstanceStatus, ""},
		{"ec2.DescribeInstanceTypes", d.handleEC2DescribeInstanceTypes, ""},
		{"ec2.DescribeInstanceAttribute", d.handleEC2DescribeInstanceAttribute, ""},
		// IAM instance profile associations: Disassociate/Replace mutate the
		// owning daemon's vm.VM (non-owners NoOp with Found=false); Describe
		// returns per-daemon matches that the gateway concatenates.
		{"ec2.IamProfileAssociation.disassociate", d.handleIamProfileDisassociate, ""},
		{"ec2.IamProfileAssociation.replace", d.handleIamProfileReplace, ""},
		{"ec2.IamProfileAssociation.describe", d.handleIamProfileDescribe, ""},
		{"ec2.EnableEbsEncryptionByDefault", d.handleEC2EnableEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.DisableEbsEncryptionByDefault", d.handleEC2DisableEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.GetEbsEncryptionByDefault", d.handleEC2GetEbsEncryptionByDefault, "spinifex-workers"},
		{"ec2.GetSerialConsoleAccessStatus", d.handleEC2GetSerialConsoleAccessStatus, "spinifex-workers"},
		{"ec2.EnableSerialConsoleAccess", d.handleEC2EnableSerialConsoleAccess, "spinifex-workers"},
		{"ec2.DisableSerialConsoleAccess", d.handleEC2DisableSerialConsoleAccess, "spinifex-workers"},
		{fmt.Sprintf("spinifex.admin.%s.health", d.node), d.handleHealthCheck, ""},
		{"spinifex.nodes.discover", d.handleNodeDiscover, ""},
		{"spinifex.node.status", d.handleNodeStatus, ""},
		{"spinifex.node.vms", d.handleNodeVMs, ""},
		{"spinifex.storage.config", d.handleStorageConfig, ""},
		{"spinifex.image.promote", d.handleSpinifexPromoteImage, "spinifex-workers"},
		// Account creation → create default VPC for new account
		{"iam.account.created", d.handleAccountCreated, "spinifex-workers"},
		// Coordinated cluster shutdown phases (fan-out, no queue group)
		{"spinifex.cluster.shutdown.gate", d.handleShutdownGate, ""},
		{"spinifex.cluster.shutdown.drain", d.handleShutdownDrain, ""},
		{"spinifex.cluster.shutdown.storage", d.handleShutdownStorage, ""},
		{"spinifex.cluster.shutdown.persist", d.handleShutdownPersist, ""},
		{"spinifex.cluster.shutdown.infra", d.handleShutdownInfra, ""},
	}

	// ELBv2 operations require a resolved gateway URL.
	// Without a subscriber the gateway returns nats.ErrNoResponders → ServiceUnavailable.
	if d.elbv2Service.GatewayURL != "" {
		subs = append(subs,
			natsSub{"elbv2.CreateLoadBalancer", d.handleELBv2CreateLoadBalancer, "spinifex-workers"},
			natsSub{"elbv2.DeleteLoadBalancer", d.handleELBv2DeleteLoadBalancer, "spinifex-workers"},
			natsSub{"elbv2.DescribeLoadBalancers", d.handleELBv2DescribeLoadBalancers, "spinifex-workers"},
			natsSub{"elbv2.CreateTargetGroup", d.handleELBv2CreateTargetGroup, "spinifex-workers"},
			natsSub{"elbv2.ModifyTargetGroup", d.handleELBv2ModifyTargetGroup, "spinifex-workers"},
			natsSub{"elbv2.DeleteTargetGroup", d.handleELBv2DeleteTargetGroup, "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetGroups", d.handleELBv2DescribeTargetGroups, "spinifex-workers"},
			natsSub{"elbv2.RegisterTargets", d.handleELBv2RegisterTargets, "spinifex-workers"},
			natsSub{"elbv2.DeregisterTargets", d.handleELBv2DeregisterTargets, "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetHealth", d.handleELBv2DescribeTargetHealth, "spinifex-workers"},
			natsSub{"elbv2.CreateListener", d.handleELBv2CreateListener, "spinifex-workers"},
			natsSub{"elbv2.DeleteListener", d.handleELBv2DeleteListener, "spinifex-workers"},
			natsSub{"elbv2.ModifyListener", d.handleELBv2ModifyListener, "spinifex-workers"},
			natsSub{"elbv2.DescribeListeners", d.handleELBv2DescribeListeners, "spinifex-workers"},
			natsSub{"elbv2.CreateRule", d.handleELBv2CreateRule, "spinifex-workers"},
			natsSub{"elbv2.ModifyRule", d.handleELBv2ModifyRule, "spinifex-workers"},
			natsSub{"elbv2.DeleteRule", d.handleELBv2DeleteRule, "spinifex-workers"},
			natsSub{"elbv2.DescribeRules", d.handleELBv2DescribeRules, "spinifex-workers"},
			natsSub{"elbv2.SetRulePriorities", d.handleELBv2SetRulePriorities, "spinifex-workers"},
			natsSub{"elbv2.DescribeTags", d.handleELBv2DescribeTags, "spinifex-workers"},
			natsSub{"elbv2.AddTags", d.handleELBv2AddTags, "spinifex-workers"},
			natsSub{"elbv2.RemoveTags", d.handleELBv2RemoveTags, "spinifex-workers"},
			natsSub{"elbv2.LBAgentHeartbeat", d.handleELBv2LBAgentHeartbeat, "spinifex-workers"},
			natsSub{"elbv2.GetLBConfig", d.handleELBv2GetLBConfig, "spinifex-workers"},
			natsSub{"elbv2.ModifyTargetGroupAttributes", d.handleELBv2ModifyTargetGroupAttributes, "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetGroupAttributes", d.handleELBv2DescribeTargetGroupAttributes, "spinifex-workers"},
			natsSub{"elbv2.ModifyLoadBalancerAttributes", d.handleELBv2ModifyLoadBalancerAttributes, "spinifex-workers"},
			natsSub{"elbv2.DescribeLoadBalancerAttributes", d.handleELBv2DescribeLoadBalancerAttributes, "spinifex-workers"},
			natsSub{"elbv2.SetSecurityGroups", d.handleELBv2SetSecurityGroups, "spinifex-workers"},
			natsSub{"elbv2.SetIpAddressType", d.handleELBv2SetIpAddressType, "spinifex-workers"},
			natsSub{"elbv2.SetSubnets", d.handleELBv2SetSubnets, "spinifex-workers"},
			natsSub{"elbv2.AddListenerCertificates", d.handleELBv2AddListenerCertificates, "spinifex-workers"},
			natsSub{"elbv2.RemoveListenerCertificates", d.handleELBv2RemoveListenerCertificates, "spinifex-workers"},
			natsSub{"elbv2.DescribeListenerCertificates", d.handleELBv2DescribeListenerCertificates, "spinifex-workers"},
			natsSub{"elbv2.DescribeSSLPolicies", d.handleELBv2DescribeSSLPolicies, "spinifex-workers"},
		)
	}

	// EKS gateway → daemon subscriptions. Every handler currently returns
	// NotImplemented; topics are subscribed up-front so the wiring layer is
	// stable while real bodies land.
	if d.eksService != nil {
		subs = append(subs,
			natsSub{"eks.CreateCluster", d.handleEKSCreateCluster, "spinifex-workers"},
			natsSub{"eks.DescribeCluster", d.handleEKSDescribeCluster, "spinifex-workers"},
			natsSub{"eks.ListClusters", d.handleEKSListClusters, "spinifex-workers"},
			natsSub{"eks.UpdateClusterConfig", d.handleEKSUpdateClusterConfig, "spinifex-workers"},
			natsSub{"eks.UpdateClusterVersion", d.handleEKSUpdateClusterVersion, "spinifex-workers"},
			natsSub{"eks.DeleteCluster", d.handleEKSDeleteCluster, "spinifex-workers"},
			natsSub{"eks.CreateNodegroup", d.handleEKSCreateNodegroup, "spinifex-workers"},
			natsSub{"eks.DescribeNodegroup", d.handleEKSDescribeNodegroup, "spinifex-workers"},
			natsSub{"eks.ListNodegroups", d.handleEKSListNodegroups, "spinifex-workers"},
			natsSub{"eks.UpdateNodegroupConfig", d.handleEKSUpdateNodegroupConfig, "spinifex-workers"},
			natsSub{"eks.UpdateNodegroupVersion", d.handleEKSUpdateNodegroupVersion, "spinifex-workers"},
			natsSub{"eks.DeleteNodegroup", d.handleEKSDeleteNodegroup, "spinifex-workers"},
			natsSub{"eks.CreateAccessEntry", d.handleEKSCreateAccessEntry, "spinifex-workers"},
			natsSub{"eks.DescribeAccessEntry", d.handleEKSDescribeAccessEntry, "spinifex-workers"},
			natsSub{"eks.ListAccessEntries", d.handleEKSListAccessEntries, "spinifex-workers"},
			natsSub{"eks.UpdateAccessEntry", d.handleEKSUpdateAccessEntry, "spinifex-workers"},
			natsSub{"eks.DeleteAccessEntry", d.handleEKSDeleteAccessEntry, "spinifex-workers"},
			natsSub{"eks.AssociateAccessPolicy", d.handleEKSAssociateAccessPolicy, "spinifex-workers"},
			natsSub{"eks.DisassociateAccessPolicy", d.handleEKSDisassociateAccessPolicy, "spinifex-workers"},
			natsSub{"eks.ListAssociatedAccessPolicies", d.handleEKSListAssociatedAccessPolicies, "spinifex-workers"},
			natsSub{"eks.ListAccessPolicies", d.handleEKSListAccessPolicies, "spinifex-workers"},
			natsSub{"eks.ListAddons", d.handleEKSListAddons, "spinifex-workers"},
			natsSub{"eks.DescribeAddonVersions", d.handleEKSDescribeAddonVersions, "spinifex-workers"},
			natsSub{"eks.CreateAddon", d.handleEKSCreateAddon, "spinifex-workers"},
			natsSub{"eks.DeleteAddon", d.handleEKSDeleteAddon, "spinifex-workers"},
			natsSub{"eks.DescribeAddon", d.handleEKSDescribeAddon, "spinifex-workers"},
			natsSub{"eks.UpdateAddon", d.handleEKSUpdateAddon, "spinifex-workers"},
			natsSub{"eks.ListStagedAddonManifests", d.handleEKSListStagedAddonManifests, "spinifex-workers"},
			natsSub{"eks.GetRecoveryDirective", d.handleEKSGetRecoveryDirective, "spinifex-workers"},
			natsSub{"eks.SetRecoveryDirective", d.handleEKSSetRecoveryDirective, "spinifex-workers"},
			natsSub{"eks.AssociateIdentityProviderConfig", d.handleEKSAssociateIdentityProviderConfig, "spinifex-workers"},
			natsSub{"eks.DescribeIdentityProviderConfig", d.handleEKSDescribeIdentityProviderConfig, "spinifex-workers"},
			natsSub{"eks.ListIdentityProviderConfigs", d.handleEKSListIdentityProviderConfigs, "spinifex-workers"},
			natsSub{"eks.DisassociateIdentityProviderConfig", d.handleEKSDisassociateIdentityProviderConfig, "spinifex-workers"},
			natsSub{"eks.TagResource", d.handleEKSTagResource, "spinifex-workers"},
			natsSub{"eks.UntagResource", d.handleEKSUntagResource, "spinifex-workers"},
			natsSub{"eks.ListTagsForResource", d.handleEKSListTagsForResource, "spinifex-workers"},
		)
	}

	// ECS gateway → daemon subscriptions (control plane; per-account KV).
	if d.ecsService != nil {
		subs = append(subs,
			natsSub{"ecs.CreateCluster", d.handleECSCreateCluster, "spinifex-workers"},
			natsSub{"ecs.DeleteCluster", d.handleECSDeleteCluster, "spinifex-workers"},
			natsSub{"ecs.DescribeClusters", d.handleECSDescribeClusters, "spinifex-workers"},
			natsSub{"ecs.ListClusters", d.handleECSListClusters, "spinifex-workers"},
			natsSub{"ecs.RegisterTaskDefinition", d.handleECSRegisterTaskDefinition, "spinifex-workers"},
			natsSub{"ecs.DeregisterTaskDefinition", d.handleECSDeregisterTaskDefinition, "spinifex-workers"},
			natsSub{"ecs.DescribeTaskDefinition", d.handleECSDescribeTaskDefinition, "spinifex-workers"},
			natsSub{"ecs.ListTaskDefinitions", d.handleECSListTaskDefinitions, "spinifex-workers"},
			natsSub{"ecs.RegisterContainerInstance", d.handleECSRegisterContainerInstance, "spinifex-workers"},
			natsSub{"ecs.DeregisterContainerInstance", d.handleECSDeregisterContainerInstance, "spinifex-workers"},
			natsSub{"ecs.UpdateContainerInstancesState", d.handleECSUpdateContainerInstancesState, "spinifex-workers"},
			natsSub{"ecs.DescribeContainerInstances", d.handleECSDescribeContainerInstances, "spinifex-workers"},
			natsSub{"ecs.ListContainerInstances", d.handleECSListContainerInstances, "spinifex-workers"},
			natsSub{"ecs.RunTask", d.handleECSRunTask, "spinifex-workers"},
			natsSub{"ecs.StartTask", d.handleECSStartTask, "spinifex-workers"},
			natsSub{"ecs.StopTask", d.handleECSStopTask, "spinifex-workers"},
			natsSub{"ecs.DescribeTasks", d.handleECSDescribeTasks, "spinifex-workers"},
			natsSub{"ecs.ListTasks", d.handleECSListTasks, "spinifex-workers"},
			natsSub{"ecs.CreateService", d.handleECSCreateService, "spinifex-workers"},
			natsSub{"ecs.UpdateService", d.handleECSUpdateService, "spinifex-workers"},
			natsSub{"ecs.DeleteService", d.handleECSDeleteService, "spinifex-workers"},
			natsSub{"ecs.DescribeServices", d.handleECSDescribeServices, "spinifex-workers"},
			natsSub{"ecs.ListServices", d.handleECSListServices, "spinifex-workers"},
			natsSub{"ecs.SubmitTaskStateChange", d.handleECSSubmitTaskStateChange, "spinifex-workers"},
			natsSub{"ecs.PollAssignments", d.handleECSPollAssignments, "spinifex-workers"},
			natsSub{"ecs.ProvisionCapacity", d.handleECSProvisionCapacity, "spinifex-workers"},
		)
	}

	// ACM gateway → daemon subscriptions (minimal certificate store).
	if d.acmService != nil {
		subs = append(subs,
			natsSub{"acm.ImportCertificate", d.handleACMImportCertificate, "spinifex-workers"},
			natsSub{"acm.DescribeCertificate", d.handleACMDescribeCertificate, "spinifex-workers"},
			natsSub{"acm.ListCertificates", d.handleACMListCertificates, "spinifex-workers"},
			natsSub{"acm.DeleteCertificate", d.handleACMDeleteCertificate, "spinifex-workers"},
			natsSub{"acm.ListTagsForCertificate", d.handleACMListTagsForCertificate, "spinifex-workers"},
			natsSub{"acm.AddTagsToCertificate", d.handleACMAddTagsToCertificate, "spinifex-workers"},
			natsSub{"acm.RemoveTagsFromCertificate", d.handleACMRemoveTagsFromCertificate, "spinifex-workers"},
		)
	}

	// ECR gateway → daemon subscriptions. The daemon owns the per-account
	// JetStream KV metadata; blob/manifest bytes never traverse these subjects.
	if d.ecrMetaService != nil {
		subs = append(subs,
			natsSub{handlers_ecr.SubjectRepoCreate, d.handleECRRepoCreate, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoDescribe, d.handleECRRepoDescribe, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoList, d.handleECRRepoList, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoDelete, d.handleECRRepoDelete, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyPut, d.handleECRPolicyPut, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyGet, d.handleECRPolicyGet, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyDelete, d.handleECRPolicyDelete, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecyclePut, d.handleECRLifecyclePut, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecycleGet, d.handleECRLifecycleGet, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecycleDelete, d.handleECRLifecycleDelete, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagPut, d.handleECRTagPut, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagGet, d.handleECRTagGet, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagList, d.handleECRTagList, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagDelete, d.handleECRTagDelete, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestPut, d.handleECRManifestPut, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestDescribe, d.handleECRManifestDescribe, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestList, d.handleECRManifestList, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestDelete, d.handleECRManifestDelete, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadCreate, d.handleECRUploadCreate, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadGet, d.handleECRUploadGet, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadUpdate, d.handleECRUploadUpdate, "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadDelete, d.handleECRUploadDelete, "spinifex-workers"},
		)
	}

	// EIP handlers are always registered: without external IPAM d.eipService is
	// the disabled stub, so reads return empty lists and mutations a clean
	// UnsupportedOperation instead of a NATS timeout.
	subs = append(subs,
		natsSub{"ec2.AllocateAddress", d.handleEC2AllocateAddress, "spinifex-workers"},
		natsSub{"ec2.ReleaseAddress", d.handleEC2ReleaseAddress, "spinifex-workers"},
		natsSub{"ec2.AssociateAddress", d.handleEC2AssociateAddress, "spinifex-workers"},
		natsSub{"ec2.DisassociateAddress", d.handleEC2DisassociateAddress, "spinifex-workers"},
		natsSub{"ec2.DescribeAddresses", d.handleEC2DescribeAddresses, "spinifex-workers"},
		natsSub{"ec2.DescribeAddressesAttribute", d.handleEC2DescribeAddressesAttribute, "spinifex-workers"},
	)

	for _, s := range subs {
		var sub *nats.Subscription
		var err error
		handler := natsMetricsHandler(natsMetricAction(s.topic, d.node), s.handler)
		if s.queueGroup != "" {
			sub, err = d.natsConn.QueueSubscribe(s.topic, s.queueGroup, handler)
		} else {
			sub, err = d.natsConn.Subscribe(s.topic, handler)
		}
		if err != nil {
			return fmt.Errorf("failed to subscribe to %s: %w", s.topic, err)
		}
		d.natsSubscriptions[s.topic] = sub
		slog.Info("Subscribed to NATS topic", "topic", s.topic, "queue", s.queueGroup)
	}
	return nil
}

// Start bootstraps the daemon in two phases (DDIL Tier 1): startLocal brings
// up HTTPS + local state without NATS, then startCluster retries NATS
// indefinitely and joins the cluster once connected.
func (d *Daemon) Start() error {
	if err := d.startLocal(); err != nil {
		return err
	}

	d.setupShutdown()

	d.shutdownWg.Go(func() {
		if err := d.startCluster(); err != nil {
			slog.Warn("Cluster bootstrap aborted", "err", err)
		}
	})

	d.awaitShutdown()
	return nil
}

// startLocal performs the no-NATS bootstrap. Failures here are fatal (local
// config errors that retry cannot fix). The daemon is reachable via /local/*
// and /health once this returns.
//
// DDIL §1e-audit: no JetStream KV must be touched here. All KV buckets are
// initialised in startCluster. assertNoClusterServicesInitialised enforces this.
func (d *Daemon) startLocal() error {
	// ClusterManager serves /health and /local/* over HTTPS. NATS-independent.
	if err := d.ClusterManager(); err != nil {
		return fmt.Errorf("failed to start cluster manager: %w", err)
	}

	// Detect management bridge for system instance control plane NICs.
	mgmtBridge := "br-mgmt"
	if d.config.Daemon.MgmtBridge != "" {
		mgmtBridge = d.config.Daemon.MgmtBridge
	}
	bridgeIP, bridgeErr := host.GetBridgeIPv4(mgmtBridge)
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

	// Initialise OVS network plumber (no NATS dep).
	if d.networkPlumber == nil {
		d.networkPlumber = host.NewOVSPlumber()
	}

	// Protect daemon from OOM killer (prefer killing QEMU VMs instead).
	if err := utils.SetOOMScore(os.Getpid(), -500); err != nil {
		slog.Warn("Failed to set daemon OOM score", "err", err)
	}

	// Recover local instance state from disk. Fatal on corruption — orphaned VMs.
	if err := d.LoadState(); err != nil {
		return fmt.Errorf("load local instance state: %w", err)
	}
	slog.Info("Loaded local instance state", "instance count", d.vmMgr.Count())

	// Rebuild mgmt IP allocator from restored VMs.
	if d.mgmtIPAllocator != nil {
		d.mgmtIPAllocator.Rebuild(d.vmMgr.SnapshotMap())
		slog.Info("Rebuilt mgmt IP allocator from restored instances", "allocated", d.mgmtIPAllocator.AllocatedCount())
	}

	if err := d.assertNoClusterServicesInitialised(); err != nil {
		return fmt.Errorf("startLocal Tier 1 invariant violated: %w", err)
	}

	// Peer-health probe is NATS-independent (dials /health directly) and must
	// start here so Mode() can detect partitions even if NATS never connects.
	d.shutdownWg.Go(d.monitorPeerReachability)

	d.ready.Store(true)
	slog.Info("Daemon local-bootstrap complete", "node", d.node, "elapsed", time.Since(d.startTime).Round(time.Second))
	return nil
}

// publicExternalPools filters out the routed-NAT transit pool: it carries
// gateway plumbing addresses, never allocatable public IPs.
func publicExternalPools(pools []config.ExternalPool) []config.ExternalPool {
	var public []config.ExternalPool
	for _, p := range pools {
		if p.Name == host.NATTransitPoolName {
			continue
		}
		public = append(public, p)
	}
	return public
}

// hasPublicIPPools reports whether the cluster can allocate routable public
// IPs: pool mode always, nat mode only with a public pool beside the transit.
func (d *Daemon) hasPublicIPPools() bool {
	if d.clusterConfig == nil {
		return false
	}
	switch d.clusterConfig.Network.ExternalMode {
	case "pool":
		return true
	case "nat":
		return len(publicExternalPools(d.clusterConfig.Network.ExternalPools)) > 0
	}
	return false
}

// assertNoClusterServicesInitialised enforces the DDIL §1e-audit invariant:
// no NATS-dependent handle may exist at the end of startLocal.
func (d *Daemon) assertNoClusterServicesInitialised() error {
	switch {
	case d.natsConn != nil:
		return errors.New("d.natsConn must be nil before startCluster")
	case d.jsManager != nil:
		return errors.New("d.jsManager must be nil before startCluster")
	case d.instanceService != nil:
		return errors.New("d.instanceService must be nil before startCluster")
	case d.imageService != nil:
		return errors.New("d.imageService must be nil before startCluster")
	case d.snapshotService != nil:
		return errors.New("d.snapshotService must be nil before startCluster")
	case d.volumeService != nil:
		return errors.New("d.volumeService must be nil before startCluster")
	case d.eigwService != nil:
		return errors.New("d.eigwService must be nil before startCluster")
	case d.igwService != nil:
		return errors.New("d.igwService must be nil before startCluster")
	case d.placementGroupService != nil:
		return errors.New("d.placementGroupService must be nil before startCluster")
	case d.launchTemplateService != nil:
		return errors.New("d.launchTemplateService must be nil before startCluster")
	case d.spotInstanceService != nil:
		return errors.New("d.spotInstanceService must be nil before startCluster")
	case d.vpcService != nil:
		return errors.New("d.vpcService must be nil before startCluster")
	case d.routeTableService != nil:
		return errors.New("d.routeTableService must be nil before startCluster")
	case d.natGatewayService != nil:
		return errors.New("d.natGatewayService must be nil before startCluster")
	case d.externalIPAM != nil:
		return errors.New("d.externalIPAM must be nil before startCluster")
	case d.eipService != nil:
		return errors.New("d.eipService must be nil before startCluster")
	case d.accountService != nil:
		return errors.New("d.accountService must be nil before startCluster")
	case d.elbv2Service != nil:
		return errors.New("d.elbv2Service must be nil before startCluster")
	case d.eksService != nil:
		return errors.New("d.eksService must be nil before startCluster")
	}
	return nil
}

// startCluster retries NATS indefinitely and initialises all cluster-scoped
// services. Errors are logged, not fatal. All JetStream KV buckets (DDIL
// §1e-audit) must be initialised here, never in startLocal.
func (d *Daemon) startCluster() error {
	if os.Getenv("SPINIFEX_REQUIRE_NATS") == "1" {
		// §1d-strict opt-in: bounded first connect, abort on timeout. Restores
		// the pre-DDIL fail-fast UX for dev/test/single-node deploys without
		// flipping the prod default (which would re-introduce the SPOF that 1d
		// removed).
		if err := d.connectNATS(utils.WithMaxWait(d.requireNATSTimeout)); err != nil {
			slog.Error("SPINIFEX_REQUIRE_NATS=1 set, NATS connect failed within 30s, aborting", "err", err, "timeout", d.requireNATSTimeout)
			d.exitFunc(1)
			return fmt.Errorf("connect NATS (strict): %w", err)
		}
	} else if err := d.connectNATS(); err != nil {
		return fmt.Errorf("connect NATS: %w", err)
	}

	if err := d.initJetStream(); err != nil {
		return fmt.Errorf("initialize JetStream: %w", err)
	}

	// Remove the obsolete spinifex-dhcp-leases bucket (idempotent).
	if js, jsErr := d.natsConn.JetStream(); jsErr == nil {
		if err := utils.DeleteKVBucketIfExists(js, "spinifex-dhcp-leases"); err != nil {
			slog.Warn("Failed to delete obsolete spinifex-dhcp-leases KV bucket", "err", err)
		}
	}

	// Enable OVN native IPsec when configured (idempotent).
	if d.clusterConfig != nil && d.clusterConfig.Network.IPSecEnabled {
		if err := host.EnableOVNIPSec(d.configPath, d.clusterConfig); err != nil {
			slog.Warn("Failed to enable OVN native IPsec; intra-AZ Geneve will be plaintext", "err", err)
		}
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
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(d.config, d.resourceMgr.instanceTypes, d.natsConn, store, d.vmMgr, d.resourceMgr, d.jsManager)
	d.keyService = handlers_ec2_key.NewKeyServiceImpl(d.config)
	d.imageService = handlers_ec2_image.NewImageServiceImpl(d.config)

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

	d.launchTemplateService, err = initServiceWithRetry("launch template service", func() (*handlers_ec2_launchtemplate.LaunchTemplateServiceImpl, error) {
		return handlers_ec2_launchtemplate.NewLaunchTemplateServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize launch template service: %w", err)
	}

	d.spotInstanceService, err = initServiceWithRetry("spot instance service", func() (*handlers_ec2_spotinstance.SpotInstanceServiceImpl, error) {
		return handlers_ec2_spotinstance.NewSpotInstanceServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize spot instance service: %w", err)
	}

	d.vpcService, err = initServiceWithRetry("VPC service", func() (*handlers_ec2_vpc.VPCServiceImpl, error) {
		return handlers_ec2_vpc.NewVPCServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize VPC service: %w", err)
	}
	// Default subnets request a public IP on launch only when the cluster has
	// pools that hand out routable public IPs (pool mode always; nat mode only
	// when a public pool rides alongside the transit segment).
	d.vpcService.SetDefaultPublicIPMapping(d.hasPublicIPPools())

	d.routeTableService, err = initServiceWithRetry("RouteTable service", func() (*handlers_ec2_routetable.RouteTableServiceImpl, error) {
		return handlers_ec2_routetable.NewRouteTableServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize RouteTable service: %w", err)
	}

	// Wire IGW attach/detach to RT-aware per-subnet egress gate fan-out.
	d.igwService.SetGatePublisher(d.routeTableService)

	d.natGatewayService, err = initServiceWithRetry("NatGateway service", func() (*handlers_ec2_natgw.NatGatewayServiceImpl, error) {
		return handlers_ec2_natgw.NewNatGatewayServiceImplWithNATS(d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize NatGateway service: %w", err)
	}

	// Initialize external IPAM when public IP pools exist (pool mode, or nat
	// mode with a public pool alongside the transit segment). The transit pool
	// never enters IPAM — its addresses are gateway-LRP plumbing, not EIPs.
	if d.hasPublicIPPools() {
		js, jsErr := d.natsConn.JetStream()
		if jsErr != nil {
			slog.Warn("Failed to get JetStream for external IPAM", "err", jsErr)
		} else {
			var pools []external.ExternalPoolConfig
			anyDHCP := false
			for _, p := range publicExternalPools(d.clusterConfig.Network.ExternalPools) {
				pools = append(pools, external.ExternalPoolConfig{
					Name:            p.Name,
					Source:          p.Source,
					BindBridge:      p.BindBridge,
					DHCPMAC:         p.DHCPMAC,
					RangeStart:      p.RangeStart,
					RangeEnd:        p.RangeEnd,
					Gateway:         p.Gateway,
					GatewayIP:       p.GatewayIP,
					PrefixLen:       p.PrefixLen,
					Region:          p.Region,
					AZ:              p.AZ,
					GwLrpRangeStart: p.GwLrpRangeStart,
					GwLrpRangeEnd:   p.GwLrpRangeEnd,
				})
				if p.Source == "dhcp" {
					anyDHCP = true
				}
			}
			d.externalIPAM, err = handlers_ec2_vpc.NewExternalIPAM(js, pools)
			if err != nil {
				slog.Warn("Failed to initialize external IPAM", "err", err)
			} else {
				if anyDHCP {
					dhcpClient := dhcp.NewNATSClient(d.natsConn, 0)
					if dhcpErr := d.externalIPAM.EnableDHCP(dhcpClient); dhcpErr != nil {
						slog.Warn("Failed to enable DHCP allocator on external IPAM", "err", dhcpErr)
					}
				}
				slog.Info("External IPAM initialized", "mode", d.clusterConfig.Network.ExternalMode, "pools", len(pools), "dhcp", anyDHCP)
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

	// Without external IPAM (nat mode or external disabled) serve EIP requests
	// from the disabled stub so the API surface stays registered.
	if d.eipService == nil {
		d.eipService = handlers_ec2_eip.NewDisabledEIPService()
		slog.Info("EIP service disabled — no external IPAM; serving empty/unsupported responses")
	}

	d.instanceService.SetTerminationDeps(d.volumeService, d.vpcService, d.externalIPAM, d.tagsService)
	d.instanceService.SetRunInstancesDeps(d.imageService, d.keyService, &daemonENICreator{d: d}, d.externalIPAM)

	if d.gpuManager != nil {
		d.instanceService.SetGPUClaimer(&daemonGPUClaimer{d: d})
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

	// Route system VM launches through NATS so they fan out across the cluster.
	d.elbv2Service.InstanceLauncher = handlers_elbv2.NewNATSSystemInstanceLauncher(d.natsConn, 0)

	// Provide a lazily-built KV-backed IAM service so an LB VM gets a system
	// instance profile and authenticates with IMDS instance-role creds. The
	// provider is resolved at LB-launch time, not now, so it cannot race the
	// NATS KV backend coming up; absent (no master key) the LB VM falls back to
	// baked static creds.
	d.elbv2Service.IAMProvider = d.systemRoleEnsurer

	d.wireLBAgentConfig()

	d.elbv2Service.SetSystemInstanceTypeFunc(func() string {
		return "sys.micro"
	})

	// Invalidate stale "healthy" target state from before restart. Best-effort.
	if err := d.elbv2Service.ResetTargetHealthOnStartup(context.Background()); err != nil {
		slog.Warn("ELBv2: target-health reset failed; continuing with stale state",
			"err", err)
	}

	d.eksService, err = initServiceWithRetry("EKS service", func() (*handlers_eks.EKSServiceImpl, error) {
		return handlers_eks.NewEKSServiceImpl(d.buildEKSServiceDeps())
	})
	if err != nil {
		return fmt.Errorf("failed to initialize EKS service: %w", err)
	}

	// ECS control plane: per-account KV-backed handlers + a leader-elected
	// scheduler goroutine that owns the Layer-2 bus subscriptions and heartbeat
	// reaper. The scheduler is disabled (handlers still serve) when JetStream is
	// unavailable.
	d.ecsService = handlers_ecs.NewService(d.natsConn, d.config.Region, d.clusterConfig.AWS.InternalSuffix).WithDeps(d.buildECSServiceDeps())
	if js, jsErr := d.natsConn.JetStream(); jsErr != nil {
		slog.Warn("ECS scheduler disabled: JetStream unavailable", "err", jsErr)
	} else if _, lbErr := handlers_ecs.InitLeaderBucket(js); lbErr != nil {
		slog.Warn("ECS scheduler disabled: leader bucket init failed", "err", lbErr)
	} else {
		d.ecsScheduler = handlers_ecs.NewScheduler(d.natsConn, d.ecsService, d.node)
		d.shutdownWg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("ECS scheduler goroutine panicked", "recover", r)
				}
			}()
			d.ecsScheduler.Run(d.ctx)
		})
	}

	d.acmService, err = initServiceWithRetry("ACM service", func() (*handlers_acm.ACMServiceImpl, error) {
		return handlers_acm.NewACMServiceImplWithNATS(d.config, d.natsConn)
	})
	if err != nil {
		return fmt.Errorf("failed to initialize ACM service: %w", err)
	}

	// ECR metadata service: owns per-account JetStream KV for repos, tags,
	// manifest records and upload-state CAS. Disabled (gateway returns NATS
	// timeouts) when JetStream is unavailable.
	if js, jsErr := d.natsConn.JetStream(); jsErr != nil {
		slog.Warn("ECR metadata service disabled: JetStream unavailable", "err", jsErr)
	} else {
		d.ecrMetaService = handlers_ecr.NewKVMetaService(js)
	}

	if err := d.eksService.SpawnRegisteredReconcilers(); err != nil {
		slog.Warn("EKS: SpawnRegisteredReconcilers failed", "err", err)
	}

	// Ensure default VPC exists for system and admin accounts
	// (matches AWS: every account has a default VPC with IGW + default SG)
	if d.vpcService != nil {
		failedDefaultVPCs := map[string]struct{}{}
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
				failedDefaultVPCs[accountID] = struct{}{}
			}
		}
		// Skip IGW/route setup for accounts whose default VPC failed; otherwise
		// we'd attach infrastructure to a half-built VPC (no default SG yet).
		d.ensureDefaultVPCInfrastructure(failedDefaultVPCs)
	}

	d.vmMgr.SetDeps(d.buildVMManagerDeps())

	d.waitForClusterReady()
	d.upgradeJetStreamReplicas()
	if err := d.restoreInstances(); err != nil {
		return fmt.Errorf("restore instances: %w", err)
	}

	// Rebuild mgmt IP allocator so already-allocated IPs aren't reused.
	if d.mgmtIPAllocator != nil {
		d.mgmtIPAllocator.Rebuild(d.vmMgr.SnapshotMap())
		slog.Info("Rebuilt mgmt IP allocator from restored instances", "allocated", d.mgmtIPAllocator.AllocatedCount())
	}

	if err := d.subscribeAll(); err != nil {
		return fmt.Errorf("failed to subscribe to NATS topics: %w", err)
	}

	// Initialize per-instance-type NATS subscriptions for capacity-aware routing.
	d.resourceMgr.initSubscriptions(d.natsConn, d.handleEC2RunInstances, d.handleSystemLaunchInstance, d.node)

	d.startHeartbeat()
	d.vmMgr.StartPendingWatchdog(d.ctx)

	// Reality→desired GC backstop (ADR-0003 §3): finish teardown interrupted by
	// a node-down mid-cascade and purge completed terminated records. The volume
	// data-safety reaper (ADR-0005 §3) rides the same backstop but only marks +
	// alarms — it never deletes volume data.
	if d.jsManager != nil {
		reapers := []vm.Reaper{
			d.vmMgr.NewTerminatedTeardownReaper(),
			d.vmMgr.NewOrphanQEMUReaper(),
		}
		if eniRec := d.newENIReconciler(); eniRec != nil {
			reapers = append(reapers, eniRec)
		}
		if d.volumeService != nil {
			reapers = append(reapers, d.volumeService.NewVolumeLeakReaper(d.leakedVolumeInstances))
		}
		if d.eksService != nil {
			reapers = append(reapers, d.eksService.NewBillableReaper(d.nodeRunningVMs))
			reapers = append(reapers, d.eksService.NewDeletingReaper())
		}
		gc := vm.NewGarbageCollector(d.jsManager.KVHealthy, reapers...)
		gc.Start(d.ctx)
	}

	d.ready.Store(true)
	slog.Info("Daemon fully initialized", "node", d.node, "startupTime", time.Since(d.startTime).Round(time.Second))

	d.setupReload()
	d.setupShutdown()
	d.awaitShutdown()

	return nil
}

// leakedVolumeInstances returns the set of instance IDs this node owns whose
// teardown leaked a volume — terminated here with a failed volumes-teardown.
// The volume data-safety reaper marks (never deletes) volumes still attached to
// these definitively-gone instances. Keying on this node's terminated set keeps
// the shared-store scan from false-marking another node's live-instance volume.
func (d *Daemon) leakedVolumeInstances() (map[string]bool, error) {
	terminated, err := d.jsManager.ListTerminatedInstances()
	if err != nil {
		return nil, err
	}
	leaked := make(map[string]bool)
	for _, v := range terminated {
		if v.LastNode == d.node && v.Teardown[vm.TeardownVolumes] == string(vm.TeardownFailed) {
			leaked[v.ID] = true
		}
	}
	return leaked, nil
}

// nodeRunningVMs returns this node's running VMs for the EKS billable reaper to
// scan. A nil stateStore (early init / test) yields an empty set.
func (d *Daemon) nodeRunningVMs() ([]*vm.VM, error) {
	if d.stateStore == nil {
		return nil, nil
	}
	running, err := d.stateStore.LoadRunningState(d.node)
	if err != nil {
		return nil, err
	}
	vms := make([]*vm.VM, 0, len(running))
	for _, v := range running {
		vms = append(vms, v)
	}
	return vms, nil
}

// connectNATS connects to NATS with infinite retry (cap 60s backoff). Tests
// override d.natsRetryOpts; extraOpts override any conflicting fields.
func (d *Daemon) connectNATS(extraOpts ...utils.RetryOption) error {
	opts := append([]utils.RetryOption{
		utils.WithMaxWait(0), // infinite retry; cancelled via d.ctx
		utils.WithMaxRetryDelay(60 * time.Second),
		utils.WithContext(d.ctx),
		utils.WithDisconnectHandler(d.onNATSDisconnect),
		utils.WithReconnectHandler(d.onNATSReconnect),
		utils.WithAttemptErrHandler(func(_ error, _ int) {
			d.natsRetryCount.Add(1)
		}),
	}, d.natsRetryOpts...)
	opts = append(opts, extraOpts...)
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(d.config.NATS.Host), d.config.NATS.ACL.Token, d.config.NATS.CACert, opts...)
	if err != nil {
		return err
	}
	d.natsConn = nc
	d.natsConnected.Store(true)
	return nil
}

// initJetStream initialises JetStream KV stores, retrying up to 5 minutes to
// allow late-joining nodes to reach quorum.
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
			d.jsManager.SetSyncObserver(d)
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

	return nil
}

// upgradeJetStreamReplicas bumps KV_* stream replication to match the cluster
// size. Runs after all buckets are created and the cluster is ready.
func (d *Daemon) upgradeJetStreamReplicas() {
	clusterSize := len(d.clusterConfig.Nodes)
	if clusterSize <= 1 || d.jsManager == nil {
		return
	}
	if err := d.jsManager.UpdateReplicas(clusterSize); err != nil {
		slog.Warn("Failed to upgrade JetStream replicas", "targetReplicas", clusterSize, "error", err)
	}
}

// initRetrySleep is the sleep seam used by initServiceWithRetry; tests override it.
var initRetrySleep = time.Sleep

// initServiceWithRetry initialises a service with exponential backoff (500ms→10s)
// for up to 5 minutes, allowing time for JetStream quorum during cluster restarts.
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
		initRetrySleep(retryDelay)
		retryDelay = min(retryDelay*2, 10*time.Second)
	}
}

// waitForClusterReady blocks until viperblock and predastore are reachable,
// preventing races during VM recovery.
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

// checkViperblockReady reports whether viperblock is reachable via NATS.
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

// LoadState loads instance state from disk. Missing file → empty map (fresh
// install). Corrupt or unknown-schema files are fatal.
func (d *Daemon) LoadState() error {
	path := d.localStatePath()
	state, err := ReadLocalState(path)
	if err != nil {
		slog.Error("Local state load failed", "path", path, "error", err)
		return fmt.Errorf("read local state: %w", err)
	}

	if state == nil {
		d.vmMgr.Replace(map[string]*vm.VM{})
		slog.Info("No local state file, starting with empty instance map", "path", path)
		return nil
	}

	d.vmMgr.Replace(state.VMS)
	slog.Info("Loaded local state", "path", path, "instances", len(state.VMS))
	return nil
}

// restoreInstances delegates to vm.Manager.Restore and syncs the local state
// file so it matches in-memory state.
func (d *Daemon) restoreInstances() error {
	d.vmMgr.Restore()
	if err := d.WriteState(); err != nil {
		slog.Error("Failed to persist local state after restore", "error", err)
	}
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

// computeConfigHash computes a SHA256 hash of the shared cluster config.
func (d *Daemon) computeConfigHash() (string, error) {
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

// routeActionMiddleware names cluster API requests by chi route pattern for
// request metrics, keeping metric attribute cardinality bounded.
func routeActionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if rc := chi.RouteContext(r.Context()); rc != nil {
			if pattern := rc.RoutePattern(); pattern != "" {
				otelsetup.SetRequestAction(r.Context(), r.Method+" "+pattern)
			}
		}
	})
}

// ClusterManager starts the HTTPS cluster management server.
func (d *Daemon) ClusterManager() error {
	daemonHost := d.config.Daemon.Host
	if daemonHost == "" {
		return fmt.Errorf("daemon.host not configured")
	}

	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("spinifex-daemon"))
	r.Use(routeActionMiddleware)

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
		if !d.config.HasService("nats") {
			if d.natsConn != nil && d.natsConn.IsConnected() {
				serviceHealth["nats"] = "remote_ok"
			} else {
				serviceHealth["nats"] = "remote_unreachable"
			}
		}

		ovnHealth := host.HealthStatus()
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

	d.registerLocalRoutes(r)

	// Resolve relative cert paths against the config directory.
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
		Certificates:     []tls.Certificate{cert},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
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

// kvSyncTimeout caps the best-effort KV sync; 1s is above healthy Put latency.
const kvSyncTimeout = time.Second

// localStatePath returns the on-disk path to this daemon's instance state file.
func (d *Daemon) localStatePath() string {
	if d.config == nil {
		return LocalStatePath("")
	}
	return LocalStatePath(d.config.DataDir)
}

// WriteState persists instance state. Local file is the source of truth; KV is
// best-effort. Both forms are marshalled inside vmMgr.View to avoid data races.
func (d *Daemon) WriteState() error {
	d.stateWriteMu.Lock()
	defer d.stateWriteMu.Unlock()

	var (
		localData, kvData []byte
		marshalErr        error
	)
	d.vmMgr.View(func(vms map[string]*vm.VM) {
		localData, marshalErr = MarshalLocalState(vms)
		if marshalErr != nil {
			return
		}
		kvData, marshalErr = marshalInstanceState(vms)
	})
	if marshalErr != nil {
		return fmt.Errorf("marshal state: %w", marshalErr)
	}

	path := d.localStatePath()
	if err := WriteLocalStateBytes(path, localData); err != nil {
		slog.Error("Local state write failed", "path", path, "error", err)
		return fmt.Errorf("write local state: %w", err)
	}
	d.stateRevision.Add(1)

	if d.jsManager != nil {
		d.jsManager.WriteStateBytesBestEffort(d.node, kvData, kvSyncTimeout)
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
		mgr, models, migProfiles := buildGPUPool(probe.Devices, d.config.Daemon)
		d.gpuManager = mgr
		d.resourceMgr.reloadGPUTypes(models, migProfiles, mgr)
		d.instanceService.SetGPUClaimer(&daemonGPUClaimer{d: d})
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
	d.instanceService.SetGPUClaimer(nil)
	d.resourceMgr.reloadGPUTypes(nil, nil, nil)
	slog.Info("GPU passthrough disabled via config reload")
}

func (d *Daemon) setupShutdown() {
	d.shutdownWg.Go(func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

		<-sigChan
		slog.Info("Received shutdown signal, cleaning up...")

		d.cancel()

		// DDIL Tier 1: SIGTERM alone never stops VMs — the new daemon reattaches
		// via the local state file. VMs stop only after coordinated DRAIN.
		if d.shuttingDown.Load() {
			slog.Info("Coordinated shutdown in progress, skipping VM stop (already handled by DRAIN phase)")
		} else {
			slog.Info("SIGTERM with no coordinated drain — leaving local VMs running for restart recovery")
			d.shuttingDown.Store(true)
		}

		if d.elbv2Service != nil {
			d.elbv2Service.Close()
		}

		if d.eksService != nil {
			d.eksService.Shutdown()
		}

		for _, sub := range d.natsSubscriptions {
			slog.Info("Unsubscribing from NATS", "subject", sub.Subject)
			if err := sub.Unsubscribe(); err != nil {
				slog.Error("Error unsubscribing from NATS", "err", err)
			}
		}

		if d.jsManager != nil {
			if err := d.jsManager.WriteShutdownMarker(d.node); err != nil {
				slog.Error("Failed to write shutdown marker", "err", err)
			}
		}

		err := d.WriteState()
		if err != nil {
			slog.Error("Failed to write state", "err", err)
		}

		// natsConn is nil when NATS was unreachable at startup (DDIL Scenario B).
		if d.natsConn != nil {
			d.natsConn.Close()
		}

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
	return rm.canAllocateLocked(instanceType, count)
}

// canAllocateLocked is the lock-free body of canAllocate. Caller must already
// hold rm.mu for read or write. Extracted so allocate can re-check capacity
// while holding the write lock without dropping it.
func (rm *ResourceManager) canAllocateLocked(instanceType *ec2.InstanceTypeInfo, count int) int {
	// Whole-GPU passthrough: gpuManager.Claim is the sole gate — one GPU per
	// slot, so host CPU/memory is never the binding constraint.
	// MIG types fall through to the normal check: one physical GPU backs many
	// concurrent slices, each consuming real host CPU and memory, so those
	// resources must be tracked and enforced like any non-GPU instance type.
	instanceTypeName := ""
	if instanceType.InstanceType != nil {
		instanceTypeName = *instanceType.InstanceType
	}
	if instancetypes.IsGPUType(instanceType) && !instancetypes.IsMIGType(instanceTypeName) {
		return count
	}

	n := canAllocateCount(
		rm.hostVCPU-rm.reservedVCPU-rm.reservedCRVCPU, rm.allocatedVCPU,
		rm.hostMemGB-rm.reservedMem-rm.reservedCRMem, rm.allocatedMem,
		instanceTypeVCPUs(instanceType),
		rm.instanceMemChargeMiB(instanceType),
		count,
		0, false,
	)
	return rm.liveMemGate(n, instanceType)
}

// liveMemGate clamps n by current MemAvailable, catching overcommit that the
// static -m budget misses. Returns n unchanged when disabled or on read error.
func (rm *ResourceManager) liveMemGate(n int, instanceType *ec2.InstanceTypeInfo) int {
	if rm.readMemAvailableGB == nil || n <= 0 {
		return n
	}
	availGB, ok := rm.readMemAvailableGB()
	if !ok {
		return n
	}
	memGB := float64(rm.instanceMemChargeMiB(instanceType)) / 1024.0
	return liveMemCount(n, availGB, rm.reservedMem+rm.reservedCRMem, memGB)
}

// allocate reserves resources for one instance and updates NATS subscriptions.
// Check and commit run under a single write-lock acquisition; without this,
// two concurrent callers could both observe free capacity through the read
// lock and then both commit, overcommitting the host. Multi-instance launch
// paths loop on allocate per VM, relying on this per-call atomicity.
func (rm *ResourceManager) allocate(instanceType *ec2.InstanceTypeInfo) error {
	rm.mu.Lock()
	if rm.canAllocateLocked(instanceType, 1) < 1 {
		rm.mu.Unlock()
		instanceTypeName := ""
		if instanceType.InstanceType != nil {
			instanceTypeName = *instanceType.InstanceType
		}
		return fmt.Errorf("insufficient resources for instance type %s", instanceTypeName)
	}
	vCPUs := instanceTypeVCPUs(instanceType)
	memoryGB := float64(rm.instanceMemChargeMiB(instanceType)) / 1024.0
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
	memoryGB := float64(rm.instanceMemChargeMiB(instanceType)) / 1024.0
	rm.allocatedVCPU -= int(vCPUs)
	rm.allocatedMem -= memoryGB
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
}

var _ handlers_ec2_instance.InstanceTypeAllocator = (*ResourceManager)(nil)

// Allocate, Deallocate, CanAllocate satisfy handlers_ec2_instance.InstanceTypeAllocator.
func (rm *ResourceManager) Allocate(it *ec2.InstanceTypeInfo) error { return rm.allocate(it) }
func (rm *ResourceManager) Deallocate(it *ec2.InstanceTypeInfo)     { rm.deallocate(it) }
func (rm *ResourceManager) CanAllocate(it *ec2.InstanceTypeInfo, count int) int {
	return rm.canAllocate(it, count)
}

// InstanceTypes returns the shared instance-type map. Callers must not mutate
// the returned map; reloadGPUTypes mutates it in place under rm.mu.
func (rm *ResourceManager) InstanceTypes() map[string]*ec2.InstanceTypeInfo {
	return rm.instanceTypes
}

// reloadGPUTypes replaces GPU instance types in the shared map (in-place, so
// all holders see the update) and refreshes NATS subscriptions. Called on SIGHUP.
func (rm *ResourceManager) reloadGPUTypes(models []instancetypes.GPUModel, migProfiles []instancetypes.MIGProfileSpec, mgr *gpu.Manager) {
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
	if len(migProfiles) > 0 {
		maps.Copy(rm.instanceTypes, instancetypes.GenerateMIGTypes(migProfiles, arch))
	}
	rm.gpuManager = mgr
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
}

func (rm *ResourceManager) initSubscriptions(nc *nats.Conn, handler nats.MsgHandler, systemHandler nats.MsgHandler, nodeID string) {
	rm.natsConn = nc
	rm.handler = handler
	rm.systemHandler = systemHandler
	rm.nodeID = nodeID
	rm.instanceSubs = make(map[string]*nats.Subscription)
	rm.updateInstanceSubscriptions()
}

// updateInstanceSubscriptions subscribes/unsubscribes per-type NATS topics
// based on current capacity. Customer types use ec2.RunInstances.*; system
// types use system.LaunchInstance.*. Each gets a queue-group and a node-targeted topic.
func (rm *ResourceManager) updateInstanceSubscriptions() {
	if rm.natsConn == nil {
		return
	}

	rm.subsMu.Lock()
	defer rm.subsMu.Unlock()

	for typeName, typeInfo := range rm.instanceTypes {
		canFit := rm.canAllocate(typeInfo, 1) >= 1

		subjectRoot := "ec2.RunInstances"
		handler := rm.handler
		queueGroup := "spinifex-workers"
		if instancetypes.IsSystemType(typeName) {
			// System types (sys.micro, etc.) are internal-only — not exposed
			// via the customer EC2 API and use a dedicated subject root so
			// ELBv2 can fan out ALB-VM launches across the cluster.
			if rm.systemHandler == nil {
				continue
			}
			subjectRoot = "system.LaunchInstance"
			handler = rm.systemHandler
		}

		queueTopic := fmt.Sprintf("%s.%s", subjectRoot, typeName)
		handler = natsMetricsHandler(queueTopic, handler)
		_, subscribed := rm.instanceSubs[queueTopic]
		if canFit && !subscribed {
			sub, err := rm.natsConn.QueueSubscribe(queueTopic, queueGroup, handler)
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

		if rm.nodeID != "" {
			// Node-targeted topic stays subscribed even when capacity is full:
			// a committed reservation (e.g. spread placement) must not fail with
			// "no responders". Capacity is enforced at launch time by allocate().
			nodeTopic := fmt.Sprintf("%s.%s.%s", subjectRoot, typeName, rm.nodeID)
			if _, nodeSubscribed := rm.instanceSubs[nodeTopic]; canFit && !nodeSubscribed {
				sub, err := rm.natsConn.Subscribe(nodeTopic, handler)
				if err != nil {
					slog.Error("Failed to subscribe to node-specific topic", "topic", nodeTopic, "err", err)
					continue
				}
				rm.instanceSubs[nodeTopic] = sub
				slog.Debug("Subscribed to node-specific instance type", "topic", nodeTopic)
			}
		}
	}
}

// wireLBAgentConfig loads system credentials, resolves the gateway URL,
// and wires them into the ELBv2 service for LB VM cloud-init injection.
func (d *Daemon) wireLBAgentConfig() {
	if d.config.Predastore.AccessKey != "" && d.config.Predastore.SecretKey != "" {
		d.systemAccessKey = d.config.Predastore.AccessKey
		d.systemSecretKey = d.config.Predastore.SecretKey
		d.elbv2Service.SystemAccessKey = d.config.Predastore.AccessKey
		d.elbv2Service.SystemSecretKey = d.config.Predastore.SecretKey
		slog.Info("System credentials loaded for LB agent auth")
	} else {
		slog.Warn("System credentials missing from spinifex.toml predastore section — LB VMs will not have SigV4 credentials for agent auth")
	}

	awsgwBindIP := ""
	if d.config.AWSGW.Host != "" {
		if h, _, splitErr := net.SplitHostPort(d.config.AWSGW.Host); splitErr == nil {
			awsgwBindIP = h
		}
	}

	advertiseIP := d.config.AdvertiseIP

	gatewayHost := d.resolveGatewayHost()

	// Set mgmtRouteVia when AWSGW is only reachable via br-mgmt (multi-node).
	if gatewayHost != "" && gatewayHost == awsgwBindIP && d.mgmtBridgeIP != "" && awsgwBindIP != advertiseIP {
		d.mgmtRouteVia = awsgwBindIP
	}

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

	if d.mgmtRouteVia != "" {
		d.elbv2Service.MgmtRouteGateway = d.mgmtBridgeIP
		d.elbv2Service.MgmtRouteTarget = d.mgmtRouteVia
	}

	d.elbv2Service.MgmtBridgeIP = d.mgmtBridgeIP
	d.elbv2Service.AdvertiseIP = advertiseIP

	if d.config.NATS.CACert != "" {
		if caBytes, err := os.ReadFile(d.config.NATS.CACert); err == nil {
			d.elbv2Service.CACert = string(caBytes)
			slog.Info("CA cert loaded for LB agent TLS", "path", d.config.NATS.CACert)
		} else {
			slog.Warn("Failed to read CA cert for LB agent TLS", "path", d.config.NATS.CACert, "err", err)
		}
	} else {
		slog.Warn("NATS CACert not configured — direct-boot LB VMs will not verify AWSGW TLS")
	}
}

// buildGPUPool partitions GPU devices into whole-GPU and MIG entries, constructs
// a Manager, and returns models and MIG profile specs for instance-type generation.
func buildGPUPool(devices []gpu.GPUDevice, cfg config.DaemonConfig) (*gpu.Manager, []instancetypes.GPUModel, []instancetypes.MIGProfileSpec) {
	type migEntry struct {
		dev      gpu.GPUDevice
		existing []gpu.MIGInstance // non-nil = restart recovery; nil = fresh free GPU
	}

	var wholeGPU []gpu.GPUDevice
	var migEntries []migEntry
	var migProfiles []instancetypes.MIGProfileSpec
	seenProfiles := make(map[string]bool)
	recoveredSlices, freeMIGGPUs := 0, 0

	for _, dev := range devices {
		if !dev.MIGCapable || !dev.MIGEnabled {
			if dev.MIGCapable && !dev.MIGEnabled {
				slog.Warn("MIG-capable GPU but MIG mode not active; using whole-GPU passthrough",
					"gpu", dev.PCIAddress, "hint", "run 'spx admin gpu mig enable'")
			}
			wholeGPU = append(wholeGPU, dev)
			continue
		}

		// Collect available profiles for MIG instance-type generation.
		profiles, err := gpu.ListProfiles(dev.PCIAddress)
		if err != nil {
			slog.Warn("Could not list MIG profiles; GPU will not advertise MIG types",
				"gpu", dev.PCIAddress, "err", err)
		}
		for _, p := range profiles {
			if !seenProfiles[p.Name] {
				seenProfiles[p.Name] = true
				migProfiles = append(migProfiles, instancetypes.MIGProfileSpec{
					Name: p.Name, MemoryMiB: p.MemoryMiB,
				})
			}
		}

		// Re-discover existing instances from a previous daemon run.
		existing, listErr := gpu.ListInstances(dev.PCIAddress)
		if listErr != nil {
			slog.Error("MIG list instances failed, falling back to whole-GPU passthrough",
				"gpu", dev.PCIAddress, "err", listErr)
			wholeGPU = append(wholeGPU, dev)
			continue
		}
		migEntries = append(migEntries, migEntry{dev: dev, existing: existing})
		if len(existing) > 0 {
			recoveredSlices += len(existing)
		} else {
			freeMIGGPUs++
		}
	}

	var models []instancetypes.GPUModel
	for _, dev := range wholeGPU {
		models = append(models, resolveGPUModel(dev, cfg.GPUModelOverrides))
	}

	mgr := gpu.NewManager(wholeGPU)
	for _, me := range migEntries {
		if len(me.existing) > 0 {
			mgr.AddMIGInstances(me.dev, me.existing)
		} else {
			mgr.AddMIGGPU(me.dev)
		}
	}

	slog.Info("GPU pool built",
		"whole_gpu", len(wholeGPU), "mig_free", freeMIGGPUs,
		"mig_slices_recovered", recoveredSlices, "mig_profiles", len(migProfiles))
	return mgr, models, migProfiles
}

// resolveGPUModel maps a GPU device to an instance type model. Overrides take
// priority, then the production model list, then a g5 default.
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

// gpuProbeResult holds the outcome of the startup GPU hardware probe.
type gpuProbeResult struct {
	Capable     bool // true when Devices, IOMMUActive, and VFIOPresent are all satisfied
	IOMMUActive bool
	VFIOPresent bool
	Devices     []gpu.GPUDevice
}

// probeGPU discovers GPU hardware and checks passthrough prerequisites (read-only).
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

	// MIG-enabled GPUs are capable without vfio-pci: the NVIDIA driver owns
	// isolation via the mdev subsystem, so vfio-pci is not required.
	hasMIG := false
	for _, d := range r.Devices {
		if d.MIGEnabled {
			hasMIG = true
			break
		}
	}
	r.Capable = len(r.Devices) > 0 && ((r.IOMMUActive && r.VFIOPresent) || hasMIG)
	return r
}
