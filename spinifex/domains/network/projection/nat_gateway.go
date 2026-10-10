package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// AddNATGateway publishes vpc.add-nat-gateway unacknowledged, asking vpcd to
// install the NAT Gateway's SNAT rule and, when SubnetId is set, its
// per-subnet egress policy. A failure is logged, not returned.
func (c *Client) AddNATGateway(evt networkv1.NATGatewayEvent) {
	c.announce(networkv1.NATGatewayAddSubject, evt)
}

// RemoveNATGateway publishes vpc.delete-nat-gateway unacknowledged, asking
// vpcd to remove the NAT Gateway's SNAT rule and per-subnet egress policy.
func (c *Client) RemoveNATGateway(evt networkv1.NATGatewayEvent) {
	c.announce(networkv1.NATGatewayDeleteSubject, evt)
}
