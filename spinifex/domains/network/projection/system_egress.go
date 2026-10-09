package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// RemoveSystemEgress publishes vpc.delete-system-egress unacknowledged,
// asking vpcd to remove the system-egress NAT for a control-plane ENI being
// torn down. A failure is logged, not returned.
//
// vpc.add-system-egress (AddSystemEgress) has a subscriber but no current
// publisher, so no method for it is added here.
func (c *Client) RemoveSystemEgress(evt networkv1.SystemEgressEvent) {
	c.announce(networkv1.SystemEgressDeleteSubject, evt)
}
