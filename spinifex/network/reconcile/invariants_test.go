package reconcile

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// IMDS-datapath invariants. Guest LSPs and DHCPOptions rows are created by two
// independent paths (live topology manager and reconciler); both are exercised
// here so neither can drift from the contract the IMDS handler trusts.

// TestI1_GuestLSPMustHavePortSecurity asserts every guest LSP carries
// port_security == "<MAC> <IP>". Without it a compromised guest can forge a
// peer's source IP, breaking the IMDS (VPC-ID, source-IP) → ENI mapping.
func TestI1_GuestLSPMustHavePortSecurity(t *testing.T) {
	ctx := context.Background()

	// Live path: topology.EnsurePort.
	t.Run("live", func(t *testing.T) {
		m := mock.New()
		mgr := topology.NewLiveManager(m)
		vpc := topology.VPCSpec{VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16")}
		sub := topology.SubnetSpec{SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.1.0/24")}
		mac, _ := net.ParseMAC("02:00:00:00:00:01")
		port := topology.PortSpec{
			PortID: "eni-a", SubnetID: "subnet-a", VPCID: "vpc-a",
			PrivateIP: netip.MustParseAddr("10.0.1.5"), MAC: mac,
		}
		if err := mgr.EnsureVPC(ctx, vpc); err != nil {
			t.Fatalf("EnsureVPC: %v", err)
		}
		if err := mgr.EnsureSubnet(ctx, sub); err != nil {
			t.Fatalf("EnsureSubnet: %v", err)
		}
		if err := mgr.EnsurePort(ctx, port); err != nil {
			t.Fatalf("EnsurePort: %v", err)
		}
		assertPortSecurity(t, m, topology.Port(port.PortID), "02:00:00:00:00:01 10.0.1.5")
	})

	// Reconciler path: applyPorts.
	t.Run("reconciler", func(t *testing.T) {
		rec, m := newTestReconciler(t)
		intent := freshIntent(t)
		if err := rec.Reconcile(ctx, intent); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		port := intent.Ports["eni-a"]
		want := port.MAC.String() + " " + port.PrivateIP.String()
		assertPortSecurity(t, m, topology.Port(port.PortID), want)
	})
}

// TestRLC5_OrphanLSPPrunedAfterReconcile asserts ADR-0003 §4 "OVN LSP prune":
// an LSP whose spinifex:eni_id has no matching intent ENI is deleted by one
// reconcile. Without it, guest LSPs leak across instance terminate and host
// reinstall — the orphan-LSP capacity-drift root cause.
func TestRLC5_OrphanLSPPrunedAfterReconcile(t *testing.T) {
	ctx := context.Background()
	rec, m := newTestReconciler(t)
	intent := freshIntent(t)

	// First reconcile builds subnet-a, the sg_a port group, and port-eni-a.
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	// Seed an orphan ENI LSP joined to the SG port group: prune must clear its
	// memberships then delete it (composed DeletePort cascade).
	orphan := &nbdb.LogicalSwitchPort{
		Name: topology.Port("eni-orphan"),
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-orphan",
			"spinifex:subnet_id": "subnet-a",
			"spinifex:vpc_id":    "vpc-a",
		},
	}
	if err := m.CreateLogicalSwitchPortInGroups(ctx, topology.SubnetSwitch("subnet-a"), orphan,
		[]string{topology.SecurityGroupPortGroup("sg-a")}); err != nil {
		t.Fatalf("seed orphan LSP: %v", err)
	}

	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}

	if _, ok := m.Ports[topology.Port("eni-orphan")]; ok {
		t.Errorf("orphan LSP %s survived reconcile: ADR-0003 §4 requires an LSP "+
			"whose spinifex:eni_id has no intent ENI to be pruned; create-only gap "+
			"leaks OVN ports across terminate/reinstall", topology.Port("eni-orphan"))
	}
	if _, ok := m.Ports[topology.Port("eni-a")]; !ok {
		t.Errorf("desired ENI LSP %s was pruned: prune must never delete an LSP "+
			"present in intent", topology.Port("eni-a"))
	}
}

// assertPortSecurity fails when the LSP's port_security doesn't match its addresses.
func assertPortSecurity(t *testing.T, m *mock.Client, portName, want string) {
	t.Helper()
	lsp, err := m.GetLogicalSwitchPort(context.Background(), portName)
	if err != nil {
		t.Fatalf("guest LSP %s missing: %v", portName, err)
	}
	if len(lsp.PortSecurity) != 1 || lsp.PortSecurity[0] != want {
		t.Errorf("guest LSP %s PortSecurity = %v, want [%q]: guest LSPs without "+
			"port_security allow source-IP spoofing, breaking the IMDS "+
			"(VPC-ID, source-IP) → ENI mapping", portName, lsp.PortSecurity, want)
	}
	// port_security must mirror addresses exactly.
	if len(lsp.Addresses) != 1 || lsp.Addresses[0] != lsp.PortSecurity[0] {
		t.Errorf("guest LSP %s Addresses %v != PortSecurity %v: enforced and advertised "+
			"identity must match", portName, lsp.Addresses, lsp.PortSecurity)
	}
}
