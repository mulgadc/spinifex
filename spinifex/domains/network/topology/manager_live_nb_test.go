package topology_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/ovntest"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// Two VPCs in different accounts commonly share a subnet CIDR (every default
// subnet is 172.31.0.0/20). Deleting one subnet, repeatedly, must never touch
// the other subnet's DHCP_Options row or the LSP reference to it.
func TestLiveManager_DeleteSubnet_SameCIDRLeavesOtherSubnetDHCP(t *testing.T) {
	nb := ovntest.StartNB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("LiveClient.Connect: %v", err)
	}
	t.Cleanup(cli.Close)
	mgr := topology.NewLiveManager(cli)

	vpcCIDR := netip.MustParsePrefix("172.31.0.0/16")
	subCIDR := netip.MustParsePrefix("172.31.0.0/20")

	victim := topology.SubnetSpec{SubnetID: "subnet-bbbb", VPCID: "vpc-bbbb", CIDR: subCIDR}
	if err := mgr.EnsureVPC(ctx, topology.VPCSpec{VPCID: victim.VPCID, CIDR: vpcCIDR}); err != nil {
		t.Fatalf("EnsureVPC victim: %v", err)
	}
	if err := mgr.EnsureSubnet(ctx, victim); err != nil {
		t.Fatalf("EnsureSubnet victim: %v", err)
	}
	if err := mgr.EnsureSGPortGroup(ctx, "sg-bbbb"); err != nil {
		t.Fatalf("EnsureSGPortGroup: %v", err)
	}
	mac, _ := net.ParseMAC("02:00:00:00:bb:01")
	port := topology.PortSpec{
		PortID: "eni-bbbb", SubnetID: victim.SubnetID, VPCID: victim.VPCID,
		PrivateIP: netip.MustParseAddr("172.31.0.10"), MAC: mac, SGIDs: []string{"sg-bbbb"},
	}
	if err := mgr.EnsurePort(ctx, port); err != nil {
		t.Fatalf("EnsurePort: %v", err)
	}
	victimRow, err := cli.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", victim.SubnetID)
	if err != nil {
		t.Fatalf("victim DHCP options: %v", err)
	}

	if err := mgr.EnsureVPC(ctx, topology.VPCSpec{VPCID: "vpc-aaaa", CIDR: vpcCIDR}); err != nil {
		t.Fatalf("EnsureVPC other: %v", err)
	}
	for i := range 10 {
		other := topology.SubnetSpec{SubnetID: "subnet-aaaa" + string(rune('0'+i)), VPCID: "vpc-aaaa", CIDR: subCIDR}
		if err := mgr.EnsureSubnet(ctx, other); err != nil {
			t.Fatalf("cycle %d EnsureSubnet: %v", i, err)
		}
		if err := mgr.DeleteSubnet(ctx, other); err != nil {
			t.Fatalf("cycle %d DeleteSubnet: %v", i, err)
		}

		rows, err := cli.ListDHCPOptions(ctx)
		if err != nil {
			t.Fatalf("ListDHCPOptions: %v", err)
		}
		if len(rows) != 1 || rows[0].UUID != victimRow.UUID {
			owners := make([]string, 0, len(rows))
			for _, r := range rows {
				owners = append(owners, r.ExternalIDs["spinifex:subnet_id"])
			}
			t.Fatalf("cycle %d: DHCP_Options rows owned by %v, want only %s", i, owners, victim.SubnetID)
		}
		lsp, err := cli.GetLogicalSwitchPort(ctx, topology.Port(port.PortID))
		if err != nil {
			t.Fatalf("GetLogicalSwitchPort: %v", err)
		}
		if lsp.DHCPv4Options == nil || *lsp.DHCPv4Options != victimRow.UUID {
			t.Fatalf("cycle %d: victim LSP dhcpv4_options = %v, want %s", i, lsp.DHCPv4Options, victimRow.UUID)
		}
	}
}
