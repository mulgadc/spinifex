package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// CreateVPC publishes vpc.create unacknowledged. A failure is logged, not
// returned: the VPC's KV record is already committed, and the reconciler
// backstops any OVN router that never gets created.
func (c *Client) CreateVPC(evt networkv1.VPCEvent) {
	c.announce(networkv1.VPCCreateSubject, evt)
}

// DeleteVPC publishes vpc.delete unacknowledged, for the same reason
// CreateVPC is unacknowledged: the KV record is already gone, and the
// reconciler backstops OVN garbage collection.
func (c *Client) DeleteVPC(evt networkv1.VPCEvent) {
	c.announce(networkv1.VPCDeleteSubject, evt)
}
