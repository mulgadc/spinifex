// Package subscribers translates VPC lifecycle NATS events into calls
// against the network/{topology,policy,external} managers.
package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/nats-io/nats.go"
)

// QueueGroup ensures one vpcd processes each event (shared OVN NB DB).
const QueueGroup = "vpcd-workers"

// MACBindingFlusher removes stale SB MAC_Binding rows resolving a private IP so a
// reused address re-resolves to the new owner's MAC rather than the terminated
// instance's. Optional dependency; a nil flusher makes the flush a no-op.
type MACBindingFlusher interface {
	FlushMACBinding(ctx context.Context, ip string) error
}

// Subscriber wires VPC lifecycle NATS topics to the network managers.
type Subscriber struct {
	topology topology.Manager
	sg       policy.SecurityGroupManager
	eip      external.EIPManager
	natgw    external.NATGWManager
	igw      external.IGWManager
	mac      MACBindingFlusher
}

// Config: all manager fields required. MAC is optional (nil disables the flush).
type Config struct {
	Topology topology.Manager
	SG       policy.SecurityGroupManager
	EIP      external.EIPManager
	NATGW    external.NATGWManager
	IGW      external.IGWManager
	MAC      MACBindingFlusher
}

// New constructs a Subscriber, returning an error when any manager is nil.
func New(cfg Config) (*Subscriber, error) {
	switch {
	case cfg.Topology == nil:
		return nil, errors.New("subscribers: Topology manager required")
	case cfg.SG == nil:
		return nil, errors.New("subscribers: SecurityGroupManager required")
	case cfg.EIP == nil:
		return nil, errors.New("subscribers: EIPManager required")
	case cfg.NATGW == nil:
		return nil, errors.New("subscribers: NATGWManager required")
	case cfg.IGW == nil:
		return nil, errors.New("subscribers: IGWManager required")
	}
	return &Subscriber{
		topology: cfg.Topology,
		sg:       cfg.SG,
		eip:      cfg.EIP,
		natgw:    cfg.NATGW,
		igw:      cfg.IGW,
		mac:      cfg.MAC,
	}, nil
}

// addNATConcurrency bounds how many vpc.add-nat requests are in flight at once.
// Comfortably above a spread launch across a large cluster, low enough that a
// runaway caller cannot spawn unbounded goroutines against OVN.
const addNATConcurrency = 32

// concurrently lets a subscription overlap its handlers. NATS delivers to a
// subscription serially, which is right for handlers that return quickly and
// wrong for vpc.add-nat: it holds its reply across the OVN flows barrier, so a
// launch of N instances becomes N barriers back to back and the later callers
// pass their deadline and roll back. The barriers are independent — each one
// syncs after its own write — so overlapping them is safe and turns N barriers
// into roughly one. The semaphore is acquired on the delivery goroutine, so
// exceeding the limit applies backpressure rather than growing without bound.
func concurrently(limit int, h nats.MsgHandler) nats.MsgHandler {
	sem := make(chan struct{}, limit)
	return func(msg *nats.Msg) {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			h(msg)
		}()
	}
}

// Subscribe registers queue subs for every VPC lifecycle topic. On
// partial failure, unsubscribes prior subs before returning the error.
func (s *Subscriber) Subscribe(nc *nats.Conn) ([]*nats.Subscription, error) {
	type sub struct {
		topic   string
		handler nats.MsgHandler
	}
	subs := []sub{
		{TopicVPCCreate, s.handleVPCCreate},
		{TopicVPCDelete, s.handleVPCDelete},
		{TopicSubnetCreate, s.handleSubnetCreate},
		{TopicSubnetDelete, s.handleSubnetDelete},
		{TopicCreatePort, s.handleCreatePort},
		{TopicDeletePort, s.handleDeletePort},
		{TopicUpdatePortSGs, s.handleUpdatePortSGs},
		{TopicIGWAttach, s.handleIGWAttach},
		{TopicIGWDetach, s.handleIGWDetach},
		{TopicAddNAT, concurrently(addNATConcurrency, s.handleAddNAT)},
		{TopicDeleteNAT, s.handleDeleteNAT},
		{TopicAddNATGateway, s.handleAddNATGateway},
		{TopicDeleteNATGateway, s.handleDeleteNATGateway},
		{TopicAddIGWRoute, s.handleAddIGWRoute},
		{TopicDeleteIGWRoute, s.handleDeleteIGWRoute},
		{TopicGateSubnetEgress, s.handleGateSubnetEgress},
		{TopicUngateSubnetEgress, s.handleUngateSubnetEgress},
		{TopicAddSystemEgress, s.handleAddSystemEgress},
		{TopicDeleteSystemEgress, s.handleDeleteSystemEgress},
		{TopicCreateSG, s.handleCreateSG},
		{TopicDeleteSG, s.handleDeleteSG},
		{TopicUpdateSG, s.handleUpdateSG},
	}

	var result []*nats.Subscription
	for _, item := range subs {
		natsSub, err := nc.QueueSubscribe(item.topic, QueueGroup, item.handler)
		if err != nil {
			for _, r := range result {
				_ = r.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe %s: %w", item.topic, err)
		}
		result = append(result, natsSub)
		slog.Info("Subscribed to VPC topic", "topic", item.topic)
	}
	return result, nil
}
