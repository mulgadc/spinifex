package vpcd

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

func newManagerForTest(t *testing.T) (topology.Manager, *MockOVNClient) {
	t.Helper()
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	h := NewTopologyHandler(mock)
	return h, mock
}

func TestManager_EnsureVPC(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()
	spec := topology.VPCSpec{
		VPCID: "vpc-mgr1",
		CIDR:  netip.MustParsePrefix("10.0.0.0/16"),
		VNI:   42,
	}
	if err := mgr.EnsureVPC(ctx, spec); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	got, err := mock.GetLogicalRouter(ctx, topology.VPCRouter(spec.VPCID))
	if err != nil {
		t.Fatalf("router not present: %v", err)
	}
	if got.ExternalIDs["spinifex:vpc_id"] != spec.VPCID {
		t.Errorf("vpc_id mismatch: %q", got.ExternalIDs["spinifex:vpc_id"])
	}
	if got.ExternalIDs["spinifex:cidr"] != "10.0.0.0/16" {
		t.Errorf("cidr mismatch: %q", got.ExternalIDs["spinifex:cidr"])
	}

	// Second call is a no-op (idempotent).
	if err := mgr.EnsureVPC(ctx, spec); err != nil {
		t.Fatalf("EnsureVPC second call: %v", err)
	}
}

func TestManager_EnsureSubnet(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()
	vpc := topology.VPCSpec{VPCID: "vpc-sub", CIDR: netip.MustParsePrefix("10.1.0.0/16")}
	if err := mgr.EnsureVPC(ctx, vpc); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	sub := topology.SubnetSpec{
		SubnetID: "subnet-A",
		VPCID:    vpc.VPCID,
		CIDR:     netip.MustParsePrefix("10.1.1.0/24"),
	}
	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	if _, err := mock.GetLogicalSwitch(ctx, topology.SubnetSwitch(sub.SubnetID)); err != nil {
		t.Errorf("subnet switch missing: %v", err)
	}
	if _, err := mock.GetLogicalRouterPort(ctx, topology.SubnetRouterPort(sub.SubnetID)); err != nil {
		t.Errorf("subnet router port missing: %v", err)
	}
	if _, err := mock.GetLogicalSwitchPort(ctx, topology.SubnetSwitchRouterPort(sub.SubnetID)); err != nil {
		t.Errorf("subnet switch-side router port missing: %v", err)
	}

	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet second call: %v", err)
	}
}

func TestManager_EnsurePort(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()

	vpc := topology.VPCSpec{VPCID: "vpc-port", CIDR: netip.MustParsePrefix("10.2.0.0/16")}
	sub := topology.SubnetSpec{SubnetID: "subnet-P", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.2.1.0/24")}
	if err := mgr.EnsureVPC(ctx, vpc); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}

	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	port := topology.PortSpec{
		PortID:    "eni-1",
		SubnetID:  sub.SubnetID,
		VPCID:     vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.2.1.10"),
		MAC:       mac,
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}
	got, err := mock.GetLogicalSwitchPort(ctx, topology.Port(port.PortID))
	if err != nil {
		t.Fatalf("port LSP missing: %v", err)
	}
	if got.ExternalIDs["spinifex:eni_id"] != port.PortID {
		t.Errorf("eni_id mismatch: %q", got.ExternalIDs["spinifex:eni_id"])
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "02:00:00:00:00:01 10.2.1.10" {
		t.Errorf("addresses mismatch: %v", got.Addresses)
	}

	// Idempotent re-call.
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort second call: %v", err)
	}
}

func TestManager_DeletePort(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()
	vpc := topology.VPCSpec{VPCID: "vpc-dp", CIDR: netip.MustParsePrefix("10.3.0.0/16")}
	sub := topology.SubnetSpec{SubnetID: "subnet-DP", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.3.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)
	mac, _ := net.ParseMAC("02:00:00:00:00:02")
	port := topology.PortSpec{
		PortID: "eni-DP", SubnetID: sub.SubnetID, VPCID: vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.3.1.5"), MAC: mac,
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}
	if err := mgr.DeletePort(ctx, port); err != nil {
		t.Fatalf("DeletePort: %v", err)
	}
	if _, err := mock.GetLogicalSwitchPort(ctx, topology.Port(port.PortID)); err == nil {
		t.Fatal("expected LSP to be gone after DeletePort")
	}
}

func TestManager_DeleteSubnetAndVPC(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()
	vpc := topology.VPCSpec{VPCID: "vpc-del", CIDR: netip.MustParsePrefix("10.4.0.0/16")}
	sub := topology.SubnetSpec{SubnetID: "subnet-del", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.4.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)

	if err := mgr.DeleteSubnet(ctx, sub); err != nil {
		t.Fatalf("DeleteSubnet: %v", err)
	}
	if _, err := mock.GetLogicalSwitch(ctx, topology.SubnetSwitch(sub.SubnetID)); err == nil {
		t.Fatal("expected subnet switch to be gone")
	}

	if err := mgr.DeleteVPC(ctx, vpc.VPCID); err != nil {
		t.Fatalf("DeleteVPC: %v", err)
	}
	if _, err := mock.GetLogicalRouter(ctx, topology.VPCRouter(vpc.VPCID)); err == nil {
		t.Fatal("expected VPC router to be gone")
	}
}

func TestManager_SetPortSecurityGroups(t *testing.T) {
	mgr, mock := newManagerForTest(t)
	ctx := context.Background()
	vpc := topology.VPCSpec{VPCID: "vpc-sg", CIDR: netip.MustParsePrefix("10.5.0.0/16")}
	sub := topology.SubnetSpec{SubnetID: "subnet-sg", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.5.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)

	// Seed an SG port group so the port can join it.
	if err := mock.CreatePortGroup(ctx, topology.SecurityGroupPortGroup("sg-A"), nil); err != nil {
		t.Fatalf("seed port group: %v", err)
	}

	mac, _ := net.ParseMAC("02:00:00:00:00:03")
	port := topology.PortSpec{
		PortID: "eni-sg", SubnetID: sub.SubnetID, VPCID: vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.5.1.7"), MAC: mac,
		SGIDs: []string{"sg-A"},
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}

	// Seed second SG, then move membership from A to B.
	if err := mock.CreatePortGroup(ctx, topology.SecurityGroupPortGroup("sg-B"), nil); err != nil {
		t.Fatalf("seed port group B: %v", err)
	}
	if err := mgr.SetPortSecurityGroups(ctx, port.PortID, []string{"sg-B"}); err != nil {
		t.Fatalf("SetPortSecurityGroups: %v", err)
	}
	names, err := mock.ListPortGroupsForPort(ctx, topology.Port(port.PortID))
	if err != nil {
		t.Fatalf("ListPortGroupsForPort: %v", err)
	}
	if len(names) != 1 || names[0] != topology.SecurityGroupPortGroup("sg-B") {
		t.Errorf("expected only sg-B membership, got %v", names)
	}
}
