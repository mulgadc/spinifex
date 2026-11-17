package topology

import (
	"fmt"
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

// PeeringSide is one VPC's view of a link: its own port, and the peer's
// range and address.
type PeeringSide struct {
	LocalVPC  string
	LocalCIDR netip.Prefix
	LocalPort string
	PeerCIDR  netip.Prefix
	PeerAddr  netip.Addr
}

// Side returns vpcID's view of the link; ok is false when vpcID is neither end.
func (s PeeringLinkSpec) Side(vpcID string) (PeeringSide, bool) {
	switch vpcID {
	case s.RequesterVPC:
		return PeeringSide{vpcID, s.RequesterCIDR, PeeringRouterPort(s.PcxID, vpcID), s.AccepterCIDR, s.AccepterAddr}, true
	case s.AccepterVPC:
		return PeeringSide{vpcID, s.AccepterCIDR, PeeringRouterPort(s.PcxID, vpcID), s.RequesterCIDR, s.RequesterAddr}, true
	}
	return PeeringSide{}, false
}

// PeeringLinkPorts returns the link's two router ports, requester first. Each
// takes a hashed MAC of its name and names the other as peer.
func PeeringLinkPorts(spec PeeringLinkSpec) ([2]nbdb.LogicalRouterPort, error) {
	if spec.PcxID == "" || spec.RequesterVPC == "" || spec.AccepterVPC == "" {
		return [2]nbdb.LogicalRouterPort{}, fmt.Errorf("peering link: connection and both VPCs required")
	}
	if !spec.RequesterAddr.Is4() || !spec.AccepterAddr.Is4() {
		return [2]nbdb.LogicalRouterPort{}, fmt.Errorf("peering link %s: both link addresses required", spec.PcxID)
	}
	reqName := PeeringRouterPort(spec.PcxID, spec.RequesterVPC)
	accName := PeeringRouterPort(spec.PcxID, spec.AccepterVPC)
	port := func(name, peer, vpcID string, addr netip.Addr) nbdb.LogicalRouterPort {
		return nbdb.LogicalRouterPort{
			Name:     name,
			MAC:      networkids.HashMAC(name),
			Networks: []string{netip.PrefixFrom(addr, 30).String()},
			Peer:     &peer,
			ExternalIDs: map[string]string{
				ovn.ExternalIDPcxID: spec.PcxID,
				"spinifex:vpc_id":   vpcID,
				"spinifex:role":     "peering",
			},
		}
	}
	return [2]nbdb.LogicalRouterPort{
		port(reqName, accName, spec.RequesterVPC, spec.RequesterAddr),
		port(accName, reqName, spec.AccepterVPC, spec.AccepterAddr),
	}, nil
}
