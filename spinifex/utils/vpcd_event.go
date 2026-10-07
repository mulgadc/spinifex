package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// natEvent is the wire payload for vpc.add-nat / vpc.delete-nat.
type natEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"`
	MAC        string `json:"mac"`
}

// addNATTimeout bounds the vpc.add-nat request-reply. vpcd's handler holds the
// reply until the flows barrier (ovn-nbctl --wait=hv sync, bounded 30 s) confirms
// every chassis realised the rule. On a cold multi-node cluster the gateway SB
// chassisredirect binding can take most of that budget to converge, so the caller
// deadline must exceed 30 s or it abandons a still-committing rule and rolls back
// the EIP mid-launch — surfacing as a spurious ServerInternal on RunInstances.
const addNATTimeout = 45 * time.Second

// AddNAT requests vpcd commit the OVN dnat_and_snat rule via NATS request-reply.
// A non-nil return means the rule may not be committed; callers must roll back and publish vpc.delete-nat.
func AddNAT(nc *nats.Conn, vpcID, externalIP, logicalIP, portName, mac string) error {
	return RequestEvent(nc, "vpc.add-nat", natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}, addNATTimeout)
}

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

// PublishNATEvent sends a NAT lifecycle event. Both topics use request-reply:
// vpc.add-nat to prevent ARP races, vpc.delete-nat so an undelivered teardown is
// reported rather than lost. Neither is fatal to the caller — the reconciler's
// orphan sweep is the backstop. Use AddNAT directly when failure must trigger a
// rollback.
func PublishNATEvent(nc *nats.Conn, topic, vpcID, externalIP, logicalIP, portName, mac string) {
	evt := natEvent{
		VpcId: vpcID, ExternalIP: externalIP, LogicalIP: logicalIP,
		PortName: portName, MAC: mac,
	}

	switch topic {
	case "vpc.add-nat":
		if err := RequestEvent(nc, topic, evt, addNATTimeout); err != nil {
			slog.Warn("PublishNATEvent: failed to add NAT rule — OVN dnat_and_snat rule not created; restart vpcd or re-associate EIP to recover",
				"topic", topic, "externalIP", externalIP, "logicalIP", logicalIP, "err", err)
		}
	case "vpc.delete-nat":
		if err := deleteNAT(nc, evt); err != nil {
			slog.Error("PublishNATEvent: failed to delete NAT rule — the host route and proxy-ARP for this address may still deliver to its old guest; the reconciler's orphan sweep is the remaining repair",
				"topic", topic, "externalIP", externalIP, "logicalIP", logicalIP, "err", err)
		}
	default:
		natsmsg.PublishEvent(nc, topic, evt)
	}
}

// deleteNAT requests the teardown, retrying only while nothing is subscribed.
func deleteNAT(nc *nats.Conn, evt natEvent) error {
	var err error
	for attempt := range deleteNATRetries {
		if attempt > 0 {
			time.Sleep(deleteNATRetryDelay)
		}
		if err = RequestEvent(nc, "vpc.delete-nat", evt, deleteNATTimeout); err == nil {
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

// RequestEvent marshals event as JSON and sends a synchronous NATS request, blocking until the subscriber acks.
func RequestEvent(nc *nats.Conn, topic string, event any, timeout time.Duration) error {
	if nc == nil {
		return fmt.Errorf("%s: nats connection not initialized", topic)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", topic, err)
	}
	resp, err := nc.Request(topic, data, timeout)
	if err != nil {
		return fmt.Errorf("%s request: %w", topic, err)
	}
	// vpcd responds with {"success":true} or {"success":false,"error":"..."}.
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if jsonErr := json.Unmarshal(resp.Data, &result); jsonErr != nil {
		return fmt.Errorf("%s: unmarshal response: %w", topic, jsonErr)
	}
	if !result.Success {
		return fmt.Errorf("%s: %s", topic, result.Error)
	}
	return nil
}
