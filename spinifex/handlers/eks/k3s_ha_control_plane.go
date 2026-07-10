package handlers_eks

import (
	"context"
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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// haControlPlaneCount is the HA spread width (odd quorum tolerating one host loss).
// Fewer schedulable hosts fall back to a single CP VM.
const haControlPlaneCount = 3

// controlPlanePlacer is the spread-placement surface for HA CP orchestration.
type controlPlanePlacer interface {
	CreatePlacementGroup(context.Context, *ec2.CreatePlacementGroupInput, string) (*ec2.CreatePlacementGroupOutput, error)
	DeletePlacementGroup(context.Context, *ec2.DeletePlacementGroupInput, string) (*ec2.DeletePlacementGroupOutput, error)
	ReserveSpreadNodes(context.Context, *handlers_ec2_placementgroup.ReserveSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReserveSpreadNodesOutput, error)
	ReleaseSpreadNodes(context.Context, *handlers_ec2_placementgroup.ReleaseSpreadNodesInput, string) (*handlers_ec2_placementgroup.ReleaseSpreadNodesOutput, error)
	FinalizeSpreadInstances(context.Context, *handlers_ec2_placementgroup.FinalizeSpreadInstancesInput, string) (*handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput, error)
	RemoveInstance(context.Context, *handlers_ec2_placementgroup.RemoveInstanceInput, string) (*handlers_ec2_placementgroup.RemoveInstanceOutput, error)
}

var _ controlPlanePlacer = (handlers_ec2_placementgroup.PlacementGroupService)(nil)

// HostScheduler answers capacity + placement fan-out questions for HA CP placement.
type HostScheduler interface {
	// SchedulableHosts returns node IDs that can fit at least one VM of instanceType.
	SchedulableHosts(ctx context.Context, instanceType string) []string
	// InstanceHosts maps each instance ID to its hosting node; absent entries are not yet visible.
	InstanceHosts(ctx context.Context, instanceIDs []string) map[string]string
}

// placeControlPlane places the cluster's CP VMs. With ≥ haControlPlaneCount
// schedulable hosts it spreads servers all-or-nothing; otherwise falls back to
// a single CP. Returns placed nodes ([0] = primary) and spread group name ("" for single-CP).
func (s *EKSServiceImpl) placeControlPlane(ctx context.Context, accountID, clusterName string, tmpl K3sServerInput) ([]ControlPlaneNode, string, error) {
	instanceType := tmpl.InstanceType
	if instanceType == "" {
		instanceType = defaultK3sServerInstanceType
	}

	hosts := s.deps.Scheduler.SchedulableHosts(ctx, instanceType)
	if len(hosts) < haControlPlaneCount {
		slog.InfoContext(ctx, "placeControlPlane: insufficient hosts for HA spread, launching single control plane",
			"cluster", clusterName, "schedulableHosts", len(hosts), "want", haControlPlaneCount)
		return s.launchSingleControlPlane(ctx, tmpl)
	}

	pgAccount := admin.SystemAccountID()
	groupName := haSpreadGroupName(accountID, clusterName)
	if err := s.ensureSpreadGroup(ctx, groupName, pgAccount); err != nil {
		return nil, "", err
	}

	reserve, err := s.deps.PlacementGroup.ReserveSpreadNodes(ctx, &handlers_ec2_placementgroup.ReserveSpreadNodesInput{
		GroupName:     groupName,
		EligibleNodes: hosts,
		MinCount:      haControlPlaneCount,
		MaxCount:      haControlPlaneCount,
	}, pgAccount)
	if err != nil {
		// Capacity raced away between fan-out and reservation; fall back to single CP.
		slog.WarnContext(ctx, "placeControlPlane: spread reservation failed, falling back to single control plane",
			"cluster", clusterName, "group", groupName, "err", err)
		s.deleteSpreadGroup(ctx, groupName, pgAccount)
		return s.launchSingleControlPlane(ctx, tmpl)
	}
	reserved := reserve.ReservedNodes

	results := s.launchControlPlaneSpread(ctx, tmpl, reserved)

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
		slog.ErrorContext(ctx, "placeControlPlane: partial spread launch, rolling back",
			"cluster", clusterName, "launched", len(launched), "failed", len(launchErrs))
		s.rollbackControlPlaneSpread(ctx, accountID, groupName, pgAccount, launched, reserved)
		return nil, "", fmt.Errorf("eks: HA control-plane launch failed: %w", errors.Join(launchErrs...))
	}

	// Verify each VM landed on its reserved host. Not-yet-visible instances are
	// tolerated; only a definitive wrong-host placement triggers rollback.
	if err := s.verifyControlPlaneSpread(ctx, launched); err != nil {
		slog.ErrorContext(ctx, "placeControlPlane: placement verification failed, rolling back",
			"cluster", clusterName, "err", err)
		s.rollbackControlPlaneSpread(ctx, accountID, groupName, pgAccount, launched, reserved)
		return nil, "", err
	}

	nodeInstances := make(map[string][]string, len(launched))
	for _, n := range launched {
		nodeInstances[n.NodeID] = []string{n.InstanceID}
	}
	if _, err := s.deps.PlacementGroup.FinalizeSpreadInstances(ctx, &handlers_ec2_placementgroup.FinalizeSpreadInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, pgAccount); err != nil {
		slog.ErrorContext(ctx, "placeControlPlane: finalize failed, rolling back", "cluster", clusterName, "err", err)
		s.rollbackControlPlaneSpread(ctx, accountID, groupName, pgAccount, launched, reserved)
		return nil, "", fmt.Errorf("eks: finalize HA control-plane placement: %w", err)
	}

	slog.InfoContext(ctx, "placeControlPlane: HA control plane placed",
		"cluster", clusterName, "group", groupName, "nodes", reserved)
	return launched, groupName, nil
}

var _ CPProvisioner = (*EKSServiceImpl)(nil)

// ProvisionReplacementCP launches one replacement control-plane VM that joins the
// surviving etcd quorum at req.JoinURL, placed on a schedulable host not already
// holding a live member so spread is preserved. It replays the create-time
// template (rotating creds re-derived) — the single-node analogue of the spread
// join branch. The caller (reconciler) swaps the returned node into meta; NLB
// per-node registration and spread-group accounting stay with the [0] mirror
// today, matching the create path.
func (s *EKSServiceImpl) ProvisionReplacementCP(ctx context.Context, req ReplacementCPRequest) (ControlPlaneNode, error) {
	if req.Template == nil {
		return ControlPlaneNode{}, errors.New("eks: ProvisionReplacementCP nil template")
	}
	if req.JoinURL == "" {
		return ControlPlaneNode{}, errors.New("eks: ProvisionReplacementCP empty join URL")
	}

	in := *req.Template
	in.ServerURL = req.JoinURL
	in.KonnServerCount = req.MemberCount
	in.PrunePeerIP = req.DeadPeerIP

	// Re-derive rotating creds the same way CreateCluster does so a replacement
	// picks up current credentials rather than a frozen create-time snapshot.
	sysAcct := admin.SystemAccountID()
	in.IamInstanceProfileArn = ""
	in.AccessKey = ""
	in.SecretKey = ""
	if profileARN := s.ensureCPInstanceProfile(sysAcct); profileARN != "" {
		in.IamInstanceProfileArn = profileARN
	} else {
		in.AccessKey = s.deps.SystemAccessKey
		in.SecretKey = s.deps.SystemSecretKey
	}
	in.PredastoreAccessKey = s.deps.SystemAccessKey
	in.PredastoreSecretKey = s.deps.SystemSecretKey

	instanceType := in.InstanceType
	if instanceType == "" {
		instanceType = defaultK3sServerInstanceType
	}
	target, err := s.pickReplacementHost(ctx, instanceType, req.ExcludeHosts)
	if err != nil {
		return ControlPlaneNode{}, err
	}
	in.TargetNodeID = target

	out, err := LaunchK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
	if err != nil {
		return ControlPlaneNode{}, err
	}
	slog.Info("ProvisionReplacementCP: replacement control plane launched",
		"cluster", req.ClusterName, "instanceId", out.InstanceID, "host", target, "joinURL", req.JoinURL)
	return controlPlaneNode(target, out), nil
}

// FreshCPRequest carries everything ProvisionFreshControlPlane needs to launch a
// brand-new single-node control plane from a persisted template — the no-survivor
// case, where there is no live quorum member to join.
type FreshCPRequest struct {
	AccountID    string
	ClusterName  string
	Template     *K3sServerInput
	ExcludeHosts []string
}

// ProvisionFreshControlPlane launches a single control-plane VM as a fresh
// cluster-init seed (ServerURL empty), replaying the create-time template with
// rotating creds re-derived. It is the single-node analogue of
// ProvisionReplacementCP for the case where the entire control plane (VM +
// volume) is gone and there is no surviving etcd quorum to join — the caller
// (restore-snapshot) drives the etcd restore via a RecoveryDirective set after
// launch, not via any state this function knows about.
func (s *EKSServiceImpl) ProvisionFreshControlPlane(ctx context.Context, req FreshCPRequest) (ControlPlaneNode, error) {
	if req.Template == nil {
		return ControlPlaneNode{}, errors.New("eks: ProvisionFreshControlPlane nil template")
	}

	in := *req.Template
	in.ServerURL = ""
	in.KonnServerCount = 1
	in.PrunePeerIP = ""

	sysAcct := admin.SystemAccountID()
	in.IamInstanceProfileArn = ""
	in.AccessKey = ""
	in.SecretKey = ""
	if profileARN := s.ensureCPInstanceProfile(sysAcct); profileARN != "" {
		in.IamInstanceProfileArn = profileARN
	} else {
		in.AccessKey = s.deps.SystemAccessKey
		in.SecretKey = s.deps.SystemSecretKey
	}
	in.PredastoreAccessKey = s.deps.SystemAccessKey
	in.PredastoreSecretKey = s.deps.SystemSecretKey

	instanceType := in.InstanceType
	if instanceType == "" {
		instanceType = defaultK3sServerInstanceType
	}
	target, err := s.pickReplacementHost(ctx, instanceType, req.ExcludeHosts)
	if err != nil {
		return ControlPlaneNode{}, err
	}
	in.TargetNodeID = target

	out, err := LaunchK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
	if err != nil {
		return ControlPlaneNode{}, err
	}
	slog.Info("ProvisionFreshControlPlane: fresh control plane launched",
		"cluster", req.ClusterName, "instanceId", out.InstanceID, "host", target)
	return controlPlaneNode(target, out), nil
}

// pickReplacementHost returns a schedulable host for the given instance type that
// is not already holding a live control-plane member, preserving one-member-per-host
// spread. Errors when every schedulable host is excluded (host permanently down),
// so the reconciler surfaces a clear degraded reason rather than looping.
func (s *EKSServiceImpl) pickReplacementHost(ctx context.Context, instanceType string, exclude []string) (string, error) {
	ex := make(map[string]bool, len(exclude))
	for _, h := range exclude {
		ex[h] = true
	}
	for _, h := range s.deps.Scheduler.SchedulableHosts(ctx, instanceType) {
		if !ex[h] {
			return h, nil
		}
	}
	return "", fmt.Errorf("eks: no schedulable host for replacement control plane (excluding %d live-member host(s))", len(exclude))
}

// launchSingleControlPlane launches one CP VM on the local node (pre-HA fallback).
func (s *EKSServiceImpl) launchSingleControlPlane(ctx context.Context, tmpl K3sServerInput) ([]ControlPlaneNode, string, error) {
	in := tmpl
	in.TargetNodeID = ""
	in.KonnServerCount = 1
	out, err := LaunchK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
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
func (s *EKSServiceImpl) launchControlPlaneSpread(ctx context.Context, tmpl K3sServerInput, nodes []string) []cpLaunchResult {
	results := make([]cpLaunchResult, len(nodes))

	tmpl.KonnServerCount = len(nodes)
	first := tmpl
	first.TargetNodeID = nodes[0]
	out, err := LaunchK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, s.deps.Image, first)
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
			o, e := LaunchK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, s.deps.Image, in)
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
func (s *EKSServiceImpl) verifyControlPlaneSpread(ctx context.Context, nodes []ControlPlaneNode) error {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.InstanceID)
	}
	hosts := s.deps.Scheduler.InstanceHosts(ctx, ids)

	seen := make(map[string]string, len(nodes)) // host -> instanceID
	for _, n := range nodes {
		actual, ok := hosts[n.InstanceID]
		if !ok {
			slog.WarnContext(ctx, "verifyControlPlaneSpread: instance not yet visible in node fan-out",
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
func (s *EKSServiceImpl) rollbackControlPlaneSpread(ctx context.Context, accountID, groupName, pgAccount string, launched []ControlPlaneNode, reserved []string) {
	for _, n := range launched {
		if err := TerminateK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, accountID, n.InstanceID, n.ENIID); err != nil {
			slog.WarnContext(ctx, "rollbackControlPlaneSpread: terminate failed", "instanceId", n.InstanceID, "err", err)
		}
	}
	if _, err := s.deps.PlacementGroup.ReleaseSpreadNodes(ctx, &handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
		GroupName: groupName,
		Nodes:     reserved,
	}, pgAccount); err != nil {
		slog.WarnContext(ctx, "rollbackControlPlaneSpread: release nodes failed", "group", groupName, "err", err)
	}
	s.deleteSpreadGroup(ctx, groupName, pgAccount)
}

// ensureSpreadGroup creates the spread placement group, reusing it if it already exists.
func (s *EKSServiceImpl) ensureSpreadGroup(ctx context.Context, groupName, pgAccount string) error {
	_, err := s.deps.PlacementGroup.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(groupName),
		Strategy:  aws.String(ec2.PlacementStrategySpread),
	}, pgAccount)
	if err == nil {
		return nil
	}
	if awserrors.IsErrorCode(err, awserrors.ErrorInvalidPlacementGroupDuplicate) {
		slog.InfoContext(ctx, "ensureSpreadGroup: spread group exists, reusing", "group", groupName)
		return nil
	}
	return fmt.Errorf("eks: create spread placement group %s: %w", groupName, err)
}

func (s *EKSServiceImpl) deleteSpreadGroup(ctx context.Context, groupName, pgAccount string) {
	if _, err := s.deps.PlacementGroup.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
		GroupName: aws.String(groupName),
	}, pgAccount); err != nil {
		slog.WarnContext(ctx, "deleteSpreadGroup: delete failed", "group", groupName, "err", err)
	}
}

// teardownSpreadGroup removes CP instances from the spread group and deletes it.
// No-op for single-CP clusters (ControlPlaneSpreadGroup == "").
func (s *EKSServiceImpl) teardownSpreadGroup(ctx context.Context, meta *ClusterMeta) {
	if meta.ControlPlaneSpreadGroup == "" {
		return
	}
	pgAccount := admin.SystemAccountID()
	for _, cp := range meta.ControlPlaneNodes {
		if cp.NodeID == "" || cp.InstanceID == "" {
			continue
		}
		if _, err := s.deps.PlacementGroup.RemoveInstance(ctx, &handlers_ec2_placementgroup.RemoveInstanceInput{
			GroupName:  meta.ControlPlaneSpreadGroup,
			NodeName:   cp.NodeID,
			InstanceID: cp.InstanceID,
		}, pgAccount); err != nil {
			slog.WarnContext(ctx, "teardownSpreadGroup: remove instance failed",
				"group", meta.ControlPlaneSpreadGroup, "instanceId", cp.InstanceID, "err", err)
		}
	}
	s.deleteSpreadGroup(ctx, meta.ControlPlaneSpreadGroup, pgAccount)
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

func (h *natsHostScheduler) SchedulableHosts(ctx context.Context, instanceType string) []string {
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
			slog.WarnContext(ctx, "SchedulableHosts: unknown system instance type, no schedulable hosts",
				"instanceType", instanceType)
			return nil
		}
	}

	var hosts []string
	seen := make(map[string]bool)
	h.fanout(ctx, "spinifex.node.status", func(data []byte) {
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

func (h *natsHostScheduler) InstanceHosts(ctx context.Context, instanceIDs []string) map[string]string {
	want := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		want[id] = true
	}
	out := make(map[string]string)
	h.fanout(ctx, "spinifex.node.vms", func(data []byte) {
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
func (h *natsHostScheduler) fanout(ctx context.Context, subject string, handle func([]byte)) {
	if h.nc == nil {
		return
	}
	inbox := nats.NewInbox()
	sub, err := h.nc.SubscribeSync(inbox)
	if err != nil {
		slog.WarnContext(ctx, "hostScheduler: subscribe inbox failed", "subject", subject, "err", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	msg := nats.NewMsg(subject)
	msg.Reply = inbox
	msg.Data = []byte("{}")
	utils.InjectTraceContext(ctx, msg.Header)
	if err := h.nc.PublishMsg(msg); err != nil {
		slog.WarnContext(ctx, "hostScheduler: publish failed", "subject", subject, "err", err)
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
