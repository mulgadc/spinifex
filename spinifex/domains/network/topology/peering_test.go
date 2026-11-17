package topology

import (
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
)

func testPeeringSpec() PeeringLinkSpec {
	return PeeringLinkSpec{
		PcxID: "pcx-1", RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: netip.MustParsePrefix("10.0.0.0/16"), AccepterCIDR: netip.MustParsePrefix("10.1.0.0/16"),
		RequesterAddr: netip.MustParseAddr("169.254.192.1"), AccepterAddr: netip.MustParseAddr("169.254.192.2"),
	}
}

// The two ports name each other as peer, so OVN joins the routers directly.
func TestPeeringLinkPort_PeersEachOther(t *testing.T) {
	sides := testPeeringSpec().Sides()
	ports := [2]nbdb.LogicalRouterPort{PeeringLinkPort(sides[0]), PeeringLinkPort(sides[1])}
	if ports[0].Name != "pcx-pcx-1-vpc-a" || ports[1].Name != "pcx-pcx-1-vpc-b" {
		t.Fatalf("names = %q, %q", ports[0].Name, ports[1].Name)
	}
	if *ports[0].Peer != ports[1].Name || *ports[1].Peer != ports[0].Name {
		t.Errorf("peers = %q, %q", *ports[0].Peer, *ports[1].Peer)
	}
	if ports[0].Networks[0] != "169.254.192.1/30" || ports[1].Networks[0] != "169.254.192.2/30" {
		t.Errorf("networks = %v, %v", ports[0].Networks, ports[1].Networks)
	}
	if ports[0].MAC == ports[1].MAC {
		t.Errorf("both ends share MAC %s", ports[0].MAC)
	}
}

// Each side sees its own port and the other side's range and address.
func TestPeeringLinkSpec_Side(t *testing.T) {
	spec := testPeeringSpec()
	acc, ok := spec.Side("vpc-b")
	if !ok || acc.LocalPort != "pcx-pcx-1-vpc-b" || acc.PeerPort != "pcx-pcx-1-vpc-a" || acc.LocalAddr != spec.AccepterAddr ||
		acc.PeerCIDR != spec.RequesterCIDR || acc.PeerAddr != spec.RequesterAddr {
		t.Errorf("accepter side = %+v, %v", acc, ok)
	}
	if _, ok := spec.Side("vpc-c"); ok {
		t.Error("a third VPC has a side")
	}
}
