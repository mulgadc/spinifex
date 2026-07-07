package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveNodeStatus replies to spinifex.node.status fan-outs with the given node
// snapshots so the NATS host scheduler can be exercised end-to-end.
func serveNodeStatus(t *testing.T, nc *nats.Conn, nodes []types.NodeStatusResponse) {
	t.Helper()
	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, n := range nodes {
			data, _ := json.Marshal(n)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// TestNATSHostScheduler_SchedulableHosts covers both fit paths: customer types
// fit on advertised per-type Available; system types fit on raw headroom.
func TestNATSHostScheduler_SchedulableHosts(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	serveNodeStatus(t, nc, []types.NodeStatusResponse{
		{
			Node: "nodeA", TotalVCPU: 32, TotalMemGB: 128,
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.medium", Available: 3}},
		},
		{
			Node: "nodeB", TotalVCPU: 0, TotalMemGB: 0,
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.medium", Available: 0}},
		},
	})
	sched := NewNATSHostScheduler(nc)

	// Customer type: only the node advertising free capacity is schedulable.
	require.Equal(t, []string{"nodeA"}, sched.SchedulableHosts(context.Background(), "t3.medium"))

	// System type: fit is decided by raw headroom, so the drained node drops out.
	sysHosts := sched.SchedulableHosts(context.Background(), defaultK3sServerInstanceType)
	sort.Strings(sysHosts)
	require.Equal(t, []string{"nodeA"}, sysHosts)

	// Unknown system type yields no hosts.
	require.Empty(t, sched.SchedulableHosts(context.Background(), "sys.bogus"))
}

// --- HA control-plane orchestrator test doubles ---

// fakeHostScheduler fakes the capacity + placement fan-out. SchedulableHosts
// returns the configured hosts; InstanceHosts maps each instance ID back to its
// node by the "i-<node>" convention seqK3sInst launches with, unless overridden
// by notVisible (listing lag) or wrongHost (placement mismatch).
type fakeHostScheduler struct {
	hosts      []string
	notVisible map[string]bool
	wrongHost  map[string]string
}

var _ HostScheduler = (*fakeHostScheduler)(nil)

func (f *fakeHostScheduler) SchedulableHosts(context.Context, string) []string { return f.hosts }

func (f *fakeHostScheduler) InstanceHosts(_ context.Context, ids []string) map[string]string {
	out := make(map[string]string)
	for _, id := range ids {
		if f.notVisible[id] {
			continue
		}
		if h, ok := f.wrongHost[id]; ok {
			out[id] = h
			continue
		}
		out[id] = strings.TrimPrefix(id, "i-")
	}
	return out
}

// seqK3sVPC issues a distinct ENI per call so a 3-node spread yields 3 ENIs.
type seqK3sVPC struct {
	mu      sync.Mutex
	n       int
	deleted []string
}

var _ k3sVPCProvisioner = (*seqK3sVPC)(nil)

func (v *seqK3sVPC) CreateNetworkInterface(_ context.Context, in *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n++
	return &ec2.CreateNetworkInterfaceOutput{NetworkInterface: &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(fmt.Sprintf("eni-%03d", v.n)),
		PrivateIpAddress:   aws.String(fmt.Sprintf("10.0.1.%d", v.n+10)),
		SubnetId:           in.SubnetId,
	}}, nil
}

func (v *seqK3sVPC) DeleteNetworkInterface(_ context.Context, in *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deleted = append(v.deleted, aws.StringValue(in.NetworkInterfaceId))
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (v *seqK3sVPC) DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	return &ec2.DescribeNetworkInterfacesOutput{}, nil
}

func (v *seqK3sVPC) DetachENI(_ context.Context, _, _ string) error { return nil }

// seqK3sInst returns instance ID "i-<nodeID>" so launches map deterministically
// back to their target host. failNodes makes a node's launch return an error.
type seqK3sInst struct {
	mu         sync.Mutex
	nodes      []string
	terminated []string
	failNodes  map[string]bool
	userData   map[string]string // nodeID -> rendered cloud-init
}

var _ k3sInstanceLauncher = (*seqK3sInst)(nil)

func (i *seqK3sInst) LaunchSystemInstance(in *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	return i.LaunchSystemInstanceOnNode("", in)
}

func (i *seqK3sInst) LaunchSystemInstanceOnNode(nodeID string, in *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failNodes[nodeID] {
		return nil, errors.New("InsufficientInstanceCapacity")
	}
	if i.userData == nil {
		i.userData = map[string]string{}
	}
	i.userData[nodeID] = in.UserData
	i.nodes = append(i.nodes, nodeID)
	id := "i-" + nodeID
	if nodeID == "" {
		id = "i-local"
	}
	return &sysinstance.SystemInstanceOutput{InstanceID: id, MgmtIP: "10.255.0.9"}, nil
}

func (i *seqK3sInst) TerminateSystemInstance(instanceID string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.terminated = append(i.terminated, instanceID)
	return nil
}

// fakePlacer records every spread-placement call and returns configurable
// reservation/finalize outcomes. Reserve defaults to the first MaxCount eligible
// nodes when reserved is unset.
type fakePlacer struct {
	mu sync.Mutex

	createGroups   []string
	deleteGroups   []string
	reserveInputs  []*handlers_ec2_placementgroup.ReserveSpreadNodesInput
	releaseInputs  []*handlers_ec2_placementgroup.ReleaseSpreadNodesInput
	finalizeInputs []*handlers_ec2_placementgroup.FinalizeSpreadInstancesInput
	removeInputs   []*handlers_ec2_placementgroup.RemoveInstanceInput

	reserved    []string
	createErr   error
	reserveErr  error
	finalizeErr error
}

var _ controlPlanePlacer = (*fakePlacer)(nil)

func (p *fakePlacer) CreatePlacementGroup(_ context.Context, in *ec2.CreatePlacementGroupInput, _ string) (*ec2.CreatePlacementGroupOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.createGroups = append(p.createGroups, aws.StringValue(in.GroupName))
	if p.createErr != nil {
		return nil, p.createErr
	}
	return &ec2.CreatePlacementGroupOutput{}, nil
}

func (p *fakePlacer) DeletePlacementGroup(_ context.Context, in *ec2.DeletePlacementGroupInput, _ string) (*ec2.DeletePlacementGroupOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteGroups = append(p.deleteGroups, aws.StringValue(in.GroupName))
	return &ec2.DeletePlacementGroupOutput{}, nil
}

func (p *fakePlacer) ReserveSpreadNodes(_ context.Context, in *handlers_ec2_placementgroup.ReserveSpreadNodesInput, _ string) (*handlers_ec2_placementgroup.ReserveSpreadNodesOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserveInputs = append(p.reserveInputs, in)
	if p.reserveErr != nil {
		return nil, p.reserveErr
	}
	reserved := p.reserved
	if reserved == nil {
		n := min(in.MaxCount, len(in.EligibleNodes))
		reserved = append([]string(nil), in.EligibleNodes[:n]...)
	}
	return &handlers_ec2_placementgroup.ReserveSpreadNodesOutput{ReservedNodes: reserved}, nil
}

func (p *fakePlacer) ReleaseSpreadNodes(_ context.Context, in *handlers_ec2_placementgroup.ReleaseSpreadNodesInput, _ string) (*handlers_ec2_placementgroup.ReleaseSpreadNodesOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releaseInputs = append(p.releaseInputs, in)
	return &handlers_ec2_placementgroup.ReleaseSpreadNodesOutput{}, nil
}

func (p *fakePlacer) FinalizeSpreadInstances(_ context.Context, in *handlers_ec2_placementgroup.FinalizeSpreadInstancesInput, _ string) (*handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finalizeInputs = append(p.finalizeInputs, in)
	if p.finalizeErr != nil {
		return nil, p.finalizeErr
	}
	return &handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput{}, nil
}

func (p *fakePlacer) RemoveInstance(_ context.Context, in *handlers_ec2_placementgroup.RemoveInstanceInput, _ string) (*handlers_ec2_placementgroup.RemoveInstanceOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeInputs = append(p.removeInputs, in)
	return &handlers_ec2_placementgroup.RemoveInstanceOutput{}, nil
}

// seqK3sAMI resolves the eks-server AMI; concurrency-safe so the parallel
// spread launches can each call DescribeImages without racing.
type seqK3sAMI struct {
	mu    sync.Mutex
	inner fakeK3sAMI
}

var _ k3sAMIResolver = (*seqK3sAMI)(nil)

func (a *seqK3sAMI) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inner.DescribeImages(context.Background(), in, accountID)
}

func newPlacerService(sched HostScheduler, placer controlPlanePlacer, vpc k3sVPCProvisioner, inst k3sInstanceLauncher) *EKSServiceImpl {
	return &EKSServiceImpl{deps: EKSServiceDeps{
		VPCK3s:         vpc,
		Instance:       inst,
		Image:          &seqK3sAMI{},
		Scheduler:      sched,
		PlacementGroup: placer,
	}}
}

func nodeIDs(nodes []ControlPlaneNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.NodeID)
	}
	return out
}

const testHAAccountID = "111122223333"

func TestPlaceControlPlane_SpreadHappyPath(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c", "node-d"}}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	vpc := &seqK3sVPC{}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, vpc, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.NoError(t, err)
	assert.Equal(t, "eks-cp-"+testHAAccountID+"-alpha", group)
	require.Len(t, nodes, 3)

	assert.ElementsMatch(t, []string{"node-a", "node-b", "node-c"}, nodeIDs(nodes))
	enis := map[string]bool{}
	for _, n := range nodes {
		assert.Equal(t, "i-"+n.NodeID, n.InstanceID)
		assert.NotEmpty(t, n.ENIID)
		enis[n.ENIID] = true
	}
	assert.Len(t, enis, 3, "each spread VM gets a distinct ENI")

	assert.Equal(t, []string{group}, placer.createGroups)
	require.Len(t, placer.reserveInputs, 1)
	assert.Equal(t, haControlPlaneCount, placer.reserveInputs[0].MinCount)
	assert.Equal(t, haControlPlaneCount, placer.reserveInputs[0].MaxCount)
	require.Len(t, placer.finalizeInputs, 1)
	assert.Len(t, placer.finalizeInputs[0].NodeInstances, 3)
	assert.Empty(t, placer.releaseInputs)
	assert.Empty(t, placer.deleteGroups)

	assert.ElementsMatch(t, []string{"node-a", "node-b", "node-c"}, inst.nodes)
	assert.Empty(t, inst.terminated)
}

func TestPlaceControlPlane_SpreadFirstInitsRestJoin(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	vpc := &seqK3sVPC{}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, vpc, inst)

	tmpl := validK3sInput()
	tmpl.JoinToken = "sharedtok123"
	_, _, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", tmpl)
	require.NoError(t, err)

	// node-a is reserved[0] → the first server: cluster-inits, no join URL.
	first := inst.userData["node-a"]
	assert.Contains(t, first, "cluster-init: true")
	assert.NotContains(t, first, "server: https://")
	assert.Contains(t, first, "SPINIFEX_K3S_ROLE=server\n")
	assert.Contains(t, first, "token: sharedtok123")

	// node-b/node-c join the first server's ENI IP (sequential first launch ⇒
	// the first ENI, 10.0.1.11) with the shared token, without cluster-init.
	for _, n := range []string{"node-b", "node-c"} {
		join := inst.userData[n]
		assert.Contains(t, join, "server: https://10.0.1.11:6443", n)
		assert.Contains(t, join, "token: sharedtok123", n)
		assert.NotContains(t, join, "cluster-init: true", n)
		assert.Contains(t, join, "SPINIFEX_K3S_ROLE=server-join", n)
	}
}

func TestPlaceControlPlane_FirstServerFailureRollsBackNoJoins(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	inst := &seqK3sInst{failNodes: map[string]bool{"node-a": true}}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Equal(t, "", group)

	// The first server never came up, so no join servers were launched and there
	// is nothing to terminate; the reservation + group are still cleaned up.
	assert.Empty(t, inst.terminated)
	assert.NotContains(t, inst.nodes, "node-b")
	assert.NotContains(t, inst.nodes, "node-c")
	require.Len(t, placer.releaseInputs, 1)
	assert.Len(t, placer.deleteGroups, 1)
	assert.Empty(t, placer.finalizeInputs)
}

func TestPlaceControlPlane_FallbackUnderThreeHosts(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b"}}
	placer := &fakePlacer{}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.NoError(t, err)
	assert.Equal(t, "", group)
	require.Len(t, nodes, 1)
	assert.Equal(t, "", nodes[0].NodeID)
	assert.Equal(t, "i-local", nodes[0].InstanceID)

	assert.Empty(t, placer.createGroups)
	assert.Empty(t, placer.reserveInputs)
	assert.Equal(t, []string{""}, inst.nodes)
}

func TestPlaceControlPlane_BoundaryExactlyThree(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.NoError(t, err)
	assert.Equal(t, "eks-cp-"+testHAAccountID+"-alpha", group)
	require.Len(t, nodes, 3)
	require.Len(t, placer.finalizeInputs, 1)
	assert.ElementsMatch(t, []string{"node-a", "node-b", "node-c"}, inst.nodes)
}

func TestPlaceControlPlane_PartialLaunchRollsBack(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	inst := &seqK3sInst{failNodes: map[string]bool{"node-c": true}}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Equal(t, "", group)

	assert.Len(t, inst.terminated, 2, "the two VMs that launched are terminated")
	require.Len(t, placer.releaseInputs, 1)
	assert.ElementsMatch(t, []string{"node-a", "node-b", "node-c"}, placer.releaseInputs[0].Nodes)
	assert.Empty(t, placer.finalizeInputs)
	assert.Len(t, placer.deleteGroups, 1)
}

func TestPlaceControlPlane_ReserveFailureFallsBackToSingle(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{reserveErr: errors.New(awserrors.ErrorInsufficientInstanceCapacity)}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.NoError(t, err)
	assert.Equal(t, "", group)
	require.Len(t, nodes, 1)
	assert.Equal(t, "", nodes[0].NodeID)

	assert.Len(t, placer.createGroups, 1)
	require.Len(t, placer.reserveInputs, 1)
	assert.Len(t, placer.deleteGroups, 1, "the reserved-but-empty group is dropped")
	assert.Equal(t, []string{""}, inst.nodes)
}

func TestPlaceControlPlane_VerifyMismatchRollsBack(t *testing.T) {
	sched := &fakeHostScheduler{
		hosts:     []string{"node-a", "node-b", "node-c"},
		wrongHost: map[string]string{"i-node-c": "node-x"},
	}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Equal(t, "", group)

	assert.Len(t, inst.terminated, 3)
	require.Len(t, placer.releaseInputs, 1)
	assert.Empty(t, placer.finalizeInputs)
	assert.Len(t, placer.deleteGroups, 1)
}

func TestPlaceControlPlane_VerifyToleratesNotYetVisible(t *testing.T) {
	sched := &fakeHostScheduler{
		hosts:      []string{"node-a", "node-b", "node-c"},
		notVisible: map[string]bool{"i-node-b": true},
	}
	placer := &fakePlacer{reserved: []string{"node-a", "node-b", "node-c"}}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.NoError(t, err)
	assert.Equal(t, "eks-cp-"+testHAAccountID+"-alpha", group)
	require.Len(t, nodes, 3)
	require.Len(t, placer.finalizeInputs, 1)
	assert.Empty(t, placer.releaseInputs)
	assert.Empty(t, inst.terminated)
}

func TestPlaceControlPlane_FinalizeFailureRollsBack(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	placer := &fakePlacer{
		reserved:    []string{"node-a", "node-b", "node-c"},
		finalizeErr: errors.New("kv write failed"),
	}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, placer, &seqK3sVPC{}, inst)

	nodes, group, err := svc.placeControlPlane(context.Background(), testHAAccountID, "alpha", validK3sInput())
	require.Error(t, err)
	assert.Nil(t, nodes)
	assert.Equal(t, "", group)

	assert.Len(t, inst.terminated, 3)
	require.Len(t, placer.finalizeInputs, 1)
	require.Len(t, placer.releaseInputs, 1)
	assert.Len(t, placer.deleteGroups, 1)
}

func TestControlPlaneTeardownNodes(t *testing.T) {
	multi := &ClusterMeta{ControlPlaneNodes: []ControlPlaneNode{
		{NodeID: "n1", InstanceID: "i-1", ENIID: "eni-1"},
		{NodeID: "n2", InstanceID: "i-2", ENIID: "eni-2"},
	}}
	assert.Len(t, controlPlaneTeardownNodes(multi), 2)

	scalar := &ClusterMeta{
		ControlPlaneInstanceID: "i-old",
		ControlPlaneENIID:      "eni-old",
		ControlPlaneENIIP:      "10.0.0.5",
		ControlPlaneMgmtIP:     "10.255.0.5",
	}
	got := controlPlaneTeardownNodes(scalar)
	require.Len(t, got, 1)
	assert.Equal(t, "i-old", got[0].InstanceID)
	assert.Equal(t, "eni-old", got[0].ENIID)
	assert.Equal(t, "10.0.0.5", got[0].ENIIP)

	assert.Nil(t, controlPlaneTeardownNodes(&ClusterMeta{}))
}

func TestTeardownSpreadGroup(t *testing.T) {
	placer := &fakePlacer{}
	svc := &EKSServiceImpl{deps: EKSServiceDeps{PlacementGroup: placer}}
	meta := &ClusterMeta{
		ControlPlaneSpreadGroup: "eks-cp-" + testHAAccountID + "-alpha",
		ControlPlaneNodes: []ControlPlaneNode{
			{NodeID: "n1", InstanceID: "i-1"},
			{NodeID: "n2", InstanceID: "i-2"},
			{NodeID: "n3", InstanceID: "i-3"},
		},
	}
	svc.teardownSpreadGroup(context.Background(), meta)
	assert.Len(t, placer.removeInputs, 3)
	assert.Equal(t, []string{meta.ControlPlaneSpreadGroup}, placer.deleteGroups)

	noGroup := &fakePlacer{}
	svc2 := &EKSServiceImpl{deps: EKSServiceDeps{PlacementGroup: noGroup}}
	svc2.teardownSpreadGroup(context.Background(), &ClusterMeta{})
	assert.Empty(t, noGroup.removeInputs)
	assert.Empty(t, noGroup.deleteGroups)
}

func TestNodeFitsSystemInstance(t *testing.T) {
	// sys.medium = 2 vCPU / 4 GB.
	vcpu, memGB, ok := instancetypes.SpecForSystemType("sys.medium")
	require.True(t, ok)
	require.Equal(t, 2, vcpu)
	require.Equal(t, 4.0, memGB)

	tests := []struct {
		name string
		st   types.NodeStatusResponse
		want bool
	}{
		{
			name: "ample headroom",
			st:   types.NodeStatusResponse{TotalVCPU: 16, TotalMemGB: 64},
			want: true,
		},
		{
			name: "exact fit after reserve+alloc",
			st:   types.NodeStatusResponse{TotalVCPU: 8, ReservedVCPU: 2, AllocVCPU: 4, TotalMemGB: 12, ReservedMemGB: 4, AllocMemGB: 4},
			want: true, // remain 2 vCPU / 4 GB == footprint
		},
		{
			name: "vcpu exhausted",
			st:   types.NodeStatusResponse{TotalVCPU: 8, AllocVCPU: 7, TotalMemGB: 64},
			want: false, // remain 1 vCPU < 2
		},
		{
			name: "memory exhausted",
			st:   types.NodeStatusResponse{TotalVCPU: 16, TotalMemGB: 8, AllocMemGB: 5},
			want: false, // remain 3 GB < 4
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nodeFitsSystemInstance(tt.st, vcpu, memGB))
		})
	}
}

func TestSpecForSystemType_RejectsNonSystem(t *testing.T) {
	_, _, ok := instancetypes.SpecForSystemType("t3.medium")
	assert.False(t, ok, "customer types are not system types")
	_, _, ok = instancetypes.SpecForSystemType("sys.nonexistent")
	assert.False(t, ok, "unknown system size has no spec")
}
