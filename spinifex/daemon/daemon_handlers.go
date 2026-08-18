package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/formation"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const daemonTracerName = "github.com/mulgadc/spinifex/spinifex/daemon"

// startOpSpan opens a child span for a named instance operation under ctx.
func startOpSpan(ctx context.Context, op, instanceID string) (context.Context, trace.Span) {
	return otel.Tracer(daemonTracerName).Start(ctx, op,
		trace.WithAttributes(attribute.String("instance.id", instanceID)))
}

// endOpSpan records err (if any) on span and ends it.
func endOpSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// respondWithError sends an error payload for the given error code on the NATS message.
func respondWithError(msg *nats.Msg, errCode string) {
	if err := msg.Respond(utils.GenerateErrorPayload(errCode)); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
}

// respondWithServiceError sends the sanitized error code AND the handler's
// message, so a caller sees the actionable reason rather than a bare code. Use
// it for any error originating in a service call: the code alone collapses a
// specific refusal ("only PRIVATE_CA certificates can be force-renewed") into
// an opaque ServerInternal, leaving the reason visible only in the daemon log.
// Mirrors utils.ServeNATSRequestCtx, which has always preserved the message.
func respondWithServiceError(msg *nats.Msg, err error) {
	payload := utils.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), err.Error())
	if respErr := msg.Respond(payload); respErr != nil {
		slog.Error("Failed to respond to NATS request", "err", respErr)
	}
}

// respondErrorOutcome answers with errCode and reports the outcome that code
// implies, so a handler classifies its failure by the code it already chose
// rather than by a second judgement that could disagree with it.
func respondErrorOutcome(msg *nats.Msg, errCode string) string {
	respondWithError(msg, errCode)
	return outcomeForCode(errCode)
}

// respondServiceErrorOutcome is respondErrorOutcome for an error value.
func respondServiceErrorOutcome(msg *nats.Msg, err error) string {
	respondWithServiceError(msg, err)
	return outcomeForError(err)
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

// handleNATSRequest returns the nats.MsgHandler for the common unmarshal → service →
// marshal → respond pattern. Per message the handler opens a consumer span joining the
// producer's trace, extracts the account ID from the NATS message header, and passes
// both to the service function.
func handleNATSRequest[I any, O any](serviceFn func(context.Context, *I, string) (*O, error)) natsHandler {
	return func(msg *nats.Msg) string {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()

		accountID := utils.AccountIDFromMsg(msg)
		// Carried in ctx rather than the service signature so only the handlers
		// that must deduplicate a retry have to look for it.
		ctx = utils.WithIdempotencyKey(ctx, utils.IdempotencyKeyFromMsg(msg))
		input := new(I)
		if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
			utils.MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
			if err := msg.Respond(errResp); err != nil {
				slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
			}
			// A payload the daemon cannot parse is the caller's mistake, not a
			// fault of its own.
			return outcomeClientError
		}
		output, err := serviceFn(ctx, input, accountID)
		if err != nil {
			// The error was otherwise only recorded on the OTel span, invisible
			// without a trace backend, so it is logged here too — at a level
			// that says whether the daemon or its caller was at fault.
			logHandlerError(ctx, "handleNATSRequest: service call failed", msg.Subject, err)
			utils.MarkSpanError(span, err)
			respondWithServiceError(msg, err)
			return outcomeForError(err)
		}
		respondWithJSON(msg, output)
		return outcomeSuccess
	}
}

// handleNATSRequestWithPrincipal is handleNATSRequest for service methods that
// also need the caller's IAM principal ARN (X-Principal-ARN header) — e.g. EKS
// CreateCluster, which mints the bootstrap-creator-admin AccessEntry for the
// caller.
func handleNATSRequestWithPrincipal[I any, O any](serviceFn func(context.Context, *I, string, string) (*O, error)) natsHandler {
	return func(msg *nats.Msg) string {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()

		accountID := utils.AccountIDFromMsg(msg)
		principalARN := utils.PrincipalARNFromMsg(msg)
		input := new(I)
		if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
			utils.MarkSpanError(span, errors.New(awserrors.ErrorInvalidParameterValue))
			if err := msg.Respond(errResp); err != nil {
				slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
			}
			return outcomeClientError
		}
		output, err := serviceFn(ctx, input, accountID, principalARN)
		if err != nil {
			logHandlerError(ctx, "handleNATSRequestWithPrincipal: service call failed", msg.Subject, err)
			utils.MarkSpanError(span, err)
			respondWithServiceError(msg, err)
			return outcomeForError(err)
		}
		respondWithJSON(msg, output)
		return outcomeSuccess
	}
}

// handleEC2Events processes incoming EC2 instance events (start, stop, terminate, attach-volume).
// It reports its own action and outcome rather than returning them, because one
// subject carries a dozen different commands and the wrapper cannot name which
// one ran.
func (d *Daemon) handleEC2Events(msg *nats.Msg) {
	start := time.Now()
	action, outcome := d.dispatchEC2Command(msg)
	if outcome == outcomeDeferred {
		return
	}
	otelsetup.RecordRequest(context.Background(), ec2CmdAction(action), outcome, time.Since(start))
}

// dispatchEC2Command runs one per-instance command and returns the command's
// name and how it answered. A command that reaches no case is named rather than
// dropped, so an attribute nothing handles is visible in the metric.
func (d *Daemon) dispatchEC2Command(msg *nats.Msg) (string, string) {
	ctx, span := utils.StartConsumerSpan(msg)
	defer span.End()

	var command types.EC2InstanceCommand

	if err := json.Unmarshal(msg.Data, &command); err != nil {
		slog.ErrorContext(ctx, "Error unmarshaling EC2 instance command", "err", err)
		utils.MarkSpanError(span, err)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return "unknown", outcomeError
	}

	slog.DebugContext(ctx, "Received message", "subject", msg.Subject, "data", string(msg.Data))

	name := ec2CommandName(command)

	instance, ok := d.vmMgr.Get(command.ID)
	if !ok {
		slog.WarnContext(ctx, "Instance is not running on this node", "id", command.ID)
		return name, respondErrorOutcome(msg, awserrors.ErrorInvalidInstanceIDNotFound)
	}

	// Verify the caller owns this instance
	if !checkInstanceOwnership(msg, command.ID, instance.AccountID) {
		return name, outcomeClientError
	}

	switch {
	case command.Attributes.AttachVolume:
		return name, d.handleAttachVolume(ctx, msg, command, instance)
	case command.Attributes.DetachVolume:
		return name, d.handleDetachVolume(ctx, msg, command, instance)
	case command.Attributes.DrainVolume:
		// A drain flushes the volume's whole dirty set to S3, so it is bounded by
		// the dirty set rather than by a unit of work. nats.go delivers a
		// subscription's messages serially, so running it inline would stall
		// every other command for this instance — stop, terminate, hot-plug —
		// for the duration. It touches no VM state, so it is safe off-thread.
		//
		// It therefore records its own point: timing the dispatch would call
		// every drain an instant success.
		started := time.Now()
		go func() {
			outcome := d.handleDrainVolume(ctx, msg, command, instance)
			otelsetup.RecordRequest(context.Background(), ec2CmdAction(name), outcome, time.Since(started))
		}()
		return name, outcomeDeferred
	case command.Attributes.AttachENI:
		return name, d.handleAttachNetworkInterface(ctx, msg, command, instance)
	case command.Attributes.DetachENI:
		return name, d.handleDetachNetworkInterface(ctx, msg, command, instance)
	case command.Attributes.AssociateIamInstanceProfile:
		return name, d.handleAssociateIamInstanceProfile(ctx, msg, command, instance)
	case command.Attributes.SetSpotLineage:
		return name, d.handleSetSpotLineage(ctx, msg, command)
	case command.Attributes.SetInstanceTags, command.Attributes.RemoveInstanceTags:
		return name, d.handleSetInstanceTags(ctx, msg, command, instance)
	case command.Attributes.StartInstance:
		opCtx, opSpan := startOpSpan(ctx, "ec2.StartInstance", instance.ID)
		err := d.instanceService.StartInstance(opCtx, instance, command)
		endOpSpan(opSpan, err)
		if err != nil {
			utils.MarkSpanError(span, err)
			return name, respondServiceErrorOutcome(msg, err)
		}
		if err := msg.Respond(fmt.Appendf(nil, `{"status":"running","instanceId":"%s"}`, instance.ID)); err != nil {
			slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
		}
		return name, outcomeSuccess
	case command.Attributes.RebootInstance:
		opCtx, opSpan := startOpSpan(ctx, "ec2.RebootInstance", instance.ID)
		err := d.instanceService.RebootInstance(opCtx, instance, command)
		endOpSpan(opSpan, err)
		if err != nil {
			utils.MarkSpanError(span, err)
			return name, respondServiceErrorOutcome(msg, err)
		}
		if err := msg.Respond([]byte(`{}`)); err != nil {
			slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
		}
		return name, outcomeSuccess
	case command.Attributes.StopInstance, command.Attributes.TerminateInstance:
		opName := "ec2.StopInstance"
		if command.Attributes.TerminateInstance {
			opName = "ec2.TerminateInstance"
		}
		opCtx, opSpan := startOpSpan(ctx, opName, instance.ID)
		err := d.instanceService.StopOrTerminateInstance(opCtx, instance, command)
		endOpSpan(opSpan, err)
		if err != nil {
			utils.MarkSpanError(span, err)
			return name, respondServiceErrorOutcome(msg, err)
		}
		if err := msg.Respond([]byte(`{}`)); err != nil {
			slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
		}
		return name, outcomeSuccess
	default:
		slog.WarnContext(ctx, "Unhandled EC2 instance command", "id", command.ID, "attributes", command.Attributes)
		return name, respondErrorOutcome(msg, awserrors.ErrorServerInternal)
	}
}

// ec2CommandName names the command an EC2InstanceCommand carries, for the
// metric action. Order matches the dispatch switch.
func ec2CommandName(command types.EC2InstanceCommand) string {
	switch {
	case command.Attributes.AttachVolume:
		return "AttachVolume"
	case command.Attributes.DetachVolume:
		return "DetachVolume"
	case command.Attributes.DrainVolume:
		return "DrainVolume"
	case command.Attributes.AttachENI:
		return "AttachNetworkInterface"
	case command.Attributes.DetachENI:
		return "DetachNetworkInterface"
	case command.Attributes.AssociateIamInstanceProfile:
		return "AssociateIamInstanceProfile"
	case command.Attributes.SetSpotLineage:
		return "SetSpotLineage"
	case command.Attributes.SetInstanceTags:
		return "SetInstanceTags"
	case command.Attributes.RemoveInstanceTags:
		return "RemoveInstanceTags"
	case command.Attributes.StartInstance:
		return "StartInstance"
	case command.Attributes.RebootInstance:
		return "RebootInstance"
	case command.Attributes.TerminateInstance:
		return "TerminateInstance"
	case command.Attributes.StopInstance:
		return "StopInstance"
	default:
		return "unknown"
	}
}

// --- Admin / node management handlers ---

// handleHealthCheck processes NATS health check requests.
func (d *Daemon) handleHealthCheck(msg *nats.Msg) string {
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
	return outcomeSuccess
}

// handleNodeDiscover responds to node discovery requests with this node's ID
// Used by the gateway to dynamically discover active spinifex nodes in the cluster.
func (d *Daemon) handleNodeDiscover(msg *nats.Msg) string {
	response := types.NodeDiscoverResponse{
		Node: d.node,
	}

	respondWithJSON(msg, response)
	slog.Debug("Node discovery responded", "node", d.node)
	return outcomeSuccess
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
func (d *Daemon) handleNodeStatus(msg *nats.Msg) string {
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

	// Query service roles concurrently to keep worst-case latency bounded.
	// OVN roles are probed only on DB quorum members; compute-only nodes skip
	// the shell-out entirely.
	var wg sync.WaitGroup
	wg.Go(func() { resp.NATSRole = d.queryNATSRole() })
	if d.isOVNDBQuorumMember() {
		wg.Go(func() { resp.OVNNBRole = host.OVNDBRole(host.OVNNBTarget, host.OVNNBSchema) })
		wg.Go(func() { resp.OVNSBRole = host.OVNDBRole(host.OVNSBTarget, host.OVNSBSchema) })
	}
	wg.Wait()

	respondWithJSON(msg, resp)
	return outcomeSuccess
}

const (
	roleLeader   = "leader"
	roleFollower = "follower"

	natsMonitorPort = 8222
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

// isOVNDBQuorumMember reports whether this node hosts the clustered OVN NB/SB
// databases. Compute-only nodes short-circuit OVN role probes to "" without
// shelling out to ovn-appctl.
func (d *Daemon) isOVNDBQuorumMember() bool {
	if d.clusterConfig == nil {
		return false
	}
	names := slices.Collect(maps.Keys(d.clusterConfig.Nodes))
	return slices.Contains(formation.OVNDBQuorumNames(names), d.node)
}

var roleHTTPClient = &http.Client{Timeout: 500 * time.Millisecond}

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

// handleNodeVMs responds with the list of VMs running on this node.
// Used by the CLI: spx get vms.
func (d *Daemon) handleNodeVMs(msg *nats.Msg) string {
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
	return outcomeSuccess
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
