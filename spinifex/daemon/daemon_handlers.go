package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// respondWithError sends an error payload for the given error code on the NATS message.
func respondWithError(msg *nats.Msg, errCode string) {
	if err := msg.Respond(utils.GenerateErrorPayload(errCode)); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
}

// respondWithJSON marshals data to JSON and sends it as a NATS response.
// On marshal failure it responds with an internal server error.
func respondWithJSON(msg *nats.Msg, data any) {
	jsonResponse, err := json.Marshal(data)
	if err != nil {
		slog.Error("Failed to marshal response", "type", fmt.Sprintf("%T", data), "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}
	if err := msg.Respond(jsonResponse); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
}

// handleNATSRequest is a generic helper for the common unmarshal → service → marshal → respond pattern.
// It extracts the account ID from the NATS message header and passes it to the service function.
func handleNATSRequest[I any, O any](msg *nats.Msg, serviceFn func(*I, string) (*O, error)) {
	accountID := utils.AccountIDFromMsg(msg)
	input := new(I)
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}
	output, err := serviceFn(input, accountID)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, output)
}

// handleNATSRequestWithPrincipal is handleNATSRequest for service methods that
// also need the caller's IAM principal ARN (X-Principal-ARN header) — e.g. EKS
// CreateCluster, which mints the bootstrap-creator-admin AccessEntry for the
// caller.
func handleNATSRequestWithPrincipal[I any, O any](msg *nats.Msg, serviceFn func(*I, string, string) (*O, error)) {
	accountID := utils.AccountIDFromMsg(msg)
	principalARN := utils.PrincipalARNFromMsg(msg)
	input := new(I)
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}
	output, err := serviceFn(input, accountID, principalARN)
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	respondWithJSON(msg, output)
}

// handleEC2Events processes incoming EC2 instance events (start, stop, terminate, attach-volume)
func (d *Daemon) handleEC2Events(msg *nats.Msg) {
	var command types.EC2InstanceCommand

	if err := json.Unmarshal(msg.Data, &command); err != nil {
		slog.Error("Error unmarshaling EC2 instance command", "err", err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	slog.Debug("Received message", "subject", msg.Subject, "data", string(msg.Data))

	instance, ok := d.vmMgr.Get(command.ID)
	if !ok {
		slog.Warn("Instance is not running on this node", "id", command.ID)
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	// Verify the caller owns this instance
	if !checkInstanceOwnership(msg, command.ID, instance.AccountID) {
		return
	}

	switch {
	case command.Attributes.AttachVolume:
		d.handleAttachVolume(msg, command, instance)
	case command.Attributes.DetachVolume:
		d.handleDetachVolume(msg, command, instance)
	case command.Attributes.AttachENI:
		d.handleAttachNetworkInterface(msg, command, instance)
	case command.Attributes.DetachENI:
		d.handleDetachNetworkInterface(msg, command, instance)
	case command.Attributes.AssociateIamInstanceProfile:
		d.handleAssociateIamInstanceProfile(msg, command, instance)
	case command.Attributes.SetSpotLineage:
		d.handleSetSpotLineage(msg, command)
	case command.Attributes.StartInstance:
		if err := d.instanceService.StartInstance(instance, command); err != nil {
			respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
			return
		}
		if err := msg.Respond(fmt.Appendf(nil, `{"status":"running","instanceId":"%s"}`, instance.ID)); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
	case command.Attributes.RebootInstance:
		if err := d.instanceService.RebootInstance(instance, command); err != nil {
			respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
			return
		}
		if err := msg.Respond([]byte(`{}`)); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
	case command.Attributes.StopInstance, command.Attributes.TerminateInstance:
		if err := d.instanceService.StopOrTerminateInstance(instance, command); err != nil {
			respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
			return
		}
		if err := msg.Respond([]byte(`{}`)); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
	default:
		slog.Warn("Unhandled EC2 instance command", "id", command.ID, "attributes", command.Attributes)
		respondWithError(msg, awserrors.ErrorServerInternal)
	}
}

// --- Admin / node management handlers ---

// handleHealthCheck processes NATS health check requests
func (d *Daemon) handleHealthCheck(msg *nats.Msg) {
	configHash, err := d.computeConfigHash()
	if err != nil {
		slog.Error("Failed to compute config hash for health check", "error", err)
		configHash = "error"
	}

	status := "running"
	if !d.ready.Load() {
		status = "starting"
	}

	response := types.NodeHealthResponse{
		Node:       d.node,
		Status:     status,
		ConfigHash: configHash,
		Epoch:      d.clusterConfig.Epoch,
		Uptime:     int64(time.Since(d.startTime).Seconds()),
	}

	respondWithJSON(msg, response)
	slog.Debug("Health check responded", "node", d.node, "epoch", d.clusterConfig.Epoch)
}

// handleNodeDiscover responds to node discovery requests with this node's ID
// Used by the gateway to dynamically discover active spinifex nodes in the cluster
func (d *Daemon) handleNodeDiscover(msg *nats.Msg) {
	response := types.NodeDiscoverResponse{
		Node: d.node,
	}

	respondWithJSON(msg, response)
	slog.Debug("Node discovery responded", "node", d.node)
}

// daemonIP extracts the IP portion from the daemon host (host:port format).
// Returns 127.0.0.1 when the host is 0.0.0.0 since that bind address is not
// a valid connect address and is excluded from cert SANs.
func (d *Daemon) daemonIP() string {
	host := d.config.Daemon.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "0.0.0.0" {
		return "127.0.0.1"
	}
	return host
}

// handleNodeStatus responds with this node's status and resource stats.
// Used by the CLI: spx get nodes, spx top nodes.
func (d *Daemon) handleNodeStatus(msg *nats.Msg) {
	totalVCPU, totalMemGB, reservedVCPU, reservedMemGB, allocVCPU, allocMemGB, caps := d.resourceMgr.GetResourceStats()

	vmCount := 0
	d.vmMgr.ForEach(func(v *vm.VM) {
		if v.Status == vm.StateRunning {
			vmCount++
		}
	})

	totalGPUs, allocGPUs := 0, 0
	if d.gpuManager != nil {
		totalGPUs = d.gpuManager.TotalCount()
		allocGPUs = d.gpuManager.AllocatedCount()
	}

	var gpuModelNames []string
	for _, dev := range d.gpuProbe.Devices {
		gpuModelNames = append(gpuModelNames, dev.Model)
	}

	var gpuInventory []types.GPUInfo
	if d.gpuManager != nil {
		gpuInventory = buildGPUInventory(d.gpuManager.Snapshot())
	}

	resp := types.NodeStatusResponse{
		Node:           d.node,
		Status:         "Ready",
		Host:           d.daemonIP(),
		Region:         d.config.Region,
		AZ:             d.config.AZ,
		Uptime:         int64(time.Since(d.startTime).Seconds()),
		Services:       d.config.GetServices(),
		TotalVCPU:      totalVCPU,
		TotalMemGB:     totalMemGB,
		ReservedVCPU:   reservedVCPU,
		ReservedMemGB:  reservedMemGB,
		AllocVCPU:      allocVCPU,
		AllocMemGB:     allocMemGB,
		TotalGPUs:      totalGPUs,
		AllocGPUs:      allocGPUs,
		GPUCapable:     d.gpuProbe.Capable,
		GPUPassthrough: d.gpuManager != nil,
		GPUModels:      gpuModelNames,
		GPUs:           gpuInventory,
		VMCount:        vmCount,
		InstanceTypes:  caps,
	}

	// Query service roles concurrently to halve worst-case latency (500ms vs 1s).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); resp.NATSRole = d.queryNATSRole() }()
	go func() { defer wg.Done(); resp.PredastoreRole = d.queryPredastoreRole() }()
	wg.Wait()

	respondWithJSON(msg, resp)
}

const (
	roleLeader   = "leader"
	roleFollower = "follower"

	natsMonitorPort  = 8222
	predastoreDBPort = 6660
)

// queryNATSRole queries the local NATS monitoring endpoint to determine this
// node's JetStream meta-leader status. Returns "leader", "follower", or "".
func (d *Daemon) queryNATSRole() string {
	if !d.config.HasService("nats") {
		return ""
	}
	url := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(natsMonitorPort)) + "/varz"
	return fetchNATSRole(url, roleHTTPClient)
}

// queryPredastoreRole queries the local Predastore DB status endpoint to
// determine this node's Raft role. Returns "leader", "follower", or "".
func (d *Daemon) queryPredastoreRole() string {
	if !d.config.HasService("predastore") {
		return ""
	}
	url := "https://" + net.JoinHostPort(d.daemonIP(), strconv.Itoa(predastoreDBPort)) + "/status"
	return fetchPredastoreRole(url, roleTLSHTTPClient)
}

var roleHTTPClient = &http.Client{Timeout: 500 * time.Millisecond}

var roleTLSHTTPClient = &http.Client{
	Timeout: 500 * time.Millisecond,
}

// fetchNATSRole queries a NATS /varz endpoint and returns "leader", "follower", or "".
func fetchNATSRole(url string, client *http.Client) string {
	resp, err := client.Get(url) //nolint:noctx // internal monitoring call
	if err != nil {
		slog.Debug("Failed to query NATS monitoring", "err", err)
		return ""
	}
	defer resp.Body.Close()

	var varz struct {
		ServerName string `json:"server_name"`
		JetStream  struct {
			Meta struct {
				Leader      string `json:"leader"`
				ClusterSize int    `json:"cluster_size"`
			} `json:"meta"`
		} `json:"jetstream"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&varz); err != nil {
		slog.Debug("Failed to decode NATS varz", "err", err)
		return ""
	}

	// Single node or no meta cluster — this node is the leader by default
	if varz.JetStream.Meta.ClusterSize <= 1 {
		return roleLeader
	}
	if varz.JetStream.Meta.Leader == varz.ServerName {
		return roleLeader
	}
	return roleFollower
}

// buildGPUInventory converts a pool snapshot into per-physical-GPU GPUInfo
// records suitable for the NodeStatusResponse. Entries are ordered by first
// appearance of each PCI address in the snapshot.
func buildGPUInventory(snapshot []gpu.PoolEntry) []types.GPUInfo {
	byPCI := make(map[string]*types.GPUInfo, len(snapshot))
	var order []string

	for _, e := range snapshot {
		pci := e.Device.PCIAddress
		if _, ok := byPCI[pci]; !ok {
			byPCI[pci] = &types.GPUInfo{
				PCIAddress: pci,
				Model:      e.Device.Model,
				VRAMMiB:    e.Device.MemoryMiB,
			}
			order = append(order, pci)
		}
		g := byPCI[pci]
		if e.MIGInstance != nil {
			g.MIGEnabled = true
			g.MIGProfile = e.MIGInstance.Profile.Name
			g.Slices = append(g.Slices, types.GPUSliceInfo{
				GIID:       e.MIGInstance.GIID,
				Profile:    e.MIGInstance.Profile.Name,
				VRAMMiB:    e.MIGInstance.Profile.MemoryMiB,
				MdevPath:   e.MIGInstance.MdevPath,
				InstanceID: e.InstanceID,
			})
		} else if g.InstanceID == "" {
			g.InstanceID = e.InstanceID
		}
	}

	gpus := make([]types.GPUInfo, 0, len(order))
	for _, pci := range order {
		gpus = append(gpus, *byPCI[pci])
	}
	return gpus
}

// buildPoolLookup snapshots the GPU manager and returns two lookup maps:
// mdev path → PoolEntry (for MIG slices) and PCI address → PoolEntry (for
// whole-GPU entries). Both maps are nil when manager is nil.
func buildPoolLookup(mgr *gpu.Manager) (byMdev, byPCI map[string]gpu.PoolEntry) {
	if mgr == nil {
		return nil, nil
	}
	snap := mgr.Snapshot()
	byMdev = make(map[string]gpu.PoolEntry, len(snap))
	byPCI = make(map[string]gpu.PoolEntry, len(snap))
	for _, e := range snap {
		if e.MIGInstance != nil {
			byMdev[e.MIGInstance.MdevPath] = e
		} else {
			byPCI[e.Device.PCIAddress] = e
		}
	}
	return byMdev, byPCI
}

// resolveVMGPU maps a single GPUAttachment to a VMGPUInfo using the pool
// lookup tables built by buildPoolLookup. Returns nil if the attachment cannot
// be matched (e.g. daemon restart before pool is fully restored).
func resolveVMGPU(att gpu.GPUAttachment, byMdev, byPCI map[string]gpu.PoolEntry) *types.VMGPUInfo {
	if att.MdevPath != "" {
		if e, ok := byMdev[att.MdevPath]; ok && e.MIGInstance != nil {
			return &types.VMGPUInfo{
				Model:    e.Device.Model,
				VRAMMiB:  e.MIGInstance.Profile.MemoryMiB,
				Profile:  e.MIGInstance.Profile.Name,
				MdevPath: att.MdevPath,
			}
		}
		return nil
	}
	if att.PCIAddress != "" {
		if e, ok := byPCI[att.PCIAddress]; ok {
			return &types.VMGPUInfo{
				Model:      e.Device.Model,
				VRAMMiB:    e.Device.MemoryMiB,
				PCIAddress: att.PCIAddress,
			}
		}
	}
	return nil
}

// fetchPredastoreRole queries a Predastore /status endpoint and returns "leader", "follower", or "".
func fetchPredastoreRole(url string, client *http.Client) string {
	resp, err := client.Get(url) //nolint:noctx // internal monitoring call
	if err != nil {
		slog.Debug("Failed to query Predastore status", "err", err)
		return ""
	}
	defer resp.Body.Close()

	var status struct {
		IsLeader bool `json:"is_leader"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		slog.Debug("Failed to decode Predastore status", "err", err)
		return ""
	}

	if status.IsLeader {
		return roleLeader
	}
	return roleFollower
}

// handleNodeVMs responds with the list of VMs running on this node.
// Used by the CLI: spx get vms.
func (d *Daemon) handleNodeVMs(msg *nats.Msg) {
	poolByMdev, poolByPCI := buildPoolLookup(d.gpuManager)

	vms := make([]types.VMInfo, 0, d.vmMgr.Count())
	d.vmMgr.ForEach(func(v *vm.VM) {
		info := types.VMInfo{
			InstanceID:   v.ID,
			Status:       string(v.Status),
			InstanceType: v.InstanceType,
			ManagedBy:    v.ManagedBy,
			Health:       vmHealthLabel(v),
			CrashCount:   v.Health.CrashCount,
		}
		if it, ok := d.resourceMgr.instanceTypes[v.InstanceType]; ok {
			info.VCPU = int(instanceTypeVCPUs(it))
			info.MemoryGB = float64(instanceTypeMemoryMiB(it)) / 1024.0
		}
		if v.Instance != nil && v.Instance.LaunchTime != nil {
			info.LaunchTime = v.Instance.LaunchTime.Unix()
		}
		if len(v.GPUAttachments) > 0 {
			info.GPU = resolveVMGPU(v.GPUAttachments[0], poolByMdev, poolByPCI)
		}
		vms = append(vms, info)
	})

	resp := types.NodeVMsResponse{
		Node: d.node,
		Host: d.daemonIP(),
		VMs:  vms,
	}

	respondWithJSON(msg, resp)
}

// vmHealthLabel derives the display health for spx get vms. Only running VMs
// carry health: QMP-unresponsive past the failure gate is impaired, a VM that
// has crashed before but is running again is recovering, otherwise ok.
func vmHealthLabel(v *vm.VM) string {
	if v.Status != vm.StateRunning {
		return "-"
	}
	if v.Health.QMPConsecutiveFailures >= vm.QMPMaxConsecutiveFailures {
		return "impaired"
	}
	if v.Health.CrashCount > 0 {
		return "recovering"
	}
	return "ok"
}
