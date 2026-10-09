package projection

import (
	"errors"
	"log/slog"
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/nats-io/nats.go"
)

// addNATTimeout bounds the vpc.add-nat request-reply. vpcd's handler holds the
// reply until the flows barrier (ovn-nbctl --wait=hv sync, bounded 30 s) confirms
// every chassis realised the rule. On a cold multi-node cluster the gateway SB
// chassisredirect binding can take most of that budget to converge, so the caller
// deadline must exceed 30 s or it abandons a still-committing rule and rolls back
// the EIP mid-launch — surfacing as a spurious ServerInternal on RunInstances.
const addNATTimeout = 45 * time.Second

// deleteNATTimeout bounds the vpc.delete-nat request-reply. The handler deletes
// the OVN row and unbinds host plumbing, each under its own 10 s context, so the
// ceiling is above both. It is a ceiling and not a cost: the reply normally lands
// in milliseconds.
const deleteNATTimeout = 15 * time.Second

// A teardown published while vpcd is restarting has no subscriber and is dropped
// on the floor, leaving a /32 host route delivering a released address to the
// guest that used to hold it. The gap is short — a restart resubscribes in a few
// hundred milliseconds — so a small bounded retry closes it. Only ErrNoResponders
// is retried, and that returns immediately, so the added latency is bounded by
// the delay and paid only when nothing is listening.
const deleteNATRetries = 3

// deleteNATRetryDelay is a var only so tests can shorten the gap between attempts.
var deleteNATRetryDelay = 500 * time.Millisecond

func natEvent(vpcID, externalIP, logicalIP, portName, mac string) networkv1.NATEvent {
	return networkv1.NATEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}
}

// AddNAT requests vpcd commit the OVN dnat_and_snat rule via NATS request-reply.
// A non-nil return means the rule may not be committed; callers must roll back
// and call RemoveNAT or AnnounceNATDelete.
func (c *Client) AddNAT(vpcID, externalIP, logicalIP, portName, mac string) error {
	return c.request(networkv1.NATAddSubject, natEvent(vpcID, externalIP, logicalIP, portName, mac), addNATTimeout)
}

// AddNATBestEffort requests the same add-nat rule as AddNAT but never fails
// the caller: a failure is logged, and the reconciler's orphan sweep is the
// backstop. Use AddNAT directly when failure must trigger a rollback.
func (c *Client) AddNATBestEffort(vpcID, externalIP, logicalIP, portName, mac string) {
	evt := natEvent(vpcID, externalIP, logicalIP, portName, mac)
	if err := c.request(networkv1.NATAddSubject, evt, addNATTimeout); err != nil {
		slog.Warn("AddNATBestEffort: failed to add NAT rule — OVN dnat_and_snat rule not created; restart vpcd or re-associate EIP to recover",
			"topic", networkv1.NATAddSubject, "externalIP", externalIP, "logicalIP", logicalIP, "err", err)
	}
}

// AnnounceNATAdd publishes vpc.add-nat unacknowledged: it bypasses the flows
// barrier AddNAT and AddNATBestEffort wait on, so the caller returns before
// vpcd confirms the rule. It exists because EIP's AssociateAddress path must
// not inherit that latency; the reconciler's orphan sweep is the backstop.
func (c *Client) AnnounceNATAdd(vpcID, externalIP, logicalIP, portName, mac string) {
	c.announce(networkv1.NATAddSubject, natEvent(vpcID, externalIP, logicalIP, portName, mac))
}

// RemoveNAT requests vpcd remove the OVN dnat_and_snat rule via NATS
// request-reply, retrying only while nothing is subscribed (ErrNoResponders),
// so a teardown published during a vpcd restart is not silently dropped. It
// never fails the caller: a failure is logged, and the reconciler's orphan
// sweep is the remaining repair.
func (c *Client) RemoveNAT(vpcID, externalIP, logicalIP, portName, mac string) {
	evt := natEvent(vpcID, externalIP, logicalIP, portName, mac)
	if err := c.removeNAT(evt); err != nil {
		slog.Error("RemoveNAT: failed to delete NAT rule — the host route and proxy-ARP for this address may still deliver to its old guest; the reconciler's orphan sweep is the remaining repair",
			"topic", networkv1.NATDeleteSubject, "externalIP", externalIP, "logicalIP", logicalIP, "err", err)
	}
}

func (c *Client) removeNAT(evt networkv1.NATEvent) error {
	var err error
	for attempt := range deleteNATRetries {
		if attempt > 0 {
			time.Sleep(deleteNATRetryDelay)
		}
		if err = c.request(networkv1.NATDeleteSubject, evt, deleteNATTimeout); err == nil {
			return nil
		}
		if !errors.Is(err, nats.ErrNoResponders) {
			return err
		}
		slog.Warn("vpc.delete-nat has no subscriber; retrying",
			"externalIP", evt.ExternalIP, "attempt", attempt+1, "attempts", deleteNATRetries)
	}
	return err
}

// AnnounceNATDelete publishes vpc.delete-nat unacknowledged: no ack, no
// retry. It exists because DeleteNetworkInterface tears down an ENI whose KV
// row is already gone, so there is nothing left to roll back to; the
// reconciler's orphan sweep is the backstop for a dropped teardown.
func (c *Client) AnnounceNATDelete(vpcID, externalIP, logicalIP, portName, mac string) {
	c.announce(networkv1.NATDeleteSubject, natEvent(vpcID, externalIP, logicalIP, portName, mac))
}
