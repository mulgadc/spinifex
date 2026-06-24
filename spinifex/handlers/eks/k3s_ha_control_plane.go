package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// haControlPlaneCount is the HA spread width (odd quorum tolerating one host loss).
// Fewer schedulable hosts fall back to a single CP VM.
const haControlPlaneCount = 3

// controlPlanePlacer is the spread-placement surface for HA CP orchestration.
type controlPlanePlacer interface {
	CreatePlacementGroup(*ec2.CreatePlacementGroupInput, string) (*ec2.CreatePlacementGroupOutput, error)
	DeletePlacementGroup(*ec2.DeletePlacementGroupInput, string) (*ec2.DeletePlacementGroupOutput, error)
	ReserveSpreadNodes(*handlers_ec2_placementgroup.ReserveSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReserveSpreadNodesOutput, error)
	ReleaseSpreadNodes(*handlers_ec2_placementgroup.ReleaseSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReleaseSpreadNodesOutput, error)
	FinalizeSpreadInstances(*handlers_ec2_placementgroup.FinalizeSpreadInstancesInput, string) (*handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput, error)
	RemoveInstance(*handlers_ec2_placementgroup.RemoveInstanceInput, string) (*handlers_ec2_placementgroup.RemoveInstanceOutput, error)
}

var _ controlPlanePlacer = (handlers_ec2_placementgroup.PlacementGroupService)(nil)

// HostScheduler answers capacity + placement fan-out questions for HA CP placement.
type HostScheduler interface {
	// SchedulableHosts returns node IDs that can fit at least one VM of instanceType.
	SchedulableHosts(instanceType string) []string
	// InstanceHosts maps each instance ID to its hosting node; absent entries are not yet visible.
	InstanceHosts(instanceIDs []string) map[string]string
}

// placeControlPlane places the cluster's CP VMs. With ≥ haControlPlaneCount
// schedulable hosts it spreads servers all-or-nothing; otherwise falls back to
// a single CP. Returns placed nodes ([0] = primary) and spread group name ("" for single-CP).
func (s *EKSServiceImpl) placeControlPlane(accountID, clusterName string, tmpl K3sServerInput) ([]ControlPlaneNode, string, error) {
	instanceType := tmpl.InstanceType
	if instanceType == "" {
		instanceType = defaultK3sServerInstanceType
	}

	hosts := s.deps.Scheduler.SchedulableHosts(instanceType)
	if len(hosts) < haControlPlaneCount {
		slog.Info("placeControlPlane: insufficient hosts for HA spread, launching single control plane",
			"cluster", clusterName, "schedulableHosts", len(hosts), "want", haControlPlaneCount)
		return s.launchSingleControlPlane(tmpl)
	}

	pgAccount := admin.SystemAccountID()
	groupName := haSpreadGroupName(accountID, clusterName)
	if err := s.ensureSpreadGroup(groupName, pgAccount); err != nil {
		return nil, "", err
	}

	reserve, err := s.deps.PlacementGroup.ReserveSpreadNodes(&handlers_ec2_placementgroup.ReserveSpreadNodesInput{
		GroupName:     groupName,
		EligibleNodes: hosts,
		MinCount:      haControlPlaneCount,
		MaxCount:      haControlPlaneCount,
	}, pgAccount)
	if err != nil {
		// Capacity raced away between fan-out and reservation; fall back to single CP.
		slog.Warn("placeControlPlane: spread reservation failed, falling back to single control plane",
			"cluster", clusterName, "group", groupName, "err", err)
		s.deleteSpreadGroup(groupName, pgAccount)
		return s.launchSingleControlPlane(tmpl)
	}
	reserved := reserve.ReservedNodes

	results := s.launchControlPlaneSpread(tmpl, reserved)

	var launched []ControlPlaneNode
	var launchErrs []error
	for _, r := range results {
		if r.err != nil {
			launchErrs = append(launchErrs, fmt.Errorf("node %s: %w", r.node, r.err))
			continue
		}
		launched = append(launched, controlPlaneNode(r.node, r.out))
	}

	// All-or-nothing: any launch failure rolls back all VMs, releases the reservation.
	if len(launchErrs) > 0 {
		slog.Error("placeControlPlane: partial spread launch, rolling back",
			"cluster", clusterName, "launched", len(launched), "failed", len(launchErrs))
		s.rollbackControlPlaneSpread(accountID, groupName, pgAccount, launched, reserved)
		return nil, "", fmt.Errorf("eks: HA control-plane launch failed: %w", errors.Join(launchErrs...))
	}

	// Verify each VM landed on its reserved host. Not-yet-visible instances are
	// tolerated; only a definitive wrong-host placement triggers rollback.
	if err := s.verifyControlPlaneSpread(launched); err != nil {
		slog.Error("placeControlPlane: placement verification failed, rolling back",
			"cluster", clusterName, "err", err)
		s.rollbackControlPlaneSpread(accountID, groupName, pgAccount, launched, reserved)
		return nil, "", err
	}

	nodeInstances := make(map[string][]string, len(launched))
	for _, n := range launched {
		nodeInstances[n.NodeID] = []string{n.InstanceID}
	}
	if _, err := s.deps.PlacementGroup.FinalizeSpreadInstances(&handlers_ec2_placementgroup.FinalizeSpreadInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, pgAccount); err != nil {
		slog.Error("placeControlPlane: finalize failed, rolling back", "cluster", clusterName, "err", err)
		s.rollbackControlPlaneSpread(accountID, groupName, pgAccount, launched, reserved)
		return nil, "", fmt.Errorf("eks: finalize HA control-plane placement: %w", err)
	}

	slog.Info("placeControlPlane: HA control plane placed",
		"cluster", clusterName, "group", groupName, "nodes", reserved)
	return launched, groupName, nil
}

// launchSingleControlPlane launches one CP VM on the local node (pre-HA fallback).
func (s *EKSServiceImpl) launchSingleControlPlane(tmpl K3sServerInput) ([]ControlPlaneNode, string, error) {
	in := tmpl
	in.TargetNodeID = ""
	in.KonnServerCount = 1
	out, err := LaunchK3sServerVM(s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
	if err != nil {
		return nil, "", err
	}
	return []ControlPlaneNode{controlPlaneNode("", out)}, "", nil
}

type cpLaunchResult struct {
	node string
	out  *K3sServerOutput
	err  error
}

// launchControlPlaneSpread launches the reserved nodes as one etcd quorum.
// nodes[0] is the cluster-init server whose ENI IP the remaining servers join;
// nodes[1..] launch in parallel. Results are index-aligned with nodes.
func (s *EKSServiceImpl) launchControlPlaneSpread(tmpl K3sServerInput, nodes []string) []cpLaunchResult {
	results := make([]cpLaunchResult, len(nodes))

	tmpl.KonnServerCount = len(nodes)
	first := tmpl
	first.TargetNodeID = nodes[0]
	out, err := LaunchK3sServerVM(s.deps.VPCK3s, s.deps.Instance, s.deps.Image, first)
	results[0] = cpLaunchResult{node: nodes[0], out: out, err: err}
	if err != nil {
		for i := 1; i < len(nodes); i++ {
			results[i] = cpLaunchResult{node: nodes[i], err: errFirstServerFailed}
		}
		return results
	}

	joinURL := k3sServerJoinURL(out.ENIIP)
	var wg sync.WaitGroup
	for i := 1; i < len(nodes); i++ {
		wg.Add(1)
		go func(idx int, nodeID string) {
			defer wg.Done()
			in := tmpl
			in.TargetNodeID = nodeID
			in.ServerURL = joinURL
			o, e := LaunchK3sServerVM(s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
			results[idx] = cpLaunchResult{node: nodeID, out: o, err: e}
		}(i, nodes[i])
	}
	wg.Wait()
	return results
}

// errFirstServerFailed marks join results skipped when the cluster-init server failed to launch.
var errFirstServerFailed = errors.New("eks: first control-plane server failed; join servers skipped")

// verifyControlPlaneSpread confirms every CP VM sits on its reserved host with
// no duplicates. VMs not yet visible to the fan-out are tolerated.
func (s *EKSServiceImpl) verifyControlPlaneSpread(nodes []ControlPlaneNode) error {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.InstanceID)
	}
	hosts := s.deps.Scheduler.InstanceHosts(ids)

	seen := make(map[string]string, len(nodes)) // host -> instanceID
	for _, n := range nodes {
		actual, ok := hosts[n.InstanceID]
		if !ok {
			slog.Warn("verifyControlPlaneSpread: instance not yet visible in node fan-out",
				"instanceId", n.InstanceID, "expectedNode", n.NodeID)
			continue
		}
		if actual != n.NodeID {
			return fmt.Errorf("eks: control-plane VM %s landed on %s, expected %s", n.InstanceID, actual, n.NodeID)
		}
		if other, dup := seen[actual]; dup {
			return fmt.Errorf("eks: control-plane VMs %s and %s share host %s", other, n.InstanceID, actual)
		}
		seen[actual] = n.InstanceID
	}
	return nil
}

// rollbackControlPlaneSpread terminates launched VMs, releases the reservation,
// and drops the group. Best-effort: a leaked internal group is harmless.
func (s *EKSServiceImpl) rollbackControlPlaneSpread(accountID, groupName, pgAccount string, launched []ControlPlaneNode, reserved []string) {
	for _, n := range launched {
		if err := TerminateK3sServerVM(s.deps.VPCK3s, s.deps.Instance, accountID, n.InstanceID, n.ENIID); err != nil {
			slog.Warn("rollbackControlPlaneSpread: terminate failed", "instanceId", n.InstanceID, "err", err)
		}
	}
	if _, err := s.deps.PlacementGroup.ReleaseSpreadNodes(&handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
		GroupName: groupName,
		Nodes:     reserved,
	}, pgAccount); err != nil {
		slog.Warn("rollbackControlPlaneSpread: release nodes failed", "group", groupName, "err", err)
	}
	s.deleteSpreadGroup(groupName, pgAccount)
}

// ensureSpreadGroup creates the spread placement group, reusing it if it already exists.
func (s *EKSServiceImpl) ensureSpreadGroup(groupName, pgAccount string) error {
	_, err := s.deps.PlacementGroup.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(groupName),
		Strategy:  aws.String(ec2.PlacementStrategySpread),
	}, pgAccount)
	if err == nil {
		return nil
	}
	if awserrors.IsErrorCode(err, awserrors.ErrorInvalidPlacementGroupDuplicate) {
		slog.Info("ensureSpreadGroup: spread group exists, reusing", "group", groupName)
		return nil
	}
	return fmt.Errorf("eks: create spread placement group %s: %w", groupName, err)
}

func (s *EKSServiceImpl) deleteSpreadGroup(groupName, pgAccount string) {
	if _, err := s.deps.PlacementGroup.DeletePlacementGroup(&ec2.DeletePlacementGroupInput{
		GroupName: aws.String(groupName),
	}, pgAccount); err != nil {
		slog.Warn("deleteSpreadGroup: delete failed", "group", groupName, "err", err)
	}
}

// teardownSpreadGroup removes CP instances from the spread group and deletes it.
// No-op for single-CP clusters (ControlPlaneSpreadGroup == "").
func (s *EKSServiceImpl) teardownSpreadGroup(meta *ClusterMeta) {
	if meta.ControlPlaneSpreadGroup == "" {
		return
	}
	pgAccount := admin.SystemAccountID()
	for _, cp := range meta.ControlPlaneNodes {
		if cp.NodeID == "" || cp.InstanceID == "" {
			continue
		}
		if _, err := s.deps.PlacementGroup.RemoveInstance(&handlers_ec2_placementgroup.RemoveInstanceInput{
			GroupName:  meta.ControlPlaneSpreadGroup,
			NodeName:   cp.NodeID,
			InstanceID: cp.InstanceID,
		}, pgAccount); err != nil {
			slog.Warn("teardownSpreadGroup: remove instance failed",
				"group", meta.ControlPlaneSpreadGroup, "instanceId", cp.InstanceID, "err", err)
		}
	}
	s.deleteSpreadGroup(meta.ControlPlaneSpreadGroup, pgAccount)
}

// controlPlaneTeardownNodes returns CP VMs for DeleteCluster. Prefers
// ControlPlaneNodes; falls back to scalar fields for older persisted clusters.
func controlPlaneTeardownNodes(meta *ClusterMeta) []ControlPlaneNode {
	if len(meta.ControlPlaneNodes) > 0 {
		return meta.ControlPlaneNodes
	}
	if meta.ControlPlaneInstanceID == "" && meta.ControlPlaneENIID == "" {
		return nil
	}
	return []ControlPlaneNode{{
		InstanceID: meta.ControlPlaneInstanceID,
		ENIID:      meta.ControlPlaneENIID,
		ENIIP:      meta.ControlPlaneENIIP,
		MgmtIP:     meta.ControlPlaneMgmtIP,
	}}
}

// haSpreadGroupName namespaces the spread group by account + cluster so same-named
// clusters across tenants don't collide. Lives under the system account.
func haSpreadGroupName(accountID, clusterName string) string {
	return "eks-cp-" + accountID + "-" + clusterName
}

func controlPlaneNode(nodeID string, out *K3sServerOutput) ControlPlaneNode {
	return ControlPlaneNode{
		NodeID:     nodeID,
		InstanceID: out.InstanceID,
		ENIID:      out.ENIID,
		ENIIP:      out.ENIIP,
		MgmtIP:     out.MgmtIP,
	}
}

// --- NATS host scheduler ---

var _ HostScheduler = (*natsHostScheduler)(nil)

// hostFanoutTimeout bounds a node status/VMs fan-out. Short fixed window
// trades a little latency for reliably seeing all hosts before placement.
const hostFanoutTimeout = time.Second

type natsHostScheduler struct {
	nc *nats.Conn
}

// NewNATSHostScheduler returns a NATS fan-out HostScheduler for EKSServiceDeps.
func NewNATSHostScheduler(nc *nats.Conn) HostScheduler {
	return &natsHostScheduler{nc: nc}
}

func (h *natsHostScheduler) SchedulableHosts(instanceType string) []string {
	// sys.* types are hidden from customers in node.status but still consume
	// vCPU/memory, so their fit is checked against raw schedulable headroom.
	// Customer types (e.g. nodegroup workers) advertise per-type Available in
	// node.status, so fit reads that directly.
	isSystem := instancetypes.IsSystemType(instanceType)
	var vcpu int
	var memGB float64
	if isSystem {
		var ok bool
		if vcpu, memGB, ok = instancetypes.SpecForSystemType(instanceType); !ok {
			slog.Warn("SchedulableHosts: unknown system instance type, no schedulable hosts",
				"instanceType", instanceType)
			return nil
		}
	}

	var hosts []string
	seen := make(map[string]bool)
	h.fanout("spinifex.node.status", func(data []byte) {
		var st types.NodeStatusResponse
		if json.Unmarshal(data, &st) != nil || st.Node == "" || seen[st.Node] {
			return
		}
		fits := nodeFitsCustomerInstance(st, instanceType)
		if isSystem {
			fits = nodeFitsSystemInstance(st, vcpu, memGB)
		}
		if fits {
			seen[st.Node] = true
			hosts = append(hosts, st.Node)
		}
	})
	return hosts
}

// nodeFitsCustomerInstance reports whether a node advertises at least one free
// slot for the given customer instance type in its node.status capacity.
func nodeFitsCustomerInstance(st types.NodeStatusResponse, instanceType string) bool {
	for _, c := range st.InstanceTypes {
		if c.Name == instanceType && c.Available >= 1 {
			return true
		}
	}
	return false
}

// nodeFitsSystemInstance reports whether a node's headroom (Total - Reserved - Alloc)
// fits at least one VM of the given vCPU/memory footprint.
func nodeFitsSystemInstance(st types.NodeStatusResponse, vcpu int, memGB float64) bool {
	remainVCPU := st.TotalVCPU - st.ReservedVCPU - st.AllocVCPU
	remainMem := st.TotalMemGB - st.ReservedMemGB - st.AllocMemGB
	return remainVCPU >= vcpu && remainMem >= memGB
}

func (h *natsHostScheduler) InstanceHosts(instanceIDs []string) map[string]string {
	want := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		want[id] = true
	}
	out := make(map[string]string)
	h.fanout("spinifex.node.vms", func(data []byte) {
		var resp types.NodeVMsResponse
		if json.Unmarshal(data, &resp) != nil || resp.Node == "" {
			return
		}
		for _, vm := range resp.VMs {
			if want[vm.InstanceID] {
				out[vm.InstanceID] = resp.Node
			}
		}
	})
	return out
}

// fanout publishes on subject and calls handle for each reply within hostFanoutTimeout.
func (h *natsHostScheduler) fanout(subject string, handle func([]byte)) {
	if h.nc == nil {
		return
	}
	inbox := nats.NewInbox()
	sub, err := h.nc.SubscribeSync(inbox)
	if err != nil {
		slog.Warn("hostScheduler: subscribe inbox failed", "subject", subject, "err", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	msg := nats.NewMsg(subject)
	msg.Reply = inbox
	msg.Data = []byte("{}")
	if err := h.nc.PublishMsg(msg); err != nil {
		slog.Warn("hostScheduler: publish failed", "subject", subject, "err", err)
		return
	}

	deadline := time.Now().Add(hostFanoutTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		reply, err := sub.NextMsg(remaining)
		if err != nil {
			return
		}
		handle(reply.Data)
	}
}
