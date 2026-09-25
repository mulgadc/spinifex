package reconcile

// The orphan sweeps read absence from intent as permission to delete, and a
// failed KV read is indistinguishable from an empty bucket: a wedged store
// returned no keys while writes were still landing, and the sweep took every
// running guest's port with it. These lock both defences — a port with a live
// local tap is never an orphan, and a sweep is refused outright when the keep
// set it would delete against holds nothing at all.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// seedGuestPorts converges every named ENI in one pass, so no intermediate
// Reconcile sweeps a port the next one was going to need.
func seedGuestPorts(t *testing.T, r *reconciler, eniIDs ...string) IntentState {
	t.Helper()
	intent := freshIntent(t)
	intent.Ports = map[string]topology.PortSpec{}
	for i, eniID := range eniIDs {
		mac, err := net.ParseMAC(fmt.Sprintf("02:00:00:00:00:%02x", i+1))
		if err != nil {
			t.Fatalf("ParseMAC: %v", err)
		}
		intent.Ports[eniID] = topology.PortSpec{PortID: eniID, SubnetID: "subnet-a", VPCID: "vpc-a",
			PrivateIP: netip.MustParseAddr(fmt.Sprintf("10.0.1.%d", 10+i)), MAC: mac, SGIDs: []string{"sg-a"}}
	}
	if err := r.Reconcile(context.Background(), intent); err != nil {
		t.Fatalf("seed Reconcile: %v", err)
	}
	return intent
}

func TestPruneOrphanPorts_RefusesWhenIntentHoldsNoENIsAtAll(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()
	intent := seedGuestPorts(t, r, "eni-a")

	intent.Ports = map[string]topology.PortSpec{}
	res := &passResult{}
	r.pruneOrphanPorts(ctx, intent, res)

	if _, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-a")); err != nil {
		t.Errorf("guest port swept against an empty ENI intent: %v", err)
	}
	if len(res.failures) != 1 || !errors.Is(res.failures[0].err, errEmptyIntentSweep) {
		t.Errorf("refusal not recorded on the pass: %+v", res.failures)
	}
}

// The precise defence, and the one that holds when intent is only partly empty:
// dropping a single VPC filters out every ENI in it without emptying the set, so
// the count guard never fires and only the tap can save those guests.
func TestPruneOrphanPorts_KeepsAPortWhoseTapIsPluggedInHere(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()
	intent := seedGuestPorts(t, r, "eni-a", "eni-b")

	r.localPorts = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{topology.Port("eni-a"): {}}, nil
	}
	// eni-b stays in intent, so the set is non-empty and the count guard is silent.
	delete(intent.Ports, "eni-a")

	r.pruneOrphanPorts(ctx, intent, &passResult{})

	if _, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-a")); err != nil {
		t.Errorf("plugged-in guest port swept: %v", err)
	}
}

// Without a tap the same port is a real orphan and must still go, or the sweep
// stops reclaiming anything.
func TestPruneOrphanPorts_SweepsAPortWithNoLocalTap(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()
	intent := seedGuestPorts(t, r, "eni-a", "eni-b")

	r.localPorts = func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{topology.Port("eni-b"): {}}, nil
	}
	delete(intent.Ports, "eni-a")

	r.pruneOrphanPorts(ctx, intent, &passResult{})

	if _, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-a")); err == nil {
		t.Errorf("orphan ENI port with no local tap was not swept")
	}
	if _, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-b")); err != nil {
		t.Errorf("live ENI port swept alongside the orphan: %v", err)
	}
}

// An unreadable OVS is a lost liveness signal, so the sweep must stand down
// rather than fall back to deciding on intent alone.
func TestPruneOrphanPorts_SkipsWhenLocalOVSCannotBeRead(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()
	intent := seedGuestPorts(t, r, "eni-a", "eni-b")

	boom := errors.New("ovs-vsctl: connection refused")
	r.localPorts = func(context.Context) (map[string]struct{}, error) { return nil, boom }
	delete(intent.Ports, "eni-a")

	res := &passResult{}
	r.pruneOrphanPorts(ctx, intent, res)

	if _, err := m.GetLogicalSwitchPort(ctx, topology.Port("eni-a")); err != nil {
		t.Errorf("port swept with no liveness signal available: %v", err)
	}
	if len(res.failures) != 1 || !errors.Is(res.failures[0].err, boom) {
		t.Errorf("skip not recorded on the pass: %+v", res.failures)
	}
}

// A NAT gateway's snat row is not stamped with an owning port, so it must not
// count toward the refusal and must never be swept by the dnat_and_snat pass.
func TestPruneOrphanEIPs_RefusesWhenLiveSetIsEmpty(t *testing.T) {
	r, m := newTestReconciler(t)
	ctx := context.Background()
	seedGuestPorts(t, r, "eni-a")
	if err := m.AddNAT(ctx, topology.VPCRouter("vpc-a"), &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.10", LogicalIP: "10.0.1.10",
		ExternalIDs: map[string]string{"spinifex:logical_port": topology.Port("eni-a")},
	}); err != nil {
		t.Fatalf("AddNAT: %v", err)
	}

	empty := freshIntent(t)
	empty.Ports = map[string]topology.PortSpec{}
	empty.EIPs = map[string]policy.EIPSpec{}
	res := &passResult{}
	r.pruneOrphanEIPs(ctx, empty, res)

	if len(res.failures) == 0 {
		t.Errorf("empty-intent EIP sweep was not refused")
	}
	if findNATByExternal(m, "dnat_and_snat", "192.168.1.10") == nil {
		t.Errorf("dnat_and_snat row swept against an empty live set")
	}
}
