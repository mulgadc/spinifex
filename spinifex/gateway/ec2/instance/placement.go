package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"

	"github.com/nats-io/nats.go"
)

// nodeAllocation tracks how many instances to launch on a specific node.
type nodeAllocation struct {
	NodeID    string
	Available int // capacity for the requested instance type
	Assigned  int // instances assigned to this node
}

// distributeInstances implements the best-effort spread algorithm for multi-node
// instance distribution. It queries cluster capacity, filters eligible nodes,
// distributes instances across nodes (1 per node first, then pack extras onto
// nodes with most remaining capacity), launches in parallel, and handles
// partial failures with rollback.
//
// Returns the merged reservation on success or an error.
func distributeInstances(input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	// Step 1: Query capacity from all nodes via fan-out
	nodes, err := queryNodeCapacity(natsConn, instanceType)
	if err != nil {
		return nil, err
	}

	if len(nodes) == 0 {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Step 2: Calculate total capacity and check feasibility
	totalCapacity := 0
	for _, n := range nodes {
		totalCapacity += n.Available
	}
	if totalCapacity < minCount {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Step 3: Determine launch count (capped to MaxCount and available capacity)
	launchCount := min(maxCount, totalCapacity)

	// Step 4: Distribute instances across nodes using best-effort spread
	allocations := spreadAllocate(nodes, launchCount)

	// Step 5: Launch instances on each node in parallel
	results := launchOnNodes(allocations, input, natsConn, accountID)

	// Step 6: Aggregate results and handle partial failure
	return aggregateResults(results, minCount, natsConn, accountID)
}

// queryNodeCapacity fans out spinifex.node.status to all daemons and returns
// eligible nodes (those with Available >= 1 for the requested instance type),
// sorted by available capacity descending with random tiebreaking for fair
// distribution among equal-capacity nodes.
//
// Uses a collection window: after the first response arrives, only waits an
// additional 200ms for remaining responses (instead of the full 3s timeout).
func queryNodeCapacity(natsConn *nats.Conn, instanceType string) ([]nodeAllocation, error) {
	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg("spinifex.node.status")
	pubMsg.Reply = inbox
	pubMsg.Data = []byte("{}")
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		return nil, fmt.Errorf("failed to publish node status request: %w", err)
	}

	const (
		initialTimeout = 3 * time.Second
		collectWindow  = 200 * time.Millisecond
	)

	deadline := time.Now().Add(initialTimeout)
	gotFirst := false
	var nodes []nodeAllocation

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				break
			}
			slog.Debug("queryNodeCapacity: error receiving message", "err", err)
			break
		}

		var status types.NodeStatusResponse
		if err := json.Unmarshal(msg.Data, &status); err != nil {
			slog.Debug("queryNodeCapacity: failed to unmarshal response", "err", err)
			continue
		}
		if status.Node == "" {
			slog.Debug("queryNodeCapacity: skipping response with empty node ID")
			continue
		}

		// Find capacity for the requested instance type on this node
		for _, cap := range status.InstanceTypes {
			if cap.Name == instanceType && cap.Available >= 1 {
				nodes = append(nodes, nodeAllocation{
					NodeID:    status.Node,
					Available: cap.Available,
				})
				break
			}
		}

		// After the first valid response, tighten the deadline so we don't
		// wait the full 3s for stragglers.
		if !gotFirst {
			gotFirst = true
			collectDeadline := time.Now().Add(collectWindow)
			if collectDeadline.Before(deadline) {
				deadline = collectDeadline
			}
		}
	}

	// Shuffle first for random tiebreaking, then stable-sort by capacity
	// descending. This ensures fair distribution among equal-capacity nodes.
	rand.Shuffle(len(nodes), func(i, j int) {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	})
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].Available > nodes[j].Available
	})

	return nodes, nil
}

// spreadAllocate distributes count instances across nodes using best-effort spread:
//   - Round 1: assign 1 instance to each node (up to count)
//   - Round 2+: assign remaining to nodes with most remaining capacity
func spreadAllocate(nodes []nodeAllocation, count int) []nodeAllocation {
	// Make a working copy
	allocs := make([]nodeAllocation, len(nodes))
	copy(allocs, nodes)

	remaining := count

	// Round 1: 1 instance per node
	for i := range allocs {
		if remaining <= 0 {
			break
		}
		allocs[i].Assigned = 1
		remaining--
	}

	// Round 2+: pack remaining onto nodes with most remaining capacity.
	// Ties are broken by preferring the node with fewer assigned instances,
	// which produces a more balanced spread.
	for remaining > 0 {
		bestIdx := -1
		bestRemaining := 0
		for i, a := range allocs {
			nodeRemaining := a.Available - a.Assigned
			if nodeRemaining > bestRemaining {
				bestRemaining = nodeRemaining
				bestIdx = i
			} else if nodeRemaining == bestRemaining && nodeRemaining > 0 &&
				(bestIdx < 0 || a.Assigned < allocs[bestIdx].Assigned) {
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break // no more capacity anywhere
		}
		allocs[bestIdx].Assigned++
		remaining--
	}

	// Filter out nodes with no assignments
	result := make([]nodeAllocation, 0, len(allocs))
	for _, a := range allocs {
		if a.Assigned > 0 {
			result = append(result, a)
		}
	}
	return result
}

// nodeLaunchResult holds the outcome of launching instances on a single node.
type nodeLaunchResult struct {
	NodeID      string
	Reservation *ec2.Reservation
	Err         error
}

// launchOnNodes sends targeted RunInstances requests to specific nodes in parallel.
// Each node gets MinCount=MaxCount=assignedCount so the daemon treats it as all-or-nothing.
func launchOnNodes(allocations []nodeAllocation, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string) []nodeLaunchResult {
	instanceType := aws.StringValue(input.InstanceType)

	results := make([]nodeLaunchResult, len(allocations))
	var wg sync.WaitGroup

	for i, alloc := range allocations {
		wg.Add(1)
		go func(idx int, a nodeAllocation) {
			defer wg.Done()

			// Build per-node input with MinCount=MaxCount=assigned
			nodeInput := *input
			nodeInput.MinCount = aws.Int64(int64(a.Assigned))
			nodeInput.MaxCount = aws.Int64(int64(a.Assigned))

			topic := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, a.NodeID)
			reservation, err := utils.NATSRequest[ec2.Reservation](natsConn, topic, &nodeInput, 5*time.Minute, accountID)
			if err != nil {
				results[idx] = nodeLaunchResult{NodeID: a.NodeID, Err: fmt.Errorf("launch on %s: %w", a.NodeID, err)}
				return
			}
			results[idx] = nodeLaunchResult{NodeID: a.NodeID, Reservation: reservation}
		}(i, alloc)
	}

	wg.Wait()
	return results
}

// aggregateResults merges successful node launches into a single reservation.
// If total launched instances < minCount, all successfully launched instances
// are terminated (rollback) and InsufficientInstanceCapacity is returned.
func aggregateResults(results []nodeLaunchResult, minCount int, natsConn *nats.Conn, accountID string) (*ec2.Reservation, error) {
	var allInstances []*ec2.Instance
	var reservationID *string

	for _, r := range results {
		if r.Err != nil {
			slog.Warn("distributeInstances: node launch failed", "node", r.NodeID, "err", r.Err)
			continue
		}
		if r.Reservation != nil {
			allInstances = append(allInstances, r.Reservation.Instances...)
			if reservationID == nil {
				reservationID = r.Reservation.ReservationId
			}
		}
	}

	totalLaunched := len(allInstances)

	if totalLaunched < minCount {
		// Rollback: terminate all successfully launched instances
		if totalLaunched > 0 {
			rollbackInstances(allInstances, natsConn, accountID)
		}
		// Propagate specific client errors (e.g. InvalidAMIID.NotFound) instead
		// of the generic InsufficientInstanceCapacity.
		if clientErr := extractClientError(results); clientErr != nil {
			return nil, clientErr
		}
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	return &ec2.Reservation{
		ReservationId: reservationID,
		Instances:     allInstances,
	}, nil
}

// distributeInstancesSpread implements strict 1-per-node spread for placement groups.
// It queries capacity, reserves unused nodes via CAS, launches 1 instance per node,
// and finalizes or rolls back the placement group record.
func distributeInstancesSpread(input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string, groupName string) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)

	// Step 1: Query capacity from all nodes
	nodes, err := queryNodeCapacity(natsConn, instanceType)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Build list of eligible node IDs
	eligibleNodeIDs := make([]string, len(nodes))
	for i, n := range nodes {
		eligibleNodeIDs[i] = n.NodeID
	}

	// Step 2: Reserve nodes atomically (CAS-based, handles concurrent launches)
	reserveOut, err := pgSvc.ReserveSpreadNodes(&handlers_ec2_placementgroup.ReserveSpreadNodesInput{
		GroupName:     groupName,
		EligibleNodes: eligibleNodeIDs,
		MinCount:      minCount,
		MaxCount:      maxCount,
	}, accountID)
	if err != nil {
		return nil, err
	}

	reservedNodes := reserveOut.ReservedNodes

	// Step 3: Build allocations (1 instance per reserved node)
	allocations := make([]nodeAllocation, len(reservedNodes))
	for i, nodeID := range reservedNodes {
		allocations[i] = nodeAllocation{NodeID: nodeID, Assigned: 1}
	}

	// Step 4: Launch instances on reserved nodes in parallel
	results := launchOnNodes(allocations, input, natsConn, accountID)

	// Step 5: Collect results
	var allInstances []*ec2.Instance
	var reservationID *string
	nodeInstances := make(map[string][]string)
	var failedNodes []string

	for _, r := range results {
		if r.Err != nil {
			slog.Warn("distributeInstancesSpread: node launch failed", "node", r.NodeID, "err", r.Err)
			failedNodes = append(failedNodes, r.NodeID)
			continue
		}
		if r.Reservation != nil {
			for _, inst := range r.Reservation.Instances {
				allInstances = append(allInstances, inst)
				if inst.InstanceId != nil {
					nodeInstances[r.NodeID] = append(nodeInstances[r.NodeID], *inst.InstanceId)
				}
			}
			if reservationID == nil {
				reservationID = r.Reservation.ReservationId
			}
		}
	}

	totalLaunched := len(allInstances)

	if totalLaunched < minCount {
		// Rollback: terminate launched instances and release all reserved nodes
		if totalLaunched > 0 {
			rollbackInstances(allInstances, natsConn, accountID)
		}
		// Release all reserved nodes from the placement group
		if _, err := pgSvc.ReleaseSpreadNodes(&handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     reservedNodes,
		}, accountID); err != nil {
			slog.Error("distributeInstancesSpread: failed to release nodes after rollback", "err", err)
		}
		if clientErr := extractClientError(results); clientErr != nil {
			return nil, clientErr
		}
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Step 6: Finalize — replace placeholders with actual instance IDs.
	// If finalization fails, the placement group record has stale placeholders;
	// roll back the launched instances to avoid untracked instances.
	if _, err := pgSvc.FinalizeSpreadInstances(&handlers_ec2_placementgroup.FinalizeSpreadInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, accountID); err != nil {
		slog.Error("distributeInstancesSpread: finalize failed, rolling back instances", "err", err)
		rollbackInstances(allInstances, natsConn, accountID)
		// Release all reserved nodes (both succeeded and failed)
		allReleaseNodes := append(reservedNodes[:0:0], reservedNodes...)
		if _, releaseErr := pgSvc.ReleaseSpreadNodes(&handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     allReleaseNodes,
		}, accountID); releaseErr != nil {
			slog.Error("distributeInstancesSpread: failed to release nodes after finalize rollback", "err", releaseErr)
		}
		return nil, fmt.Errorf("failed to finalize placement group record: %w", err)
	}

	// Release any failed nodes that didn't launch
	if len(failedNodes) > 0 {
		if _, err := pgSvc.ReleaseSpreadNodes(&handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     failedNodes,
		}, accountID); err != nil {
			slog.Error("distributeInstancesSpread: failed to release failed nodes", "err", err)
		}
	}

	return &ec2.Reservation{
		ReservationId: reservationID,
		Instances:     allInstances,
	}, nil
}

// distributeInstancesCluster implements cluster placement group routing.
// All instances are pinned to a single node. If the group already has instances,
// subsequent launches go to the same node. If empty, picks the node with most capacity.
func distributeInstancesCluster(input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string, groupName string) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)

	// Step 1: Query capacity from all nodes
	nodes, err := queryNodeCapacity(natsConn, instanceType)
	if err != nil {
		return nil, err
	}

	// Build eligible node IDs (already sorted by capacity desc)
	eligibleNodeIDs := make([]string, len(nodes))
	for i, n := range nodes {
		eligibleNodeIDs[i] = n.NodeID
	}

	// Step 2: Reserve the cluster target node (CAS-based for empty groups)
	reserveOut, err := pgSvc.ReserveClusterNode(&handlers_ec2_placementgroup.ReserveClusterNodeInput{
		GroupName:     groupName,
		EligibleNodes: eligibleNodeIDs,
	}, accountID)
	if err != nil {
		return nil, err
	}

	targetNode := reserveOut.TargetNode

	// Step 3: Check capacity on the target node
	var targetCapacity int
	for _, n := range nodes {
		if n.NodeID == targetNode {
			targetCapacity = n.Available
			break
		}
	}

	if targetCapacity < minCount {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Cap launch count to available capacity and MaxCount
	launchCount := min(maxCount, targetCapacity)

	// Step 4: Targeted launch — all instances to the pinned node
	allocations := []nodeAllocation{{
		NodeID:   targetNode,
		Assigned: launchCount,
	}}
	results := launchOnNodes(allocations, input, natsConn, accountID)

	// Step 5: Handle result (single node, no partial failure logic needed)
	if results[0].Err != nil {
		if clientErr := extractClientError(results); clientErr != nil {
			return nil, clientErr
		}
		return nil, results[0].Err
	}

	reservation := results[0].Reservation
	if reservation == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Step 6: CAS-update record with launched instance IDs
	nodeInstances := make(map[string][]string)
	for _, inst := range reservation.Instances {
		if inst.InstanceId != nil {
			nodeInstances[targetNode] = append(nodeInstances[targetNode], *inst.InstanceId)
		}
	}

	if _, err := pgSvc.FinalizeClusterInstances(&handlers_ec2_placementgroup.FinalizeClusterInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, accountID); err != nil {
		slog.Error("distributeInstancesCluster: finalize failed, rolling back instances", "err", err)
		rollbackInstances(reservation.Instances, natsConn, accountID)
		return nil, fmt.Errorf("failed to finalize cluster placement group record: %w", err)
	}

	return reservation, nil
}

// extractClientError scans node launch results for specific client validation
// errors (e.g. InvalidAMIID.NotFound) that should be propagated instead of the
// generic InsufficientInstanceCapacity. Returns the first match, or nil.
func extractClientError(results []nodeLaunchResult) error {
	for _, r := range results {
		if r.Err == nil {
			continue
		}
		inner := errors.Unwrap(r.Err)
		if inner == nil {
			continue
		}
		switch inner.Error() {
		case awserrors.ErrorInvalidAMIIDNotFound,
			awserrors.ErrorInvalidAMIIDMalformed,
			awserrors.ErrorInvalidAMIIDUnavailable,
			awserrors.ErrorInvalidKeyPairNotFound,
			awserrors.ErrorMissingParameter,
			awserrors.ErrorInvalidGroupNotFound,
			awserrors.ErrorInvalidParameterValue,
			awserrors.ErrorSecurityGroupsPerInterfaceLimitExceeded:
			return inner
		}
	}
	return nil
}

// rollbackInstances terminates all instances from a failed multi-node launch.
func rollbackInstances(instances []*ec2.Instance, natsConn *nats.Conn, accountID string) {
	var ids []*string
	for _, inst := range instances {
		if inst.InstanceId != nil {
			ids = append(ids, inst.InstanceId)
		}
	}
	if len(ids) == 0 {
		return
	}

	slog.Warn("distributeInstances: rolling back launched instances", "count", len(ids))
	_, err := TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: ids,
	}, natsConn, accountID)
	if err != nil {
		slog.Error("distributeInstances: rollback failed", "err", err)
	}
}
