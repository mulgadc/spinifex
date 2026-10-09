package projection

import (
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
)

// vpcdSGEventTimeout bounds the synchronous vpcd round-trip for SG events.
const vpcdSGEventTimeout = 5 * time.Second

// CreateSecurityGroup requests vpcd ensure the SG's port group and apply its
// ACL set. Errors surface to the caller.
func (c *Client) CreateSecurityGroup(evt networkv1.SecurityGroupEvent) error {
	return c.request(networkv1.SecurityGroupCreateSubject, evt, vpcdSGEventTimeout)
}

// DeleteSecurityGroup requests vpcd remove the SG's port group and ACLs.
// Idempotent; errors surface to the caller.
func (c *Client) DeleteSecurityGroup(evt networkv1.SecurityGroupEvent) error {
	return c.request(networkv1.SecurityGroupDeleteSubject, evt, vpcdSGEventTimeout)
}

// UpdateSecurityGroup requests vpcd replace the SG's ACL set; the port group
// is unaffected. Errors surface to the caller.
func (c *Client) UpdateSecurityGroup(evt networkv1.SecurityGroupEvent) error {
	return c.request(networkv1.SecurityGroupUpdateSubject, evt, vpcdSGEventTimeout)
}
