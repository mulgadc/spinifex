package topology

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
)

func newLiveManagerForTest(t *testing.T) (Manager, *mock.Client) {
	t.Helper()
	m := mock.New()
	_ = m.Connect(context.Background())
	return NewLiveManager(m), m
}

func TestLiveManager_EnsureVPC(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()
	spec := VPCSpec{
		VPCID: "vpc-mgr1",
		CIDR:  netip.MustParsePrefix("10.0.0.0/16"),
		VNI:   42,
	}
	if err := mgr.EnsureVPC(ctx, spec); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	got, err := mockClient.GetLogicalRouter(ctx, VPCRouter(spec.VPCID))
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

func TestLiveManager_EnsureSubnet(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()
	vpc := VPCSpec{VPCID: "vpc-sub", CIDR: netip.MustParsePrefix("10.1.0.0/16")}
	if err := mgr.EnsureVPC(ctx, vpc); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	sub := SubnetSpec{
		SubnetID: "subnet-A",
		VPCID:    vpc.VPCID,
		CIDR:     netip.MustParsePrefix("10.1.1.0/24"),
	}
	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}
	if _, err := mockClient.GetLogicalSwitch(ctx, SubnetSwitch(sub.SubnetID)); err != nil {
		t.Errorf("subnet switch missing: %v", err)
	}
	if _, err := mockClient.GetLogicalRouterPort(ctx, SubnetRouterPort(sub.SubnetID)); err != nil {
		t.Errorf("subnet router port missing: %v", err)
	}
	if _, err := mockClient.GetLogicalSwitchPort(ctx, SubnetSwitchRouterPort(sub.SubnetID)); err != nil {
		t.Errorf("subnet switch-side router port missing: %v", err)
	}

	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet second call: %v", err)
	}
}

func TestLiveManager_EnsurePort(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()

	vpc := VPCSpec{VPCID: "vpc-port", CIDR: netip.MustParsePrefix("10.2.0.0/16")}
	sub := SubnetSpec{SubnetID: "subnet-P", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.2.1.0/24")}
	if err := mgr.EnsureVPC(ctx, vpc); err != nil {
		t.Fatalf("EnsureVPC: %v", err)
	}
	if err := mgr.EnsureSubnet(ctx, sub); err != nil {
		t.Fatalf("EnsureSubnet: %v", err)
	}

	mac, _ := net.ParseMAC("02:00:00:00:00:01")
	port := PortSpec{
		PortID:    "eni-1",
		SubnetID:  sub.SubnetID,
		VPCID:     vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.2.1.10"),
		MAC:       mac,
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}
	got, err := mockClient.GetLogicalSwitchPort(ctx, Port(port.PortID))
	if err != nil {
		t.Fatalf("port LSP missing: %v", err)
	}
	if got.ExternalIDs["spinifex:eni_id"] != port.PortID {
		t.Errorf("eni_id mismatch: %q", got.ExternalIDs["spinifex:eni_id"])
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "02:00:00:00:00:01 10.2.1.10" {
		t.Errorf("addresses mismatch: %v", got.Addresses)
	}

	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort second call: %v", err)
	}
}

func TestLiveManager_DeletePort(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()
	vpc := VPCSpec{VPCID: "vpc-dp", CIDR: netip.MustParsePrefix("10.3.0.0/16")}
	sub := SubnetSpec{SubnetID: "subnet-DP", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.3.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)
	mac, _ := net.ParseMAC("02:00:00:00:00:02")
	port := PortSpec{
		PortID: "eni-DP", SubnetID: sub.SubnetID, VPCID: vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.3.1.5"), MAC: mac,
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}
	if err := mgr.DeletePort(ctx, port); err != nil {
		t.Fatalf("DeletePort: %v", err)
	}
	if _, err := mockClient.GetLogicalSwitchPort(ctx, Port(port.PortID)); err == nil {
		t.Fatal("expected LSP to be gone after DeletePort")
	}
}

func TestLiveManager_DeleteSubnetAndVPC(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()
	vpc := VPCSpec{VPCID: "vpc-del", CIDR: netip.MustParsePrefix("10.4.0.0/16")}
	sub := SubnetSpec{SubnetID: "subnet-del", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.4.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)

	if err := mgr.DeleteSubnet(ctx, sub); err != nil {
		t.Fatalf("DeleteSubnet: %v", err)
	}
	if _, err := mockClient.GetLogicalSwitch(ctx, SubnetSwitch(sub.SubnetID)); err == nil {
		t.Fatal("expected subnet switch to be gone")
	}

	if err := mgr.DeleteVPC(ctx, vpc.VPCID); err != nil {
		t.Fatalf("DeleteVPC: %v", err)
	}
	if _, err := mockClient.GetLogicalRouter(ctx, VPCRouter(vpc.VPCID)); err == nil {
		t.Fatal("expected VPC router to be gone")
	}
}

func TestLiveManager_EnsureSGPortGroup(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()

	if err := mgr.EnsureSGPortGroup(ctx, "sg-pg1"); err != nil {
		t.Fatalf("EnsureSGPortGroup: %v", err)
	}
	pg, err := mockClient.GetPortGroup(ctx, SecurityGroupPortGroup("sg-pg1"))
	if err != nil {
		t.Fatalf("port group not present: %v", err)
	}
	if pg.Name != SecurityGroupPortGroup("sg-pg1") {
		t.Errorf("port group name mismatch: %q", pg.Name)
	}

	// Idempotent: second call must not fail or churn state.
	if err := mgr.EnsureSGPortGroup(ctx, "sg-pg1"); err != nil {
		t.Fatalf("EnsureSGPortGroup second call: %v", err)
	}
}

func TestLiveManager_DeleteSGPortGroup(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()

	if err := mgr.EnsureSGPortGroup(ctx, "sg-del1"); err != nil {
		t.Fatalf("EnsureSGPortGroup: %v", err)
	}
	if err := mgr.DeleteSGPortGroup(ctx, "sg-del1"); err != nil {
		t.Fatalf("DeleteSGPortGroup: %v", err)
	}
	if _, err := mockClient.GetPortGroup(ctx, SecurityGroupPortGroup("sg-del1")); err == nil {
		t.Fatalf("port group still present after delete")
	}

	// Idempotent on already-absent.
	if err := mgr.DeleteSGPortGroup(ctx, "sg-del1"); err != nil {
		t.Fatalf("DeleteSGPortGroup second call: %v", err)
	}
}

func TestLiveManager_SetPortSecurityGroups(t *testing.T) {
	mgr, mockClient := newLiveManagerForTest(t)
	ctx := context.Background()
	vpc := VPCSpec{VPCID: "vpc-sg", CIDR: netip.MustParsePrefix("10.5.0.0/16")}
	sub := SubnetSpec{SubnetID: "subnet-sg", VPCID: vpc.VPCID, CIDR: netip.MustParsePrefix("10.5.1.0/24")}
	_ = mgr.EnsureVPC(ctx, vpc)
	_ = mgr.EnsureSubnet(ctx, sub)

	if err := mockClient.CreatePortGroup(ctx, SecurityGroupPortGroup("sg-A"), nil); err != nil {
		t.Fatalf("seed port group: %v", err)
	}

	mac, _ := net.ParseMAC("02:00:00:00:00:03")
	port := PortSpec{
		PortID: "eni-sg", SubnetID: sub.SubnetID, VPCID: vpc.VPCID,
		PrivateIP: netip.MustParseAddr("10.5.1.7"), MAC: mac,
		SGIDs: []string{"sg-A"},
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}

	if err := mockClient.CreatePortGroup(ctx, SecurityGroupPortGroup("sg-B"), nil); err != nil {
		t.Fatalf("seed port group B: %v", err)
	}
	if err := mgr.SetPortSecurityGroups(ctx, port.PortID, []string{"sg-B"}); err != nil {
		t.Fatalf("SetPortSecurityGroups: %v", err)
	}
	names, err := mockClient.ListPortGroupsForPort(ctx, Port(port.PortID))
	if err != nil {
		t.Fatalf("ListPortGroupsForPort: %v", err)
	}
	if len(names) != 1 || names[0] != SecurityGroupPortGroup("sg-B") {
		t.Errorf("expected only sg-B membership, got %v", names)
	}
}

func TestLiveManager_WithDNSServer(t *testing.T) {
	mockClient := mock.New()
	_ = mockClient.Connect(context.Background())
	mgr := NewLiveManager(mockClient, WithDNSServer(func() string { return "{10.0.0.2}" }))
	ctx := context.Background()
	_ = mgr.EnsureVPC(ctx, VPCSpec{VPCID: "vpc-dns", CIDR: netip.MustParsePrefix("10.6.0.0/16")})
	_ = mgr.EnsureSubnet(ctx, SubnetSpec{SubnetID: "subnet-dns", VPCID: "vpc-dns", CIDR: netip.MustParsePrefix("10.6.1.0/24")})
	opts, err := mockClient.FindDHCPOptionsByCIDR(ctx, "10.6.1.0/24")
	if err != nil {
		t.Fatalf("DHCPOptions missing: %v", err)
	}
	if opts.Options["dns_server"] != "{10.0.0.2}" {
		t.Errorf("dns_server = %q, want {10.0.0.2}", opts.Options["dns_server"])
	}
}

func TestSubnetGatewayCIDR(t *testing.T) {
	cases := []struct {
		cidr   string
		wantIP string
		bits   int
	}{
		{"10.0.1.0/24", "10.0.1.1", 24},
		{"192.168.0.0/16", "192.168.0.1", 16},
		{"172.16.0.0/20", "172.16.0.1", 20},
		{"172.31.0.0/28", "172.31.0.1", 28},
	}
	for _, tc := range cases {
		t.Run(tc.cidr, func(t *testing.T) {
			gw, bits, err := SubnetGatewayCIDR(netip.MustParsePrefix(tc.cidr))
			if err != nil {
				t.Fatalf("SubnetGatewayCIDR: %v", err)
			}
			if gw != tc.wantIP || bits != tc.bits {
				t.Errorf("got (%q, %d), want (%q, %d)", gw, bits, tc.wantIP, tc.bits)
			}
		})
	}
}

func TestSubnetGatewayCIDR_IPv6Rejected(t *testing.T) {
	if _, _, err := SubnetGatewayCIDR(netip.MustParsePrefix("2001:db8::/32")); err == nil {
		t.Fatal("expected error for IPv6 prefix")
	}
}
