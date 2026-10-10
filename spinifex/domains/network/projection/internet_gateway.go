package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// AttachInternetGateway publishes vpc.igw-attach unacknowledged, asking
// vpcd to create the OVN external switch, gateway and SNAT. A failure is
// logged, not returned; the reconciler backstops a missing gateway.
func (c *Client) AttachInternetGateway(evt networkv1.InternetGatewayEvent) {
	c.announce(networkv1.InternetGatewayAttachSubject, evt)
}

// DetachInternetGateway publishes vpc.igw-detach unacknowledged, asking
// vpcd to clean up the OVN external switch, gateway and NAT.
func (c *Client) DetachInternetGateway(evt networkv1.InternetGatewayEvent) {
	c.announce(networkv1.InternetGatewayDetachSubject, evt)
}
