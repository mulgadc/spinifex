package topology

import (
	"net/netip"

	networkids "github.com/mulgadc/spinifex/spinifex/domains/network/identifiers"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
)

// PeeringLinkSpec is a linked VPC peering connection at L2: the two VPCs and
// the link /30 addresses their routers' ports take.
type PeeringLinkSpec struct {
	PcxID         string
	RequesterVPC  string
	AccepterVPC   string
	RequesterCIDR netip.Prefix
	AccepterCIDR  netip.Prefix
	RequesterAddr netip.Addr
	AccepterAddr  netip.Addr
}

// PeeringSide is one VPC's view of a link: its own range, address and port,
// and the peer's port, range and address.
type PeeringSide struct {
	PcxID     string
	LocalVPC  string
	LocalCIDR netip.Prefix
	LocalAddr netip.Addr
	LocalPort string
	PeerPort  string
	PeerCIDR  netip.Prefix
	PeerAddr  netip.Addr
}

// Side returns vpcID's view of the link; ok is false when vpcID is neither end.
func (s PeeringLinkSpec) Side(vpcID string) (PeeringSide, bool) {
	reqPort, accPort := PeeringRouterPort(s.PcxID, s.RequesterVPC), PeeringRouterPort(s.PcxID, s.AccepterVPC)
	switch vpcID {
	case s.RequesterVPC:
		return PeeringSide{s.PcxID, vpcID, s.RequesterCIDR, s.RequesterAddr, reqPort, accPort, s.AccepterCIDR, s.AccepterAddr}, true
	case s.AccepterVPC:
		return PeeringSide{s.PcxID, vpcID, s.AccepterCIDR, s.AccepterAddr, accPort, reqPort, s.RequesterCIDR, s.RequesterAddr}, true
	}
	return PeeringSide{}, false
}

// Sides returns both ends' views of the link, requester first.
func (s PeeringLinkSpec) Sides() [2]PeeringSide {
	req, _ := s.Side(s.RequesterVPC)
	acc, _ := s.Side(s.AccepterVPC)
	return [2]PeeringSide{req, acc}
}

// PeeringLinkPort is side's router port on the link. It takes a hashed MAC of
// its name and names the peer's port as peer.
func PeeringLinkPort(side PeeringSide) nbdb.LogicalRouterPort {
	peer := side.PeerPort
	return nbdb.LogicalRouterPort{
		Name:     side.LocalPort,
		MAC:      networkids.HashMAC(side.LocalPort),
		Networks: []string{netip.PrefixFrom(side.LocalAddr, 30).String()},
		Peer:     &peer,
		ExternalIDs: map[string]string{
			ovn.ExternalIDPcxID: side.PcxID,
			"spinifex:vpc_id":   side.LocalVPC,
			ovn.ExternalIDRole:  "peering",
		},
	}
}
