// Cluster-wide free capacity per instance type, for a client that needs the
// count rather than the catalogue.

package admin

import (
	"context"
	"encoding/json"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// GetInstanceTypeCapacityAction is open to any account. It reports how many
// instances of each type the cluster can still admit and names no resource, so
// a policy grants or denies it as a whole.
const GetInstanceTypeCapacityAction = "GetInstanceTypeCapacity"

// capacityCensusTimeout bounds the node-status fan-out. A short deadline is
// right here: the figure is advisory, and RunInstances is what decides.
const capacityCensusTimeout = 3 * time.Second

// GetInstanceTypeCapacityOutput is the response for GetInstanceTypeCapacity.
//
// Available is keyed by instance type name and holds the cluster-wide free slot
// count. A type the cluster supports but cannot currently admit is absent, so
// an absent key and a zero are the same answer.
type GetInstanceTypeCapacityOutput struct {
	Available map[string]int64 `json:"available"`
	Nodes     int              `json:"nodes"`
}

// GetInstanceTypeCapacity sums each node's per-type availability.
//
// The reply is bounded by the number of instance types, never by how much
// capacity is free. The same census is what the scheduler places against.
func GetInstanceTypeCapacity(ctx context.Context, nc *nats.Conn, expectedNodes int, accountID string) (*GetInstanceTypeCapacityOutput, error) {
	if expectedNodes < 1 {
		expectedNodes = 1
	}

	frames, _, err := natsmsg.Gather(ctx, nc, clusterv1.NodeStatusSubject, []byte("{}"),
		natsmsg.GatherOpts{Timeout: capacityCensusTimeout, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	available := make(map[string]int64)
	seen := make(map[string]bool, len(frames))
	for _, frame := range frames {
		var status clusterv1.NodeStatusResponse
		if err := json.Unmarshal(frame.Data, &status); err != nil {
			continue
		}
		// A node answering twice would double its capacity.
		if status.Node == "" || seen[status.Node] {
			continue
		}
		seen[status.Node] = true

		for _, c := range status.InstanceTypes {
			if c.Name == "" || c.Available <= 0 {
				continue
			}
			available[c.Name] += int64(c.Available)
		}
	}

	return &GetInstanceTypeCapacityOutput{Available: available, Nodes: len(seen)}, nil
}
