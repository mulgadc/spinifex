package projection

import networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"

// CreateSubnet publishes vpc.create-subnet unacknowledged. vpc.create and
// vpc.create-subnet have no ordering guarantee; vpcd's handler pre-ensures
// the VPC router itself, so a failure here is backstopped by the reconciler.
func (c *Client) CreateSubnet(evt networkv1.SubnetEvent) {
	c.announce(networkv1.SubnetCreateSubject, evt)
}

// DeleteSubnet publishes vpc.delete-subnet unacknowledged, for the same
// reason CreateSubnet is unacknowledged.
func (c *Client) DeleteSubnet(evt networkv1.SubnetEvent) {
	c.announce(networkv1.SubnetDeleteSubject, evt)
}
