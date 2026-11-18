// Exercises newFlowsBarrier, which is unexported wiring, by swapping the
// unexported waitForFlowsHV var it closes over.
//
//test:in-package
package vpcd

import (
	"context"
	"net/netip"
	"os/exec"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The barrier has to confirm against the same NB cluster vpcd writes through.
// A barrier with no address falls back to the local unix socket, which on a
// node outside the configured cluster is a database that never saw the write.
func TestNewFlowsBarrierUsesTheConfiguredNBAddress(t *testing.T) {
	const nbAddr = "tcp:10.2.0.2:6641,tcp:10.2.0.3:6641,tcp:10.2.0.4:6641"

	original := waitForFlowsHV
	t.Cleanup(func() { waitForFlowsHV = original })

	var got string
	var calls int
	waitForFlowsHV = func(addr string) error {
		got = addr
		calls++
		return nil
	}

	require.NoError(t, newFlowsBarrier(nbAddr)())
	assert.Equal(t, 1, calls, "the barrier runs the sync once per call")
	assert.Equal(t, nbAddr, got, "the barrier confirms against the configured cluster")
}

// A barrier that cannot reach the chassis must hold the connection in
// provisioning and fail the pass, so the next pass after the SB returns
// is the one that reports it active.
func TestFlowsBarrierFailureKeepsPeeringProvisioning(t *testing.T) {
	sbDown := true
	original := sudoCommand
	t.Cleanup(func() { sudoCommand = original })
	sudoCommand = func(string, ...string) *exec.Cmd {
		if sbDown {
			return exec.Command("sh", "-c", "echo 'ovn-nbctl: timeout expired' >&2; exit 1")
		}
		return exec.Command("true")
	}

	m := mock.New()
	sg := policy.NewSecurityGroupManager(m, policy.EgressPolicy{})
	nat, err := policy.NewNATManager(m, policy.NATModeDistributed)
	require.NoError(t, err)
	routes := policy.NewRouteManager(m)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat, Allocator: external.LinkLocalAllocator{}, NATMode: policy.NATModeDistributed,
	})
	require.NoError(t, err)
	active := map[string]int64{}
	rec, err := reconcile.New(reconcile.Config{
		OVN: m, SG: sg, NAT: nat, Routes: routes, IGW: igw, Topology: topology.NewLiveManager(m),
		LocalAZ: "us-east-1a", NodeHostname: "test-host",
		FlowsBarrier: newFlowsBarrier("tcp:127.0.0.1:6641"),
		MarkPeeringActive: func(_ context.Context, id string, gen int64) error {
			active[id] = gen
			return nil
		},
	})
	require.NoError(t, err)

	ctx := context.Background()
	err = rec.Reconcile(ctx, barrierPeeringIntent())
	require.ErrorIs(t, err, reconcile.ErrPassIncomplete, "a failed barrier fails the pass so it is retried")
	assert.Empty(t, active, "no chassis confirmed the link, so it stays provisioning")

	sbDown = false
	require.NoError(t, rec.Reconcile(ctx, barrierPeeringIntent()))
	assert.Equal(t, map[string]int64{barrierPcx: 3}, active, "the pass after the SB returns reports it active")
}

const barrierPcx = "pcx-0123456789abcdef0"

func barrierPeeringIntent() reconcile.IntentState {
	reqCIDR, accCIDR := netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.1.0.0/16")
	spec := topology.PeeringLinkSpec{
		PcxID: barrierPcx, RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: reqCIDR, AccepterCIDR: accCIDR,
		RequesterAddr: netip.MustParseAddr("169.254.192.1"), AccepterAddr: netip.MustParseAddr("169.254.192.2"),
	}
	return reconcile.IntentState{
		VPCs: map[string]topology.VPCSpec{
			"vpc-a": {VPCID: "vpc-a", CIDR: reqCIDR, VNI: 100},
			"vpc-b": {VPCID: "vpc-b", CIDR: accCIDR, VNI: 101},
		},
		Subnets: map[string]topology.SubnetSpec{
			"subnet-a": {SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.1.0/24")},
			"subnet-b": {SubnetID: "subnet-b", VPCID: "vpc-b", CIDR: netip.MustParsePrefix("10.1.1.0/24")},
		},
		Ports:       map[string]topology.PortSpec{},
		SGs:         map[string]policy.SGSpec{},
		IGWs:        map[string]external.IGWSpec{},
		EIPs:        map[string]policy.EIPSpec{},
		NATGWs:      map[string]policy.NATGWSpec{},
		IGWRoutes:   map[string]reconcile.SubnetEgressIntent{},
		NATGWRoutes: map[string]reconcile.SubnetEgressIntent{},
		DropGates:   map[string]reconcile.SubnetEgressIntent{},
		PeeringIntent: reconcile.PeeringIntent{
			PeeringLinks:     map[string]reconcile.PeeringLinkIntent{barrierPcx: {Spec: spec, Generation: 3}},
			PeeringTeardowns: map[string]reconcile.PeeringTeardownIntent{},
			PeeringLive:      map[string]struct{}{barrierPcx: {}},
		},
	}
}
