package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// AddIGWRoute publishes vpc.add-igw-route unacknowledged, asking vpcd to
// install the egress policy routing destinationCidr to the internet gateway.
// A failure is logged, not returned; the reconciler backstops a missing route.
func (c *Client) AddIGWRoute(evt networkv1.IGWRouteEvent) {
	c.announce(networkv1.IGWRouteAddSubject, evt)
}

// RemoveIGWRoute publishes vpc.delete-igw-route unacknowledged, asking vpcd
// to remove the egress policy AddIGWRoute installed.
func (c *Client) RemoveIGWRoute(evt networkv1.IGWRouteEvent) {
	c.announce(networkv1.IGWRouteDeleteSubject, evt)
}
