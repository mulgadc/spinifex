package topology

import (
	"net/netip"

	networkids "github.com/mulgadc/spinifex/spinifex/domains/network/identifiers"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
)

// TransitGatewayRouter is the OVN LR of a Transit Gateway.
func TransitGatewayRouter(tgwID string) string { return "tgw-" + tgwID }

// TransitGatewayVPCPort is the VPC router's port on an attachment's link.
func TransitGatewayVPCPort(attachmentID string) string { return "tgwv-" + attachmentID }

// TransitGatewayPort is the Transit Gateway router's port on an attachment's link.
func TransitGatewayPort(attachmentID string) string { return "tgwt-" + attachmentID }

// TransitGatewayLinkSpec is a realised attachment at L2: the VPC and Transit
// Gateway it links, the VPC's range, and the /30 addresses of the two ports.
type TransitGatewayLinkSpec struct {
	AttachmentID     string
	TransitGatewayID string
	VPCID            string
	VPCCIDR          netip.Prefix
	VPCAddr          netip.Addr
	TGWAddr          netip.Addr
}

// TransitGatewayVPCLinkPort is the VPC router's end of the link.
func TransitGatewayVPCLinkPort(spec TransitGatewayLinkSpec) nbdb.LogicalRouterPort {
	return transitGatewayLinkPort(spec, TransitGatewayVPCPort(spec.AttachmentID), TransitGatewayPort(spec.AttachmentID), spec.VPCAddr)
}

// TransitGatewayLinkPort is the Transit Gateway router's end of the link.
func TransitGatewayLinkPort(spec TransitGatewayLinkSpec) nbdb.LogicalRouterPort {
	return transitGatewayLinkPort(spec, TransitGatewayPort(spec.AttachmentID), TransitGatewayVPCPort(spec.AttachmentID), spec.TGWAddr)
}

func transitGatewayLinkPort(spec TransitGatewayLinkSpec, name, peer string, addr netip.Addr) nbdb.LogicalRouterPort {
	return nbdb.LogicalRouterPort{
		Name:     name,
		MAC:      networkids.HashMAC(name),
		Networks: []string{netip.PrefixFrom(addr, 30).String()},
		Peer:     &peer,
		ExternalIDs: map[string]string{
			ovn.ExternalIDTGWAttachmentID: spec.AttachmentID,
			ovn.ExternalIDTGWID:           spec.TransitGatewayID,
			"spinifex:vpc_id":             spec.VPCID,
			ovn.ExternalIDRole:            "tgw-link",
		},
	}
}
