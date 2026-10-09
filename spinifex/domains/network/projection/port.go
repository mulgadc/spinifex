package projection

import (
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
)

// vpcdPortEventTimeout bounds the synchronous vpcd round-trip for port events.
const vpcdPortEventTimeout = 5 * time.Second

// CreatePort requests vpcd create the logical switch port and join it to its
// security groups. Errors surface to the caller.
func (c *Client) CreatePort(evt networkv1.PortEvent) error {
	return c.request(networkv1.PortCreateSubject, evt, vpcdPortEventTimeout)
}

// DeletePort requests vpcd remove the logical switch port so a same-IP
// recreate cannot collide with a stale one. Callers treat a failure as
// non-fatal: the owning KV row is already gone, so this is best confirmed,
// not required, and the reconciler's orphan prune is the backstop.
func (c *Client) DeletePort(evt networkv1.PortEvent) error {
	return c.request(networkv1.PortDeleteSubject, evt, vpcdPortEventTimeout)
}

// UpdatePortSecurityGroups requests vpcd replace a port's declared security
// group membership. vpcd computes the OVN diff; errors surface to the caller.
func (c *Client) UpdatePortSecurityGroups(evt networkv1.PortSecurityGroupsUpdateEvent) error {
	return c.request(networkv1.PortSecurityGroupsUpdateSubject, evt, vpcdPortEventTimeout)
}
