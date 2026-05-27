package reconcile

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// TestL1_ReconcilerConverges enforces ADR-0006 liveness clause L1.
//
//	"While both the NATS KV and OVN NB DB are reachable, the reconciler
//	 eventually drives the delta to zero. Idempotent EnsureX methods make
//	 repeated runs safe."
//
// Two reconcile passes over IntentStates of N={1,3,8} VPCs: state on pass 2
// must equal state on pass 1.
func TestL1_ReconcilerConverges(t *testing.T) {
	for _, n := range []int{1, 3, 8} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			rec, m := newTestReconciler(t)
			ctx := context.Background()
			intent := scaledIntent(t, n)

			if err := rec.Reconcile(ctx, intent); err != nil {
				t.Fatalf("Reconcile #1: %v", err)
			}
			routers1 := len(m.Routers)
			switches1 := len(m.Switches)
			ports1 := len(m.Ports)
			pgs1 := len(m.PortGroups)

			if err := rec.Reconcile(ctx, intent); err != nil {
				t.Fatalf("Reconcile #2: %v", err)
			}
			if len(m.Routers) != routers1 {
				t.Errorf("ADR-0006 L1: convergence violated — routers %d → %d on second pass", routers1, len(m.Routers))
			}
			if len(m.Switches) != switches1 {
				t.Errorf("ADR-0006 L1: convergence violated — switches %d → %d on second pass", switches1, len(m.Switches))
			}
			if len(m.Ports) != ports1 {
				t.Errorf("ADR-0006 L1: convergence violated — ports %d → %d on second pass", ports1, len(m.Ports))
			}
			if len(m.PortGroups) != pgs1 {
				t.Errorf("ADR-0006 L1: convergence violated — port groups %d → %d on second pass", pgs1, len(m.PortGroups))
			}
		})
	}
}

// TestL2_DriftIntervalBounded enforces ADR-0006 liveness clause L2.
//
//	"While the leader is active, the periodic drift-detection loop
//	 eventually re-runs and re-applies any divergence introduced since
//	 the last reconciler run."
//
// Structural-only: bounds DriftInterval to (0, 30m]. Full behavioural
// coverage blocked on a clock-injectable DriftLoop signature.
func TestL2_DriftIntervalBounded(t *testing.T) {
	if DriftInterval <= 0 {
		t.Fatalf("ADR-0006 L2: DriftInterval must be > 0; got %v", DriftInterval)
	}
	if DriftInterval > 30*time.Minute {
		t.Fatalf("ADR-0006 L2: DriftInterval %v exceeds 30m support-window upper bound", DriftInterval)
	}
	// TODO: inject a fake clock + fake JetStream into DriftLoop and assert
	// runDriftCycle is invoked at least twice within ctx.Deadline().
	// Blocked on a clock-injectable DriftLoop signature.
}

// TestL3_FederationReEnablesOnLinkRecovery is a placeholder for ADR-0006 L3.
//
//	"Once an inter-AZ link reaches ≥ Degraded class (ADR-0003), L4
//	 eventually calls BringUpLink and establishes the OVN-IC tunnel to
//	 the peer AZ."
func TestL3_FederationReEnablesOnLinkRecovery(t *testing.T) {
	t.Skip(`ADR-0006 L3 deferred: network/federation/ has not yet been built. ` +
		`Once it lands, replace this stub with a test that drives a fake ` +
		`LinkObserver from Down → Degraded and asserts BringUpLink fires.`)
}

// scaledIntent produces an IntentState with n VPCs, each holding one
// subnet, one ENI port, and one SG.
func scaledIntent(t *testing.T, n int) IntentState {
	t.Helper()
	intent := IntentState{
		VPCs:    make(map[string]topology.VPCSpec, n),
		Subnets: make(map[string]topology.SubnetSpec, n),
		Ports:   make(map[string]topology.PortSpec, n),
		SGs:     make(map[string]policy.SGSpec, n),
		IGWs:    map[string]external.IGWSpec{},
		EIPs:    map[string]policy.EIPSpec{},
		NATGWs:  map[string]policy.NATGWSpec{},
	}
	for i := range n {
		vpcID := fmt.Sprintf("vpc-l1-%d", i)
		subnetID := fmt.Sprintf("subnet-l1-%d", i)
		eniID := fmt.Sprintf("eni-l1-%d", i)
		sgID := fmt.Sprintf("sg-l1-%d", i)

		base := byte(i)
		mac, _ := net.ParseMAC(fmt.Sprintf("02:00:00:00:00:%02x", base+1))

		intent.VPCs[vpcID] = topology.VPCSpec{
			VPCID: vpcID,
			CIDR:  netip.MustParsePrefix(fmt.Sprintf("10.%d.0.0/16", base)),
			VNI:   int64(100 + i),
		}
		intent.Subnets[subnetID] = topology.SubnetSpec{
			SubnetID: subnetID, VPCID: vpcID,
			CIDR: netip.MustParsePrefix(fmt.Sprintf("10.%d.1.0/24", base)),
		}
		intent.Ports[eniID] = topology.PortSpec{
			PortID: eniID, SubnetID: subnetID, VPCID: vpcID,
			PrivateIP: netip.MustParseAddr(fmt.Sprintf("10.%d.1.10", base)),
			MAC:       mac, SGIDs: []string{sgID},
		}
		intent.SGs[sgID] = policy.SGSpec{GroupID: sgID, VPCID: vpcID}
	}
	return intent
}
