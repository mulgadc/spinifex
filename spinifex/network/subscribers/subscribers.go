// Package subscribers translates VPC lifecycle NATS events into calls
// against the network/{topology,policy,external} managers. Each handler is
// a thin JSON-decode + manager-call adapter; ACL semantics, OVN object
// lifecycle, and convergence live in the managers themselves. Constructed
// eagerly with every dependency injected — no lazy *Once builders.
package subscribers

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/nats-io/nats.go"
)

// QueueGroup is the NATS queue group used for every VPC lifecycle topic.
// Membership is exclusive within the cluster: only one vpcd processes any
// given event, since all vpcds share the same OVN NB DB.
const QueueGroup = "vpcd-workers"

// Subscriber wires VPC lifecycle NATS topics to the network managers.
type Subscriber struct {
	topology topology.Manager
	sg       policy.SecurityGroupManager
	nat      policy.NATManager
	igw      external.IGWManager
}

// Config is the construction-time bag. Every field is required.
type Config struct {
	Topology topology.Manager
	SG       policy.SecurityGroupManager
	NAT      policy.NATManager
	IGW      external.IGWManager
}

// New constructs a Subscriber, returning an error when any manager is nil.
func New(cfg Config) (*Subscriber, error) {
	switch {
	case cfg.Topology == nil:
		return nil, errors.New("subscribers: Topology manager required")
	case cfg.SG == nil:
		return nil, errors.New("subscribers: SecurityGroupManager required")
	case cfg.NAT == nil:
		return nil, errors.New("subscribers: NATManager required")
	case cfg.IGW == nil:
		return nil, errors.New("subscribers: IGWManager required")
	}
	return &Subscriber{
		topology: cfg.Topology,
		sg:       cfg.SG,
		nat:      cfg.NAT,
		igw:      cfg.IGW,
	}, nil
}

// Subscribe registers NATS queue subscriptions for every VPC lifecycle
// topic. Cleanup on partial failure: previously-registered subscriptions
// are unsubscribed before the error is returned.
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
		{TopicAddNAT, s.handleAddNAT},
		{TopicDeleteNAT, s.handleDeleteNAT},
		{TopicAddNATGateway, s.handleAddNATGateway},
		{TopicDeleteNATGateway, s.handleDeleteNATGateway},
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
