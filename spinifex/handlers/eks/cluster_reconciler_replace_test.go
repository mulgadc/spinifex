package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapCPControl is a CPInstanceControl whose per-instance state (and optional
// describe error) is configured by instance ID, so member-count reconcile can be
// driven with a mix of live, restartable, and lost members.
type mapCPControl struct {
	states     map[string]string
	errs       map[string]error
	startCalls int
}

func (m *mapCPControl) InstanceState(_ context.Context, id string) (string, error) {
	if m.errs != nil {
		if e, ok := m.errs[id]; ok {
			return "", e
		}
	}
	if s, ok := m.states[id]; ok {
		return s, nil
	}
	return "", fmt.Errorf("instance %s not found", id)
}

func (m *mapCPControl) StartInstance(_ context.Context, _ string) error {
	m.startCalls++
	return nil
}

// fakeCPProvisioner records replacement requests and returns a configurable node/error.
type fakeCPProvisioner struct {
	node    ControlPlaneNode
	err     error
	calls   int
	lastReq ReplacementCPRequest
}

func (f *fakeCPProvisioner) ProvisionReplacementCP(_ context.Context, req ReplacementCPRequest) (ControlPlaneNode, error) {
	f.calls++
	f.lastReq = req
	return f.node, f.err
}

// threeMemberCP is the canonical HA CP: three members on distinct hosts, each
// with a join-able ENI IP.
func threeMemberCP() []ControlPlaneNode {
	return []ControlPlaneNode{
		{NodeID: "node-a", InstanceID: "i-cp0", ENIID: "eni-0", ENIIP: "10.0.0.10"},
		{NodeID: "node-b", InstanceID: "i-cp1", ENIID: "eni-1", ENIIP: "10.0.0.11"},
		{NodeID: "node-c", InstanceID: "i-cp2", ENIID: "eni-2", ENIIP: "10.0.0.12"},
	}
}

// haMeta builds an ACTIVE HA cluster meta with a persisted launch template.
func haMeta(nodes []ControlPlaneNode) *ClusterMeta {
	return &ClusterMeta{
		Name:                    "alpha",
		Status:                  ClusterStatusActive,
		ControlPlaneInstanceID:  nodes[0].InstanceID,
		ControlPlaneENIID:       nodes[0].ENIID,
		ControlPlaneENIIP:       nodes[0].ENIIP,
		ControlPlaneNodes:       nodes,
		ControlPlaneSpreadGroup: "eks-cp-acct-alpha",
		ControlPlaneTemplate:    &K3sServerInput{ClusterName: "alpha", InstanceType: "sys.medium"},
	}
}

// newReplaceReconciler wires a reconciler with the given CP control + provisioner
// and stores meta in the account KV so SwapControlPlaneMember can CAS it.
func newReplaceReconciler(t *testing.T, cp CPInstanceControl, prov CPProvisioner, meta *ClusterMeta) (*ClusterReconciler, nats.KeyValue) {
	t.Helper()
	r, _, acctKV := newStateReconcilerHarness(t, WithCPInstanceControl(cp), WithCPProvisioner(prov))
	require.NoError(t, PutClusterMeta(acctKV, meta))
	return r, acctKV
}

func TestMaybeReplaceCP_ReplacesLostMemberWithHealthyQuorum(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{node: ControlPlaneNode{NodeID: "node-c", InstanceID: "i-cp-new", ENIID: "eni-new", ENIIP: "10.0.0.20"}}
	meta := haMeta(nodes)
	r, acctKV := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute) // past grace

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	require.Equal(t, 1, prov.calls, "exactly one replacement provisioned for one lost member")
	assert.Equal(t, k3sServerJoinURL("10.0.0.10"), prov.lastReq.JoinURL, "joins the first healthy survivor's ENI IP")
	assert.Equal(t, haControlPlaneCount, prov.lastReq.MemberCount)
	assert.Equal(t, "10.0.0.12", prov.lastReq.DeadPeerIP, "replacement is asked to prune the dead member's etcd peer")
	assert.ElementsMatch(t, []string{"node-a", "node-b"}, prov.lastReq.ExcludeHosts, "excludes live-member hosts from placement")

	got, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	ids := cpMemberInstanceIDs(got)
	assert.Contains(t, ids, "i-cp-new", "replacement swapped into meta")
	assert.NotContains(t, ids, "i-cp2", "dead member dropped from meta")
}

func TestMaybeReplaceCP_DescribeErrorMemberTreatedAsLost(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{
		states: map[string]string{"i-cp0": "running", "i-cp1": "running"},
		errs:   map[string]error{"i-cp2": errors.New("not visible on any host")},
	}
	prov := &fakeCPProvisioner{node: ControlPlaneNode{NodeID: "node-c", InstanceID: "i-cp-new", ENIIP: "10.0.0.20"}}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Equal(t, 1, prov.calls, "an undescribable member is treated as lost and replaced")
}

func TestMaybeReplaceCP_NoQuorumDoesNotReplace(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "terminated", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Zero(t, prov.calls, "never add an etcd member without a surviving majority")
}

func TestMaybeReplaceCP_UnhealthyQuorumDoesNotReplace(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "apiserver healthz=\"fail\"")

	assert.Zero(t, prov.calls, "a degraded survivor quorum defers replacement")
}

func TestMaybeReplaceCP_RestartableMemberIsNoop(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "stopped"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Zero(t, prov.calls, "a stopped member is the restart path's; member-count pass yields")
	assert.True(t, r.replacingSince.IsZero(), "yielding clears the replace clock")
}

func TestMaybeReplaceCP_WithinGraceDoesNotReplace(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Zero(t, prov.calls, "no replacement before the grace window elapses")
	assert.False(t, r.replacingSince.IsZero(), "first lost tick starts the replace clock")
}

func TestMaybeReplaceCP_BackoffAndCap(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{node: ControlPlaneNode{NodeID: "node-c", InstanceID: "i-cp-new", ENIIP: "10.0.0.20"}}
	meta := haMeta(nodes)
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")
	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")
	assert.Equal(t, 1, prov.calls, "second attempt within backoff is suppressed")

	// Past backoff but at the attempt cap → still suppressed.
	r.lastReplaceAt = time.Now().Add(-10 * time.Minute)
	r.replaceAttempts = r.maxReplaceAttempts
	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")
	assert.Equal(t, 1, prov.calls, "no replacement once the attempt cap is reached")
}

func TestMaybeReplaceCP_ProvisionFailureLeavesDegraded(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{err: errors.New("no free host")}
	meta := haMeta(nodes)
	r, acctKV := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Equal(t, 1, prov.calls)
	got, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Contains(t, cpMemberInstanceIDs(got), "i-cp2", "meta unchanged when provision fails")
}

func TestMaybeReplaceCP_SingleCPClusterIsNoop(t *testing.T) {
	nodes := []ControlPlaneNode{{NodeID: "", InstanceID: "i-cp0", ENIIP: "10.0.0.10"}}
	cp := &mapCPControl{states: map[string]string{"i-cp0": "terminated"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	meta.ControlPlaneSpreadGroup = "" // single-CP: no spread
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Zero(t, prov.calls, "single-CP clusters have no quorum to join a replacement into")
}

func TestMaybeReplaceCP_NoTemplateIsNoop(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{}
	meta := haMeta(nodes)
	meta.ControlPlaneTemplate = nil // pre-feature cluster
	r, _ := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	r.maybeReplaceControlPlaneMember(context.Background(), meta, "")

	assert.Zero(t, prov.calls, "cannot replace faithfully without the persisted template")
}

func TestMaybeReplaceCP_NilProvisionerIsNoop(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	meta := haMeta(nodes)
	r, _, acctKV := newStateReconcilerHarness(t, WithCPInstanceControl(cp)) // no provisioner
	require.NoError(t, PutClusterMeta(acctKV, meta))
	r.replacingSince = time.Now().Add(-10 * time.Minute)

	require.NotPanics(t, func() {
		r.maybeReplaceControlPlaneMember(context.Background(), meta, "")
	})
}

func TestProvisionReplacementCP_LaunchesJoinOnFreeHost(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b", "node-c"}}
	inst := &seqK3sInst{}
	svc := newPlacerService(sched, &fakePlacer{}, &seqK3sVPC{}, inst)
	// IAM is unwired here, so the replacement re-derives static system creds from
	// deps (as CreateCluster does) — the launcher rejects an input with neither.
	svc.deps.SystemAccessKey = "AKIATEST"
	svc.deps.SystemSecretKey = "secret"
	tmpl := validK3sInput()

	node, err := svc.ProvisionReplacementCP(context.Background(), ReplacementCPRequest{
		ClusterName:  "alpha",
		Template:     &tmpl,
		JoinURL:      k3sServerJoinURL("10.0.0.10"),
		ExcludeHosts: []string{"node-a", "node-b"},
		MemberCount:  haControlPlaneCount,
		DeadPeerIP:   "10.0.0.12",
	})

	require.NoError(t, err)
	assert.Equal(t, "node-c", node.NodeID, "placed on the one non-excluded host")
	assert.Equal(t, "i-node-c", node.InstanceID)
	assert.NotEmpty(t, node.ENIID, "replacement carries a fresh ENI")
	assert.Contains(t, inst.userData["node-c"], "EKS_ETCD_PRUNE_PEER_IP=10.0.0.12",
		"replacement is told to prune the dead peer from etcd once joined")
}

func TestProvisionReplacementCP_NoFreeHostErrors(t *testing.T) {
	sched := &fakeHostScheduler{hosts: []string{"node-a", "node-b"}}
	svc := newPlacerService(sched, &fakePlacer{}, &seqK3sVPC{}, &seqK3sInst{})
	tmpl := validK3sInput()

	_, err := svc.ProvisionReplacementCP(context.Background(), ReplacementCPRequest{
		ClusterName:  "alpha",
		Template:     &tmpl,
		JoinURL:      k3sServerJoinURL("10.0.0.10"),
		ExcludeHosts: []string{"node-a", "node-b"},
		MemberCount:  haControlPlaneCount,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no schedulable host")
}

func TestProvisionReplacementCP_GuardsBadRequests(t *testing.T) {
	svc := newPlacerService(&fakeHostScheduler{}, &fakePlacer{}, &seqK3sVPC{}, &seqK3sInst{})

	_, err := svc.ProvisionReplacementCP(context.Background(), ReplacementCPRequest{JoinURL: "x"})
	require.Error(t, err, "nil template rejected")

	tmpl := validK3sInput()
	_, err = svc.ProvisionReplacementCP(context.Background(), ReplacementCPRequest{Template: &tmpl})
	require.Error(t, err, "empty join URL rejected")
}

// TestActiveBranchReplacesLostCPMember exercises the full ACTIVE reconcile path:
// a healthy 2/3 quorum with one terminated member drives a replacement provision
// and meta swap once past the grace window.
func TestActiveBranchReplacesLostCPMember(t *testing.T) {
	nodes := threeMemberCP()
	cp := &mapCPControl{states: map[string]string{"i-cp0": "running", "i-cp1": "running", "i-cp2": "terminated"}}
	prov := &fakeCPProvisioner{node: ControlPlaneNode{NodeID: "node-c", InstanceID: "i-cp-new", ENIIP: "10.0.0.20"}}
	meta := haMeta(nodes)
	r, acctKV := newReplaceReconciler(t, cp, prov, meta)
	r.replacingSince = time.Now().Add(-10 * time.Minute)
	r.latest.Store(freshReport("ok", 3))

	require.NoError(t, r.reconcileOnce(context.Background()))

	require.Equal(t, 1, prov.calls, "wedged member replaced through the ACTIVE arm")
	got, err := GetClusterMeta(acctKV, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status, "status stays AWS-faithful ACTIVE")
	assert.Contains(t, cpMemberInstanceIDs(got), "i-cp-new")
	assert.NotContains(t, cpMemberInstanceIDs(got), "i-cp2")
}
