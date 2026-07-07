package gateway_ec2_capacityreservation

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// censusTimeout bounds the census-complete node-status fan-out. Unlike the
// latency-optimised queryNodeCapacity (which narrows after the first reply), we wait
// for every expected node or this full deadline so a "no node fits" answer is trustworthy.
const censusTimeout = 3 * time.Second

// nodeCensus is one node's capacity snapshot from spinifex.node.status: its id,
// availability zone, and per-instance-type available count.
type nodeCensus struct {
	NodeID    string
	AZ        string
	Available map[string]int
}

// collectCensus fans out spinifex.node.status and returns one entry per distinct
// node that responds, stopping once expectedNodes answer or censusTimeout elapses.
// It does not narrow the deadline after the first reply, trading latency for completeness.
func collectCensus(ctx context.Context, natsConn *nats.Conn, expectedNodes int, accountID string) ([]nodeCensus, error) {
	if expectedNodes < 1 {
		expectedNodes = 1
	}

	frames, _, err := utils.GatherCtx(ctx, natsConn, "spinifex.node.status", []byte("{}"),
		utils.GatherOpts{Timeout: censusTimeout, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var census []nodeCensus
	for _, frame := range frames {
		var status types.NodeStatusResponse
		if err := json.Unmarshal(frame, &status); err != nil {
			slog.DebugContext(ctx, "collectCensus: failed to unmarshal response", "err", err)
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

// azInCensus reports whether any node in the census lives in az. A requested AZ
// that no node reports is treated as unknown (InvalidAvailabilityZone).
func azInCensus(census []nodeCensus, az string) bool {
	for _, n := range census {
		if n.AZ == az {
			return true
		}
	}
	return false
}

// typeInCensus reports whether instanceType is in any node's catalog. Known types
// appear in node status even at zero available count, so an absent key means the type
// is unsupported (or GPU, excluded from schedulable capacity) — rejected as InvalidInstanceType.
func typeInCensus(census []nodeCensus, instanceType string) bool {
	for _, n := range census {
		if _, ok := n.Available[instanceType]; ok {
			return true
		}
	}
	return false
}
