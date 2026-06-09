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

// haControlPlaneCount is the control-plane spread width: an odd quorum of three
// so an embedded-etcd control plane tolerates a single host loss. Fewer than
// this many schedulable hosts falls back to a single control-plane VM (today's
// behaviour) instead of failing the create.
const haControlPlaneCount = 3

// controlPlanePlacer is the spread-placement surface the HA control-plane
// orchestrator needs. handlers/ec2/placementgroup.PlacementGroupService (wired
// by the daemon as NewNATSPlacementGroupService) satisfies it.
type controlPlanePlacer interface {
	CreatePlacementGroup(*ec2.CreatePlacementGroupInput, string) (*ec2.CreatePlacementGroupOutput, error)
	DeletePlacementGroup(*ec2.DeletePlacementGroupInput, string) (*ec2.DeletePlacementGroupOutput, error)
	ReserveSpreadNodes(*handlers_ec2_placementgroup.ReserveSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReserveSpreadNodesOutput, error)
	ReleaseSpreadNodes(*handlers_ec2_placementgroup.ReleaseSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReleaseSpreadNodesOutput, error)
	FinalizeSpreadInstances(*handlers_ec2_placementgroup.FinalizeSpreadInstancesInput, string) (*handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput, error)
	RemoveInstance(*handlers_ec2_placementgroup.RemoveInstanceInput, string) (*handlers_ec2_placementgroup.RemoveInstanceOutput, error)
}

var _ controlPlanePlacer = (handlers_ec2_placementgroup.PlacementGroupService)(nil)

// HostScheduler answers the capacity + placement fan-out questions the HA
// control-plane orchestrator asks. The daemon wires a NATS implementation; unit
// tests fake it so the orchestration logic needs no live cluster.
type HostScheduler interface {
	// SchedulableHosts returns the distinct node IDs that can fit at least one
	// VM of instanceType.
	SchedulableHosts(instanceType string) []string
	// InstanceHosts maps each instance ID to the node currently hosting it.
	// Instances absent from the result are not (yet) visible to the fan-out.
	InstanceHosts(instanceIDs []string) map[string]string
}

// placeControlPlane places the cluster's control-plane VM(s). With at least
// haControlPlaneCount schedulable hosts it spreads that many k3s server VMs
// across distinct hosts, all-or-nothing; otherwise it launches a single
// control-plane VM on the local node (today's behaviour). Returns the placed
// nodes ([0] is the primary the NLB + egress wire to until 231.7.3) and the
// spread placement-group name ("" for the single-CP fallback) the caller
// persists for DeleteCluster teardown.
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
		// Could not reserve the full quorum of distinct hosts (capacity raced
		// away between the status fan-out and the CAS reservation). Drop the
		// group and fall back to a single control plane rather than failing the
		// whole create.
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

	// All-or-nothing: any launch failure rolls back every VM that did come up,
	// releases the reservation, drops the group, and fails the create.
	if len(launchErrs) > 0 {
		slog.Error("placeControlPlane: partial spread launch, rolling back",
			"cluster", clusterName, "launched", len(launched), "failed", len(launchErrs))
		s.rollbackControlPlaneSpread(accountID, groupName, pgAccount, launched, reserved)
		return nil, "", fmt.Errorf("eks: HA control-plane launch failed: %w", errors.Join(launchErrs...))
	}

	// Confirm each VM actually landed on its reserved host (acceptance: verified
	// via the node-VM fan-out). The node-targeted launch subject already pins
	// placement, so an instance not yet visible to the fan-out is tolerated;
	// only a definitive wrong-host placement triggers rollback.
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

// launchSingleControlPlane launches one control-plane VM on the local node, the
// pre-HA behaviour. NodeID is empty: the local launch is node-blind.
func (s *EKSServiceImpl) launchSingleControlPlane(tmpl K3sServerInput) ([]ControlPlaneNode, string, error) {
	in := tmpl
	in.TargetNodeID = ""
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

// launchControlPlaneSpread launches one server VM per reserved node in parallel
// (each remote launch blocks on a full-AMI boot round trip, so sequential would
// be N×). Results stay index-aligned with nodes.
func (s *EKSServiceImpl) launchControlPlaneSpread(tmpl K3sServerInput, nodes []string) []cpLaunchResult {
	results := make([]cpLaunchResult, len(nodes))
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(idx int, nodeID string) {
			defer wg.Done()
			in := tmpl
			in.TargetNodeID = nodeID
			out, err := LaunchK3sServerVM(s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
			results[idx] = cpLaunchResult{node: nodeID, out: out, err: err}
		}(i, node)
	}
	wg.Wait()
	return results
}

// verifyControlPlaneSpread confirms every launched VM sits on its reserved host
// and that no two share a host. A VM not yet visible to the fan-out is tolerated
// (listing lag — the node-targeted launch subject already guarantees the host).
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

// rollbackControlPlaneSpread tears down a partial HA placement: terminate every
// VM that launched, release the reservation (clears the empty placeholders —
// the record was never finalized on any rollback path), and drop the group.
// Best-effort: a stranded internal placement group is harmless and re-creatable.
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

// ensureSpreadGroup creates the per-cluster spread placement group. A duplicate
// (a prior attempt left it behind) is reused — ReserveSpreadNodes filters
// already-occupied hosts, so reserving against a stale group is safe.
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

// teardownSpreadGroup removes the cluster's CP instances from the spread group
// and deletes it. Best-effort: a leaked internal group strands nothing billable.
// Runs only for HA clusters (ControlPlaneSpreadGroup set).
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

// controlPlaneTeardownNodes returns the control-plane VMs DeleteCluster must
// tear down. It prefers the multi-node list; for clusters persisted before that
// field existed it synthesizes a single entry from the scalar fields.
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

// haSpreadGroupName namespaces the spread group by account + cluster. The group
// lives under the system account (admin.SystemAccountID) so it never surfaces in
// a customer's DescribePlacementGroups; the account therefore cannot
// disambiguate same-named clusters across tenants, so it is folded into the name.
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

// hostFanoutTimeout bounds a node status / node VMs fan-out. Sized for a LAN
// round trip to every daemon; CreateCluster is already multi-second so a fixed
// short collection window trades a little latency for reliably seeing every host
// (a missed host would wrongly drop the cluster to single-CP).
const hostFanoutTimeout = time.Second

type natsHostScheduler struct {
	nc *nats.Conn
}

// NewNATSHostScheduler builds the NATS fan-out HostScheduler the daemon wires
// into EKSServiceDeps.
func NewNATSHostScheduler(nc *nats.Conn) HostScheduler {
	return &natsHostScheduler{nc: nc}
}

func (h *natsHostScheduler) SchedulableHosts(instanceType string) []string {
	// The control-plane VM is a system type (sys.*), which node.status omits from
	// its per-type capacity list (system types are hidden from customers). A
	// system VM still consumes host vCPU/memory like any guest, so size the fit
	// against each node's raw schedulable headroom and the type's footprint.
	vcpu, memGB, ok := instancetypes.SpecForSystemType(instanceType)
	if !ok {
		slog.Warn("SchedulableHosts: unknown system instance type, no schedulable hosts",
			"instanceType", instanceType)
		return nil
	}

	var hosts []string
	seen := make(map[string]bool)
	h.fanout("spinifex.node.status", func(data []byte) {
		var st types.NodeStatusResponse
		if json.Unmarshal(data, &st) != nil || st.Node == "" || seen[st.Node] {
			return
		}
		if nodeFitsSystemInstance(st, vcpu, memGB) {
			seen[st.Node] = true
			hosts = append(hosts, st.Node)
		}
	})
	return hosts
}

// nodeFitsSystemInstance reports whether a node's schedulable headroom
// (Total - Reserved - Alloc, the same arithmetic node.status documents for guest
// scheduling) admits at least one VM of the given vCPU/memory footprint.
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

// fanout publishes an empty request on subject and invokes handle for every
// reply that arrives within hostFanoutTimeout.
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
