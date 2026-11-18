package featurelevel

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

const (
	probeWait  = 3 * time.Second
	probeGrace = 150 * time.Millisecond
)

// StatusSubject is one daemon's read-only status route, addressed per node so a
// probe reaches exactly the node it asks about.
func StatusSubject(node string) string {
	return "cluster.v1.FeatureLevelStatus." + node
}

// ValidNode reports whether node is exactly one NATS subject token. Responder
// and probe both check it, so neither can address a wildcard or another node.
func ValidNode(node string) bool {
	return node != "" && !strings.ContainsAny(node, ".*>\t\n\r ")
}

// StatusRequest asks node for its feature-level status.
type StatusRequest struct {
	Node string `json:"node"`
}

// StatusReply is one node's status. Revision is the record revision the node
// read to answer, not one it holds from earlier. InFlight is the count of
// admitted callers per level and InFlightTotal their sum.
type StatusReply struct {
	Node          string      `json:"node"`
	Build         int         `json:"build"`
	Revision      uint64      `json:"revision"`
	InFlight      map[int]int `json:"in_flight"`
	InFlightTotal int         `json:"in_flight_total"`
}

// ServeStatus answers the status route for node. It re-reads the record on every
// request and refuses a request that names another node.
func (g *Gate) ServeStatus(node string, build int) func(context.Context, *StatusRequest, string) (*StatusReply, error) {
	return func(ctx context.Context, req *StatusRequest, _ string) (*StatusReply, error) {
		if g == nil || g.store == nil {
			return nil, fmt.Errorf("feature level gate on node %q is not ready", node)
		}
		if req.Node != node {
			return nil, fmt.Errorf("feature level status for node %q answered by node %q", req.Node, node)
		}
		_, rev, err := g.store.Read(ctx)
		if err != nil {
			return nil, err
		}
		in := g.InFlight()
		total := 0
		for _, c := range in {
			total += c
		}
		return &StatusReply{Node: node, Build: build, Revision: rev, InFlight: in, InFlightTotal: total}, nil
	}
}

// NodeStatus is one configured node's probe result. Reply is nil, with Reason
// set, when the node gave no usable answer.
type NodeStatus struct {
	Node   string
	Reply  *StatusReply
	Reason string
}

// Probe asks every configured node for its status, in parallel under one shared
// wait, so N nodes cost one wait. It never caches.
type Probe struct {
	nc      *nats.Conn
	nodeIDs []string
	wait    time.Duration
	grace   time.Duration
}

// NewProbe probes nodeIDs, the configured nodes.
func NewProbe(nc *nats.Conn, nodeIDs []string) *Probe {
	return &Probe{nc: nc, nodeIDs: slices.Sorted(slices.Values(nodeIDs)), wait: probeWait, grace: probeGrace}
}

// Probe returns one entry per configured node, sorted by node.
func (p *Probe) Probe(ctx context.Context) []NodeStatus {
	wait := p.wait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}
	results := make([]NodeStatus, len(p.nodeIDs))
	var wg sync.WaitGroup
	for i, node := range p.nodeIDs {
		wg.Go(func() {
			if wait <= 0 || ctx.Err() != nil {
				results[i] = NodeStatus{Node: node, Reason: "no-reply"}
				return
			}
			results[i] = p.probeNode(ctx, node, wait)
		})
	}
	wg.Wait()
	return results
}

func (p *Probe) probeNode(ctx context.Context, node string, wait time.Duration) NodeStatus {
	if !ValidNode(node) {
		return NodeStatus{Node: node, Reason: "invalid-node-id"}
	}
	data, err := json.Marshal(StatusRequest{Node: node})
	if err != nil {
		return NodeStatus{Node: node, Reason: "malformed"}
	}
	frames, sum, err := natsmsg.Gather(ctx, p.nc, StatusSubject(node), data, natsmsg.GatherOpts{
		Timeout: wait, ExpectedResponders: 1, Mode: natsmsg.CollectUntilDeadline, ResponderGrace: p.grace,
	})
	switch {
	case err != nil:
		return NodeStatus{Node: node, Reason: "transport"}
	case sum.CapHit, len(sum.ConflictNodes) > 0, sum.DuplicateFrames > 0:
		return NodeStatus{Node: node, Reason: "duplicate"}
	case sum.Unidentified > 0:
		return NodeStatus{Node: node, Reason: "wrong-identity"}
	case len(sum.ErrorResponders) > 0:
		return NodeStatus{Node: node, Reason: "error-reply"}
	case sum.Received == 0, len(frames) == 0:
		return NodeStatus{Node: node, Reason: "no-reply"}
	case frames[0].NodeID != node:
		return NodeStatus{Node: node, Reason: "wrong-identity"}
	}
	var reply StatusReply
	if json.Unmarshal(frames[0].Data, &reply) != nil {
		return NodeStatus{Node: node, Reason: "malformed"}
	}
	if reply.Node != node {
		return NodeStatus{Node: node, Reason: "wrong-identity"}
	}
	return NodeStatus{Node: node, Reply: &reply}
}
