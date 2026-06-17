package gateway_ec2_capacityreservation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// censusTimeout bounds the census-complete node-status fan-out. Unlike the
// latency-optimised queryNodeCapacity (which narrows to +200ms after the first
// reply), we wait for every expected node or this full deadline so that a "no
// node fits" answer is trustworthy rather than a race against a slow daemon.
const censusTimeout = 3 * time.Second

// nodeCensus is one node's capacity snapshot from spinifex.node.status: its id,
// availability zone, and per-instance-type available count.
type nodeCensus struct {
	NodeID    string
	AZ        string
	Available map[string]int
}

// collectCensus fans out spinifex.node.status and returns one entry per distinct
// node that responds, stopping once expectedNodes nodes have answered or
// censusTimeout elapses. It deliberately does not narrow the deadline after the
// first reply, trading latency for a complete cluster picture.
func collectCensus(natsConn *nats.Conn, expectedNodes int) ([]nodeCensus, error) {
	if expectedNodes < 1 {
		expectedNodes = 1
	}

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	pubMsg := nats.NewMsg("spinifex.node.status")
	pubMsg.Reply = inbox
	pubMsg.Data = []byte("{}")
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		return nil, fmt.Errorf("failed to publish node status request: %w", err)
	}

	deadline := time.Now().Add(censusTimeout)
	seen := make(map[string]struct{})
	var census []nodeCensus

	for len(seen) < expectedNodes {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				break
			}
			slog.Debug("collectCensus: error receiving message", "err", err)
			break
		}

		var status types.NodeStatusResponse
		if err := json.Unmarshal(msg.Data, &status); err != nil {
			slog.Debug("collectCensus: failed to unmarshal response", "err", err)
			continue
		}
		if status.Node == "" {
			continue
		}
		if _, dup := seen[status.Node]; dup {
			continue
		}
		seen[status.Node] = struct{}{}

		avail := make(map[string]int, len(status.InstanceTypes))
		for _, c := range status.InstanceTypes {
			avail[c.Name] = c.Available
		}
		census = append(census, nodeCensus{NodeID: status.Node, AZ: status.AZ, Available: avail})
	}

	return census, nil
}

// selectNode returns the id of the in-AZ node with the most available capacity
// for instanceType that can fit count instances, or "" if none qualifies. Ties
// are broken by the lowest node id for deterministic placement.
func selectNode(census []nodeCensus, az, instanceType string, count int) string {
	best := ""
	bestAvail := -1
	for _, n := range census {
		if n.AZ != az {
			continue
		}
		avail := n.Available[instanceType]
		if avail < count {
			continue
		}
		if avail > bestAvail || (avail == bestAvail && n.NodeID < best) {
			best = n.NodeID
			bestAvail = avail
		}
	}
	return best
}
