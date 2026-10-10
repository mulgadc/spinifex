package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// GateSubnetEgress publishes vpc.gate-subnet-egress unacknowledged, asking
// vpcd to install a DROP policy above both reroute priorities for a subnet
// that has no internet-bound route. A failure is logged, not returned.
func (c *Client) GateSubnetEgress(evt networkv1.SubnetEgressGateEvent) {
	c.announce(networkv1.SubnetEgressGateSubject, evt)
}

// UngateSubnetEgress publishes vpc.ungate-subnet-egress unacknowledged,
// asking vpcd to remove the DROP policy GateSubnetEgress installed.
func (c *Client) UngateSubnetEgress(evt networkv1.SubnetEgressUngateEvent) {
	c.announce(networkv1.SubnetEgressUngateSubject, evt)
}
