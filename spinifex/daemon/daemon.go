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
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
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

// Daemon represents the main daemon service.
type Daemon struct {
	node                  string
	clusterConfig         *config.ClusterConfig
	config                *config.Config
	natsConn              *nats.Conn
	resourceMgr           *ResourceManager
	instanceService       *handlers_ec2_instance.InstanceServiceImpl
	dnsWriter             *handlers_dns.Writer
	dnsReconciler         *handlers_dns.Reconciler
	dnsBaseDomain         string
	dnsInternalDomain     string
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

// getSystemMemory returns the total system memory in GB.
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

// GetInstanceTypeInfos returns all instance types as ec2.InstanceTypeInfo for AWS API compatibility.
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

// SetConfigPath sets the configuration file path for cluster management.
func (d *Daemon) SetConfigPath(path string) {
	d.configPath = path
}

// NewDaemon creates a new daemon instance.
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
		{"ec2.CreateKeyPair", handleNATSRequest(d.keyService.CreateKeyPair), "spinifex-workers"},
		{"ec2.DeleteKeyPair", handleNATSRequest(d.keyService.DeleteKeyPair), "spinifex-workers"},
		{"ec2.DescribeKeyPairs", handleNATSRequest(d.keyService.DescribeKeyPairs), "spinifex-workers"},
		{"ec2.ImportKeyPair", handleNATSRequest(d.keyService.ImportKeyPair), "spinifex-workers"},
		{"imds.ec2.get_public_key", d.handleIMDSGetPublicKey, "spinifex-workers"},
		{"ec2.DescribeImages", handleNATSRequest(d.imageService.DescribeImages), "spinifex-workers"},
		{"ec2.CreateImage", d.handleEC2CreateImage, ""},
		{"ec2.DeregisterImage", handleNATSRequest(d.imageService.DeregisterImage), "spinifex-workers"},
		{"ec2.RegisterImage", handleNATSRequest(d.imageService.RegisterImage), "spinifex-workers"},
		{"ec2.CopyImage", handleNATSRequest(d.imageService.CopyImage), "spinifex-workers"},
		{"ec2.DescribeImageAttribute", handleNATSRequest(d.imageService.DescribeImageAttribute), "spinifex-workers"},
		{"ec2.ModifyImageAttribute", handleNATSRequest(d.imageService.ModifyImageAttribute), "spinifex-workers"},
		{"ec2.ResetImageAttribute", handleNATSRequest(d.imageService.ResetImageAttribute), "spinifex-workers"},
		{"ec2.CreateVolume", handleNATSRequest(d.volumeService.CreateVolume), "spinifex-workers"},
		{"ec2.DescribeVolumes", handleNATSRequest(d.volumeService.DescribeVolumes), "spinifex-workers"},
		{"ec2.ModifyVolume", d.handleEC2ModifyVolume, "spinifex-workers"},
		{"ec2.DeleteVolume", handleNATSRequest(d.volumeService.DeleteVolume), "spinifex-workers"},
		{"ec2.DescribeVolumeStatus", handleNATSRequest(d.volumeService.DescribeVolumeStatus), "spinifex-workers"},
		{"ec2.DescribeVolumesModifications", handleNATSRequest(d.volumeService.DescribeVolumesModifications), "spinifex-workers"},
		{"ec2.CreateSnapshot", handleNATSRequest(d.snapshotService.CreateSnapshot), "spinifex-workers"},
		{"ec2.DescribeSnapshots", handleNATSRequest(d.snapshotService.DescribeSnapshots), "spinifex-workers"},
		{"ec2.DeleteSnapshot", handleNATSRequest(d.snapshotService.DeleteSnapshot), "spinifex-workers"},
		{"ec2.CopySnapshot", handleNATSRequest(d.snapshotService.CopySnapshot), "spinifex-workers"},
		{"ec2.CreateTags", handleNATSRequest(d.createTags), "spinifex-workers"},
		{"ec2.DeleteTags", handleNATSRequest(d.deleteTags), "spinifex-workers"},
		{"ec2.DescribeTags", handleNATSRequest(d.tagsService.DescribeTags), "spinifex-workers"},
		{"ec2.CreateEgressOnlyInternetGateway", handleNATSRequest(d.eigwService.CreateEgressOnlyInternetGateway), "spinifex-workers"},
		{"ec2.DeleteEgressOnlyInternetGateway", handleNATSRequest(d.eigwService.DeleteEgressOnlyInternetGateway), "spinifex-workers"},
		{"ec2.DescribeEgressOnlyInternetGateways", handleNATSRequest(d.eigwService.DescribeEgressOnlyInternetGateways), "spinifex-workers"},
		{"ec2.CreateInternetGateway", handleNATSRequest(d.igwService.CreateInternetGateway), "spinifex-workers"},
		{"ec2.DeleteInternetGateway", handleNATSRequest(d.igwService.DeleteInternetGateway), "spinifex-workers"},
		{"ec2.DescribeInternetGateways", handleNATSRequest(d.igwService.DescribeInternetGateways), "spinifex-workers"},
		{"ec2.AttachInternetGateway", handleNATSRequest(d.igwService.AttachInternetGateway), "spinifex-workers"},
		{"ec2.DetachInternetGateway", handleNATSRequest(d.igwService.DetachInternetGateway), "spinifex-workers"},
		{"ec2.CreatePlacementGroup", handleNATSRequest(d.placementGroupService.CreatePlacementGroup), "spinifex-workers"},
		{"ec2.DeletePlacementGroup", handleNATSRequest(d.placementGroupService.DeletePlacementGroup), "spinifex-workers"},
		{"ec2.DescribePlacementGroups", handleNATSRequest(d.placementGroupService.DescribePlacementGroups), "spinifex-workers"},
		{"ec2.ReserveSpreadNodes", handleNATSRequest(d.placementGroupService.ReserveSpreadNodes), "spinifex-workers"},
		{"ec2.FinalizeSpreadInstances", handleNATSRequest(d.placementGroupService.FinalizeSpreadInstances), "spinifex-workers"},
		{"ec2.ReleaseSpreadNodes", handleNATSRequest(d.placementGroupService.ReleaseSpreadNodes), "spinifex-workers"},
		{"ec2.RemoveInstanceFromPlacementGroup", handleNATSRequest(d.placementGroupService.RemoveInstance), "spinifex-workers"},
		{"ec2.ReserveClusterNode", handleNATSRequest(d.placementGroupService.ReserveClusterNode), "spinifex-workers"},
		{"ec2.FinalizeClusterInstances", handleNATSRequest(d.placementGroupService.FinalizeClusterInstances), "spinifex-workers"},
		{"ec2.CreateLaunchTemplate", handleNATSRequest(d.launchTemplateService.CreateLaunchTemplate), "spinifex-workers"},
		{"ec2.CreateLaunchTemplateVersion", handleNATSRequest(d.launchTemplateService.CreateLaunchTemplateVersion), "spinifex-workers"},
		{"ec2.DeleteLaunchTemplate", handleNATSRequest(d.launchTemplateService.DeleteLaunchTemplate), "spinifex-workers"},
		{"ec2.DeleteLaunchTemplateVersions", handleNATSRequest(d.launchTemplateService.DeleteLaunchTemplateVersions), "spinifex-workers"},
		{"ec2.ModifyLaunchTemplate", handleNATSRequest(d.launchTemplateService.ModifyLaunchTemplate), "spinifex-workers"},
		{"ec2.DescribeLaunchTemplates", handleNATSRequest(d.launchTemplateService.DescribeLaunchTemplates), "spinifex-workers"},
		{"ec2.DescribeLaunchTemplateVersions", handleNATSRequest(d.launchTemplateService.DescribeLaunchTemplateVersions), "spinifex-workers"},
		{"ec2.PutSpotInstanceRequests", handleNATSRequest(d.spotInstanceService.PutSpotInstanceRequests), "spinifex-workers"},
		{"ec2.DescribeSpotInstanceRequests", handleNATSRequest(d.spotInstanceService.DescribeSpotInstanceRequests), "spinifex-workers"},
		{"ec2.CancelSpotInstanceRequests", handleNATSRequest(d.spotInstanceService.CancelSpotInstanceRequests), "spinifex-workers"},
		// Capacity reservations: Create is node-targeted (gateway pins one node);
		// Describe fans out and Cancel broadcasts, so both use plain Subscribe.
		{fmt.Sprintf("ec2.CreateCapacityReservation.%s", d.node), d.handleEC2CreateCapacityReservation, ""},
		{"ec2.DescribeCapacityReservations", d.handleEC2DescribeCapacityReservations, ""},
		{"ec2.CancelCapacityReservation", d.handleEC2CancelCapacityReservation, ""},
		{"ec2.CreateNatGateway", handleNATSRequest(d.natGatewayService.CreateNatGateway), "spinifex-workers"},
		{"ec2.DeleteNatGateway", handleNATSRequest(d.natGatewayService.DeleteNatGateway), "spinifex-workers"},
		{"ec2.DescribeNatGateways", handleNATSRequest(d.natGatewayService.DescribeNatGateways), "spinifex-workers"},
		{"ec2.CreateRouteTable", handleNATSRequest(d.routeTableService.CreateRouteTable), "spinifex-workers"},
		{"ec2.DeleteRouteTable", handleNATSRequest(d.routeTableService.DeleteRouteTable), "spinifex-workers"},
		{"ec2.DescribeRouteTables", handleNATSRequest(d.routeTableService.DescribeRouteTables), "spinifex-workers"},
		{"ec2.CreateRoute", handleNATSRequest(d.routeTableService.CreateRoute), "spinifex-workers"},
		{"ec2.DeleteRoute", handleNATSRequest(d.routeTableService.DeleteRoute), "spinifex-workers"},
		{"ec2.ReplaceRoute", handleNATSRequest(d.routeTableService.ReplaceRoute), "spinifex-workers"},
		{"ec2.AssociateRouteTable", handleNATSRequest(d.routeTableService.AssociateRouteTable), "spinifex-workers"},
		{"ec2.DisassociateRouteTable", handleNATSRequest(d.routeTableService.DisassociateRouteTable), "spinifex-workers"},
		{"ec2.ReplaceRouteTableAssociation", handleNATSRequest(d.routeTableService.ReplaceRouteTableAssociation), "spinifex-workers"},
		{"ec2.CreateVpc", handleNATSRequest(d.vpcService.CreateVpc), "spinifex-workers"},
		{"ec2.DeleteVpc", handleNATSRequest(d.vpcService.DeleteVpc), "spinifex-workers"},
		{"ec2.DescribeVpcs", handleNATSRequest(d.vpcService.DescribeVpcs), "spinifex-workers"},
		{"ec2.CreateSubnet", handleNATSRequest(d.vpcService.CreateSubnet), "spinifex-workers"},
		{"ec2.DeleteSubnet", handleNATSRequest(d.vpcService.DeleteSubnet), "spinifex-workers"},
		{"ec2.DescribeSubnets", handleNATSRequest(d.vpcService.DescribeSubnets), "spinifex-workers"},
		{"ec2.ModifySubnetAttribute", handleNATSRequest(d.vpcService.ModifySubnetAttribute), "spinifex-workers"},
		{"ec2.ModifyVpcAttribute", handleNATSRequest(d.vpcService.ModifyVpcAttribute), "spinifex-workers"},
		{"ec2.DescribeVpcAttribute", handleNATSRequest(d.vpcService.DescribeVpcAttribute), "spinifex-workers"},
		{"ec2.CreateNetworkInterface", handleNATSRequest(d.vpcService.CreateNetworkInterface), "spinifex-workers"},
		{"ec2.DeleteNetworkInterface", handleNATSRequest(d.vpcService.DeleteNetworkInterface), "spinifex-workers"},
		{"ec2.DescribeNetworkInterfaces", handleNATSRequest(d.vpcService.DescribeNetworkInterfaces), "spinifex-workers"},
		{"ec2.ModifyNetworkInterfaceAttribute", handleNATSRequest(d.vpcService.ModifyNetworkInterfaceAttribute), "spinifex-workers"},
		{"ec2.CreateSecurityGroup", handleNATSRequest(d.vpcService.CreateSecurityGroup), "spinifex-workers"},
		{"ec2.DeleteSecurityGroup", handleNATSRequest(d.vpcService.DeleteSecurityGroup), "spinifex-workers"},
		{"ec2.DescribeSecurityGroups", handleNATSRequest(d.vpcService.DescribeSecurityGroups), "spinifex-workers"},
		{"ec2.DescribeSecurityGroupRules", handleNATSRequest(d.vpcService.DescribeSecurityGroupRules), "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupIngress", handleNATSRequest(d.vpcService.AuthorizeSecurityGroupIngress), "spinifex-workers"},
		{"ec2.AuthorizeSecurityGroupEgress", handleNATSRequest(d.vpcService.AuthorizeSecurityGroupEgress), "spinifex-workers"},
		{"ec2.RevokeSecurityGroupIngress", handleNATSRequest(d.vpcService.RevokeSecurityGroupIngress), "spinifex-workers"},
		{"ec2.RevokeSecurityGroupEgress", handleNATSRequest(d.vpcService.RevokeSecurityGroupEgress), "spinifex-workers"},
		{"ec2.ModifyInstanceAttribute", handleNATSRequest(d.instanceService.ModifyInstanceAttribute), "spinifex-workers"},
		{"ec2.ModifyInstanceMetadataOptions", handleNATSRequest(d.instanceService.ModifyInstanceMetadataOptions), "spinifex-workers"},
		{"ec2.start", d.handleEC2StartStoppedInstance, "spinifex-workers"},
		// ec2.start.{node} is the node-targeted variant: it always starts locally
		// and never re-forwards, so it goes straight to the service (no routing loop).
		{fmt.Sprintf("ec2.start.%s", d.node), handleNATSRequest(d.instanceService.StartStoppedInstance), ""},
		{"ec2.terminate", handleNATSRequest(d.instanceService.TerminateStoppedInstance), "spinifex-workers"},
		{"ec2.DescribeStoppedInstances", handleNATSRequest(d.instanceService.DescribeStoppedInstances), "spinifex-workers"},
		{"ec2.DescribeTerminatedInstances", handleNATSRequest(d.instanceService.DescribeTerminatedInstances), "spinifex-workers"},
		// these fan out to all nodes and gateway aggregates the results. The
		// handler only sees per-daemon local state (vmMgr/stoppedStore), so
		// any queue-grouped routing produces 1/N false NotFound responses.
		{"ec2.DescribeInstances", handleNATSRequest(d.instanceService.DescribeInstances), ""},
		{"ec2.DescribeInstanceStatus", handleNATSRequest(d.instanceService.DescribeInstanceStatus), ""},
		{"ec2.DescribeInstanceTypes", handleNATSRequest(d.instanceService.DescribeInstanceTypes), ""},
		{"ec2.DescribeInstanceAttribute", handleNATSRequest(d.instanceService.DescribeInstanceAttribute), ""},
		// IAM instance profile associations: Disassociate/Replace mutate the
		// owning daemon's vm.VM (non-owners NoOp with Found=false); Describe
		// returns per-daemon matches that the gateway concatenates.
		{"ec2.IamProfileAssociation.disassociate", handleNATSRequest(d.instanceService.DisassociateIamProfileAssociation), ""},
		{"ec2.IamProfileAssociation.replace", handleNATSRequest(d.instanceService.ReplaceIamProfileAssociation), ""},
		{"ec2.IamProfileAssociation.describe", handleNATSRequest(d.instanceService.DescribeIamProfileAssociations), ""},
		{"ec2.EnableEbsEncryptionByDefault", handleNATSRequest(d.accountService.EnableEbsEncryptionByDefault), "spinifex-workers"},
		{"ec2.DisableEbsEncryptionByDefault", handleNATSRequest(d.accountService.DisableEbsEncryptionByDefault), "spinifex-workers"},
		{"ec2.GetEbsEncryptionByDefault", handleNATSRequest(d.accountService.GetEbsEncryptionByDefault), "spinifex-workers"},
		{"ec2.GetSerialConsoleAccessStatus", handleNATSRequest(d.accountService.GetSerialConsoleAccessStatus), "spinifex-workers"},
		{"ec2.EnableSerialConsoleAccess", handleNATSRequest(d.accountService.EnableSerialConsoleAccess), "spinifex-workers"},
		{"ec2.DisableSerialConsoleAccess", handleNATSRequest(d.accountService.DisableSerialConsoleAccess), "spinifex-workers"},
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
			natsSub{"elbv2.CreateLoadBalancer", handleNATSRequest(d.elbv2Service.CreateLoadBalancer), "spinifex-workers"},
			natsSub{"elbv2.DeleteLoadBalancer", handleNATSRequest(d.elbv2Service.DeleteLoadBalancer), "spinifex-workers"},
			natsSub{"elbv2.DescribeLoadBalancers", handleNATSRequest(d.elbv2Service.DescribeLoadBalancers), "spinifex-workers"},
			natsSub{"elbv2.CreateTargetGroup", handleNATSRequest(d.elbv2Service.CreateTargetGroup), "spinifex-workers"},
			natsSub{"elbv2.ModifyTargetGroup", handleNATSRequest(d.elbv2Service.ModifyTargetGroup), "spinifex-workers"},
			natsSub{"elbv2.DeleteTargetGroup", handleNATSRequest(d.elbv2Service.DeleteTargetGroup), "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetGroups", handleNATSRequest(d.elbv2Service.DescribeTargetGroups), "spinifex-workers"},
			natsSub{"elbv2.RegisterTargets", handleNATSRequest(d.elbv2Service.RegisterTargets), "spinifex-workers"},
			natsSub{"elbv2.DeregisterTargets", handleNATSRequest(d.elbv2Service.DeregisterTargets), "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetHealth", handleNATSRequest(d.elbv2Service.DescribeTargetHealth), "spinifex-workers"},
			natsSub{"elbv2.CreateListener", handleNATSRequest(d.elbv2Service.CreateListener), "spinifex-workers"},
			natsSub{"elbv2.DeleteListener", handleNATSRequest(d.elbv2Service.DeleteListener), "spinifex-workers"},
			natsSub{"elbv2.ModifyListener", handleNATSRequest(d.elbv2Service.ModifyListener), "spinifex-workers"},
			natsSub{"elbv2.DescribeListeners", handleNATSRequest(d.elbv2Service.DescribeListeners), "spinifex-workers"},
			natsSub{"elbv2.CreateRule", handleNATSRequest(d.elbv2Service.CreateRule), "spinifex-workers"},
			natsSub{"elbv2.ModifyRule", handleNATSRequest(d.elbv2Service.ModifyRule), "spinifex-workers"},
			natsSub{"elbv2.DeleteRule", handleNATSRequest(d.elbv2Service.DeleteRule), "spinifex-workers"},
			natsSub{"elbv2.DescribeRules", handleNATSRequest(d.elbv2Service.DescribeRules), "spinifex-workers"},
			natsSub{"elbv2.SetRulePriorities", handleNATSRequest(d.elbv2Service.SetRulePriorities), "spinifex-workers"},
			natsSub{"elbv2.DescribeTags", handleNATSRequest(d.elbv2Service.DescribeTags), "spinifex-workers"},
			natsSub{"elbv2.AddTags", handleNATSRequest(d.elbv2Service.AddTags), "spinifex-workers"},
			natsSub{"elbv2.RemoveTags", handleNATSRequest(d.elbv2Service.RemoveTags), "spinifex-workers"},
			natsSub{"elbv2.LBAgentHeartbeat", handleNATSRequest(d.elbv2Service.LBAgentHeartbeat), "spinifex-workers"},
			natsSub{"elbv2.GetLBConfig", handleNATSRequest(d.elbv2Service.GetLBConfig), "spinifex-workers"},
			natsSub{"elbv2.ModifyTargetGroupAttributes", handleNATSRequest(d.elbv2Service.ModifyTargetGroupAttributes), "spinifex-workers"},
			natsSub{"elbv2.DescribeTargetGroupAttributes", handleNATSRequest(d.elbv2Service.DescribeTargetGroupAttributes), "spinifex-workers"},
			natsSub{"elbv2.ModifyLoadBalancerAttributes", handleNATSRequest(d.elbv2Service.ModifyLoadBalancerAttributes), "spinifex-workers"},
			natsSub{"elbv2.DescribeLoadBalancerAttributes", handleNATSRequest(d.elbv2Service.DescribeLoadBalancerAttributes), "spinifex-workers"},
			natsSub{"elbv2.SetSecurityGroups", handleNATSRequest(d.elbv2Service.SetSecurityGroups), "spinifex-workers"},
			natsSub{"elbv2.SetIpAddressType", handleNATSRequest(d.elbv2Service.SetIpAddressType), "spinifex-workers"},
			natsSub{"elbv2.SetSubnets", handleNATSRequest(d.elbv2Service.SetSubnets), "spinifex-workers"},
			natsSub{"elbv2.AddListenerCertificates", handleNATSRequest(d.elbv2Service.AddListenerCertificates), "spinifex-workers"},
			natsSub{"elbv2.RemoveListenerCertificates", handleNATSRequest(d.elbv2Service.RemoveListenerCertificates), "spinifex-workers"},
			natsSub{"elbv2.DescribeListenerCertificates", handleNATSRequest(d.elbv2Service.DescribeListenerCertificates), "spinifex-workers"},
			natsSub{"elbv2.DescribeSSLPolicies", handleNATSRequest(d.elbv2Service.DescribeSSLPolicies), "spinifex-workers"},
		)
	}

	// EKS gateway → daemon subscriptions. Every handler currently returns
	// NotImplemented; topics are subscribed up-front so the wiring layer is
	// stable while real bodies land.
	if d.eksService != nil {
		subs = append(subs,
			natsSub{"eks.CreateCluster", handleNATSRequestWithPrincipal(d.eksService.CreateCluster), "spinifex-workers"},
			natsSub{"eks.DescribeCluster", handleNATSRequest(d.eksService.DescribeCluster), "spinifex-workers"},
			natsSub{"eks.ListClusters", handleNATSRequest(d.eksService.ListClusters), "spinifex-workers"},
			natsSub{"eks.UpdateClusterConfig", handleNATSRequest(d.eksService.UpdateClusterConfig), "spinifex-workers"},
			natsSub{"eks.UpdateClusterVersion", handleNATSRequest(d.eksService.UpdateClusterVersion), "spinifex-workers"},
			natsSub{"eks.DeleteCluster", handleNATSRequest(d.eksService.DeleteCluster), "spinifex-workers"},
			natsSub{"eks.CreateNodegroup", handleNATSRequest(d.eksService.CreateNodegroup), "spinifex-workers"},
			natsSub{"eks.DescribeNodegroup", handleNATSRequest(d.eksService.DescribeNodegroup), "spinifex-workers"},
			natsSub{"eks.ListNodegroups", handleNATSRequest(d.eksService.ListNodegroups), "spinifex-workers"},
			natsSub{"eks.UpdateNodegroupConfig", handleNATSRequest(d.eksService.UpdateNodegroupConfig), "spinifex-workers"},
			natsSub{"eks.UpdateNodegroupVersion", handleNATSRequest(d.eksService.UpdateNodegroupVersion), "spinifex-workers"},
			natsSub{"eks.DeleteNodegroup", handleNATSRequest(d.eksService.DeleteNodegroup), "spinifex-workers"},
			natsSub{"eks.CreateAccessEntry", handleNATSRequest(d.eksService.CreateAccessEntry), "spinifex-workers"},
			natsSub{"eks.DescribeAccessEntry", handleNATSRequest(d.eksService.DescribeAccessEntry), "spinifex-workers"},
			natsSub{"eks.ListAccessEntries", handleNATSRequest(d.eksService.ListAccessEntries), "spinifex-workers"},
			natsSub{"eks.UpdateAccessEntry", handleNATSRequest(d.eksService.UpdateAccessEntry), "spinifex-workers"},
			natsSub{"eks.DeleteAccessEntry", handleNATSRequest(d.eksService.DeleteAccessEntry), "spinifex-workers"},
			natsSub{"eks.AssociateAccessPolicy", handleNATSRequest(d.eksService.AssociateAccessPolicy), "spinifex-workers"},
			natsSub{"eks.DisassociateAccessPolicy", handleNATSRequest(d.eksService.DisassociateAccessPolicy), "spinifex-workers"},
			natsSub{"eks.ListAssociatedAccessPolicies", handleNATSRequest(d.eksService.ListAssociatedAccessPolicies), "spinifex-workers"},
			natsSub{"eks.ListAccessPolicies", handleNATSRequest(d.eksService.ListAccessPolicies), "spinifex-workers"},
			natsSub{"eks.ListAddons", handleNATSRequest(d.eksService.ListAddons), "spinifex-workers"},
			natsSub{"eks.DescribeAddonVersions", handleNATSRequest(d.eksService.DescribeAddonVersions), "spinifex-workers"},
			natsSub{"eks.CreateAddon", handleNATSRequest(d.eksService.CreateAddon), "spinifex-workers"},
			natsSub{"eks.DeleteAddon", handleNATSRequest(d.eksService.DeleteAddon), "spinifex-workers"},
			natsSub{"eks.DescribeAddon", handleNATSRequest(d.eksService.DescribeAddon), "spinifex-workers"},
			natsSub{"eks.UpdateAddon", handleNATSRequest(d.eksService.UpdateAddon), "spinifex-workers"},
			natsSub{"eks.ListStagedAddonManifests", handleNATSRequest(d.eksService.ListStagedAddonManifests), "spinifex-workers"},
			natsSub{"eks.GetRecoveryDirective", handleNATSRequest(d.eksService.GetRecoveryDirective), "spinifex-workers"},
			natsSub{"eks.SetRecoveryDirective", handleNATSRequest(d.eksService.SetRecoveryDirective), "spinifex-workers"},
			natsSub{"eks.RestoreSnapshot", handleNATSRequest(d.eksService.RestoreSnapshot), "spinifex-workers"},
			natsSub{"eks.AssociateIdentityProviderConfig", handleNATSRequest(d.eksService.AssociateIdentityProviderConfig), "spinifex-workers"},
			natsSub{"eks.DescribeIdentityProviderConfig", handleNATSRequest(d.eksService.DescribeIdentityProviderConfig), "spinifex-workers"},
			natsSub{"eks.ListIdentityProviderConfigs", handleNATSRequest(d.eksService.ListIdentityProviderConfigs), "spinifex-workers"},
			natsSub{"eks.DisassociateIdentityProviderConfig", handleNATSRequest(d.eksService.DisassociateIdentityProviderConfig), "spinifex-workers"},
			natsSub{"eks.TagResource", handleNATSRequest(d.eksService.TagResource), "spinifex-workers"},
			natsSub{"eks.UntagResource", handleNATSRequest(d.eksService.UntagResource), "spinifex-workers"},
			natsSub{"eks.ListTagsForResource", handleNATSRequest(d.eksService.ListTagsForResource), "spinifex-workers"},
		)
	}

	// ECS gateway → daemon subscriptions (control plane; per-account KV).
	if d.ecsService != nil {
		subs = append(subs,
			natsSub{"ecs.CreateCluster", handleNATSRequest(d.ecsService.CreateCluster), "spinifex-workers"},
			natsSub{"ecs.DeleteCluster", handleNATSRequest(d.ecsService.DeleteCluster), "spinifex-workers"},
			natsSub{"ecs.DescribeClusters", handleNATSRequest(d.ecsService.DescribeClusters), "spinifex-workers"},
			natsSub{"ecs.ListClusters", handleNATSRequest(d.ecsService.ListClusters), "spinifex-workers"},
			natsSub{"ecs.RegisterTaskDefinition", handleNATSRequest(d.ecsService.RegisterTaskDefinition), "spinifex-workers"},
			natsSub{"ecs.DeregisterTaskDefinition", handleNATSRequest(d.ecsService.DeregisterTaskDefinition), "spinifex-workers"},
			natsSub{"ecs.DescribeTaskDefinition", handleNATSRequest(d.ecsService.DescribeTaskDefinition), "spinifex-workers"},
			natsSub{"ecs.ListTaskDefinitions", handleNATSRequest(d.ecsService.ListTaskDefinitions), "spinifex-workers"},
			natsSub{"ecs.RegisterContainerInstance", handleNATSRequest(d.ecsService.RegisterContainerInstance), "spinifex-workers"},
			natsSub{"ecs.DeregisterContainerInstance", handleNATSRequest(d.ecsService.DeregisterContainerInstance), "spinifex-workers"},
			natsSub{"ecs.UpdateContainerInstancesState", handleNATSRequest(d.ecsService.UpdateContainerInstancesState), "spinifex-workers"},
			natsSub{"ecs.DescribeContainerInstances", handleNATSRequest(d.ecsService.DescribeContainerInstances), "spinifex-workers"},
			natsSub{"ecs.ListContainerInstances", handleNATSRequest(d.ecsService.ListContainerInstances), "spinifex-workers"},
			natsSub{"ecs.RunTask", handleNATSRequest(d.ecsService.RunTask), "spinifex-workers"},
			natsSub{"ecs.StartTask", handleNATSRequest(d.ecsService.StartTask), "spinifex-workers"},
			natsSub{"ecs.StopTask", handleNATSRequest(d.ecsService.StopTask), "spinifex-workers"},
			natsSub{"ecs.DescribeTasks", handleNATSRequest(d.ecsService.DescribeTasks), "spinifex-workers"},
			natsSub{"ecs.ListTasks", handleNATSRequest(d.ecsService.ListTasks), "spinifex-workers"},
			natsSub{"ecs.CreateService", handleNATSRequest(d.ecsService.CreateService), "spinifex-workers"},
			natsSub{"ecs.UpdateService", handleNATSRequest(d.ecsService.UpdateService), "spinifex-workers"},
			natsSub{"ecs.DeleteService", handleNATSRequest(d.ecsService.DeleteService), "spinifex-workers"},
			natsSub{"ecs.DescribeServices", handleNATSRequest(d.ecsService.DescribeServices), "spinifex-workers"},
			natsSub{"ecs.ListServices", handleNATSRequest(d.ecsService.ListServices), "spinifex-workers"},
			natsSub{"ecs.SubmitTaskStateChange", handleNATSRequest(d.ecsService.SubmitTaskStateChange), "spinifex-workers"},
			natsSub{"ecs.PollAssignments", handleNATSRequest(d.ecsService.PollAssignments), "spinifex-workers"},
			natsSub{"ecs.ReportTaskGPU", handleNATSRequest(d.ecsService.ReportTaskGPU), "spinifex-workers"},
			natsSub{"ecs.ProvisionCapacity", handleNATSRequest(d.ecsService.ProvisionCapacity), "spinifex-workers"},
			natsSub{"ecs.TagResource", handleNATSRequest(d.ecsService.TagResource), "spinifex-workers"},
			natsSub{"ecs.UntagResource", handleNATSRequest(d.ecsService.UntagResource), "spinifex-workers"},
			natsSub{"ecs.ListTagsForResource", handleNATSRequest(d.ecsService.ListTagsForResource), "spinifex-workers"},
			natsSub{"ecs.PutClusterCapacityProviders", handleNATSRequest(d.ecsService.PutClusterCapacityProviders), "spinifex-workers"},
			natsSub{"ecs.CreateCapacityProvider", handleNATSRequest(d.ecsService.CreateCapacityProvider), "spinifex-workers"},
			natsSub{"ecs.DescribeCapacityProviders", handleNATSRequest(d.ecsService.DescribeCapacityProviders), "spinifex-workers"},
			natsSub{"ecs.DeleteCapacityProvider", handleNATSRequest(d.ecsService.DeleteCapacityProvider), "spinifex-workers"},
		)
	}

	// ACM gateway → daemon subscriptions (minimal certificate store).
	if d.acmService != nil {
		subs = append(subs,
			natsSub{"acm.ImportCertificate", handleNATSRequest(d.acmService.ImportCertificate), "spinifex-workers"},
			natsSub{"acm.DescribeCertificate", handleNATSRequest(d.acmService.DescribeCertificate), "spinifex-workers"},
			natsSub{"acm.ListCertificates", handleNATSRequest(d.acmService.ListCertificates), "spinifex-workers"},
			natsSub{"acm.DeleteCertificate", handleNATSRequest(d.acmService.DeleteCertificate), "spinifex-workers"},
			natsSub{"acm.ListTagsForCertificate", handleNATSRequest(d.acmService.ListTagsForCertificate), "spinifex-workers"},
			natsSub{"acm.AddTagsToCertificate", handleNATSRequest(d.acmService.AddTagsToCertificate), "spinifex-workers"},
			natsSub{"acm.RemoveTagsFromCertificate", handleNATSRequest(d.acmService.RemoveTagsFromCertificate), "spinifex-workers"},
		)
	}

	// ECR gateway → daemon subscriptions. The daemon owns the per-account
	// JetStream KV metadata; blob/manifest bytes never traverse these subjects.
	if d.ecrMetaService != nil {
		subs = append(subs,
			natsSub{handlers_ecr.SubjectRepoCreate, handleNATSRequest(d.ecrMetaService.RepoCreate), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoDescribe, handleNATSRequest(d.ecrMetaService.RepoDescribe), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoList, handleNATSRequest(d.ecrMetaService.RepoList), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectRepoDelete, handleNATSRequest(d.ecrMetaService.RepoDelete), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyPut, handleNATSRequest(d.ecrMetaService.PolicyPut), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyGet, handleNATSRequest(d.ecrMetaService.PolicyGet), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectPolicyDelete, handleNATSRequest(d.ecrMetaService.PolicyDelete), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecyclePut, handleNATSRequest(d.ecrMetaService.LifecyclePut), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecycleGet, handleNATSRequest(d.ecrMetaService.LifecycleGet), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectLifecycleDelete, handleNATSRequest(d.ecrMetaService.LifecycleDelete), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagPut, handleNATSRequest(d.ecrMetaService.TagPut), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagGet, handleNATSRequest(d.ecrMetaService.TagGet), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagList, handleNATSRequest(d.ecrMetaService.TagList), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectTagDelete, handleNATSRequest(d.ecrMetaService.TagDelete), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestPut, handleNATSRequest(d.ecrMetaService.ManifestPut), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestDescribe, handleNATSRequest(d.ecrMetaService.ManifestDescribe), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestList, handleNATSRequest(d.ecrMetaService.ManifestList), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectManifestDelete, handleNATSRequest(d.ecrMetaService.ManifestDelete), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadCreate, handleNATSRequest(d.ecrMetaService.UploadCreate), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadGet, handleNATSRequest(d.ecrMetaService.UploadGet), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadUpdate, handleNATSRequest(d.ecrMetaService.UploadUpdate), "spinifex-workers"},
			natsSub{handlers_ecr.SubjectUploadDelete, handleNATSRequest(d.ecrMetaService.UploadDelete), "spinifex-workers"},
		)
	}

	// EIP handlers are always registered: without external IPAM d.eipService is
	// the disabled stub, so reads return empty lists and mutations a clean
	// UnsupportedOperation instead of a NATS timeout.
	subs = append(subs,
		natsSub{"ec2.AllocateAddress", handleNATSRequest(d.eipService.AllocateAddress), "spinifex-workers"},
		natsSub{"ec2.ReleaseAddress", handleNATSRequest(d.eipService.ReleaseAddress), "spinifex-workers"},
		natsSub{"ec2.AssociateAddress", handleNATSRequest(d.eipService.AssociateAddress), "spinifex-workers"},
		natsSub{"ec2.DisassociateAddress", handleNATSRequest(d.eipService.DisassociateAddress), "spinifex-workers"},
		natsSub{"ec2.DescribeAddresses", handleNATSRequest(d.eipService.DescribeAddresses), "spinifex-workers"},
		natsSub{"ec2.DescribeAddressesAttribute", handleNATSRequest(d.eipService.DescribeAddressesAttribute), "spinifex-workers"},
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

	// Set the default KV replica count before any handler creates a bucket, so
	// lazily-created buckets are born at cluster-size replication instead of R1.
	if d.clusterConfig != nil {
		utils.SetDefaultKVReplicas(len(d.clusterConfig.Nodes))
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
	d.dnsWriter = handlers_dns.NewWriter(d.config, d.clusterConfig, d.natsConn)
	d.dnsReconciler = handlers_dns.NewReconciler(d.config, d.natsConn, d.dnsDesiredSet)
	d.dnsBaseDomain = handlers_dns.ResolveBaseDomain(d.config)
	d.dnsInternalDomain = handlers_dns.ResolveInternalDomain(d.config)
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

	d.elbv2Service.StartLifecycleReaper(context.Background())

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

	// Bind the mgmt IP allocator to cluster KV now that JetStream is up (DDIL
	// §1e-audit: startLocal built it KV-less). Rebuild then reconciles this
	// node's already-running VMs into the cluster-wide record instead of
	// just refreshing the local cache, so already-allocated IPs aren't
	// reused by another node either.
	if d.mgmtIPAllocator != nil && d.jsManager != nil {
		d.mgmtIPAllocator.BindKV(d.jsManager, d.node)
		d.mgmtIPAllocator.Rebuild(d.vmMgr.SnapshotMap())
		slog.Info("Rebuilt mgmt IP allocator from restored instances", "allocated", d.mgmtIPAllocator.AllocatedCount())
	}

	if err := d.subscribeAll(); err != nil {
		return fmt.Errorf("failed to subscribe to NATS topics: %w", err)
	}

	// DNS record writer: the single queue-group consumer of
	// dns.recordset.change. No-op when northstar S3 is not configured.
	if sub, err := d.dnsWriter.Subscribe(d.natsConn); err != nil {
		return fmt.Errorf("failed to subscribe DNS record writer: %w", err)
	} else if sub != nil {
		d.natsSubscriptions[handlers_dns.SubjectRecordsetChange] = sub
		slog.Info("Subscribed DNS record writer", "subject", handlers_dns.SubjectRecordsetChange, "queue", handlers_dns.QueueGroup)
	}

	// DNS drift backstop: periodically rebuild managed records
	// from the live cross-tenant inventory and converge the zone. It publishes
	// through the same queue-group writer, so every node running it serialises on
	// one writer and never races the zone. No-op when northstar is not configured.
	if d.dnsReconciler.Enabled() {
		go d.dnsReconciler.Run(d.ctx)
		slog.Info("Started DNS reconcile backstop", "interval", handlers_dns.DefaultReconcileInterval)
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
			d.vmMgr.NewStuckTerminateReaper(),
		}
		if eniRec := d.newENIReconciler(); eniRec != nil {
			reapers = append(reapers, eniRec)
		}
		if gpuRec := d.newGPUPoolReconciler(); gpuRec != nil {
			reapers = append(reapers, gpuRec)
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
				if errors.Is(err, nats.ErrBadSubscription) {
					slog.Debug("NATS subscription already invalid during shutdown", "subject", sub.Subject)
				} else {
					slog.Error("Error unsubscribing from NATS", "err", err)
				}
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
//
// GPU instance types (whole-GPU and MIG alike) are gated on BOTH the cpu/mem
// calc AND GPU-slot availability. cpu/mem still applies to whole-GPU types —
// the guest still consumes real host vCPU/RAM even though a GPU backs it —
// and GPU-slot count is an additional hard constraint layered on top, so
// admission never advertises more instances than there are GPU slots to
// back them.
func (rm *ResourceManager) canAllocateLocked(instanceType *ec2.InstanceTypeInfo, count int) int {
	instanceTypeName := ""
	if instanceType.InstanceType != nil {
		instanceTypeName = *instanceType.InstanceType
	}

	requiresGPU := instancetypes.IsGPUType(instanceType)
	availGPU := 0
	if requiresGPU && rm.gpuManager != nil {
		gpusNeeded := instancetypes.GPUCountForType(instanceTypeName)
		if gpusNeeded > 0 {
			availGPU = rm.gpuManager.Available() / gpusNeeded
		}
	}

	n := canAllocateCount(
		rm.hostVCPU-rm.reservedVCPU-rm.reservedCRVCPU, rm.allocatedVCPU,
		rm.hostMemGB-rm.reservedMem-rm.reservedCRMem, rm.allocatedMem,
		instanceTypeVCPUs(instanceType),
		rm.instanceMemChargeMiB(instanceType),
		count,
		availGPU, requiresGPU,
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

// deallocate releases resources for an instance and updates NATS subscriptions.
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
