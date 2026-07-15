package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

// distributeInstances spreads instances across nodes: queries capacity, allocates
// (1 per node first, then packs extras by remaining capacity), launches in parallel,
// and rolls back on partial failure.
func distributeInstances(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string, expectedNodes int) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	nodes, err := queryNodeCapacity(ctx, natsConn, instanceType, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	if len(nodes) == 0 {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	totalCapacity := 0
	for _, n := range nodes {
		totalCapacity += n.Available
	}
	if totalCapacity < minCount {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	launchCount := min(maxCount, totalCapacity)
	allocations := spreadAllocate(nodes, launchCount)
	results := launchOnNodes(ctx, allocations, input, natsConn, accountID)
	return aggregateResults(ctx, results, minCount, natsConn, accountID)
}

// queryNodeCapacity fans out spinifex.node.status and returns eligible nodes
// (Available >= 1 for instanceType), sorted by capacity desc with random tiebreaking.
// Early-exits once expectedNodes reply; on a degraded cluster it waits the full timeout
// rather than placing on a partial view.
func queryNodeCapacity(ctx context.Context, natsConn *nats.Conn, instanceType string, expectedNodes int, accountID string) ([]nodeAllocation, error) {
	frames, _, err := utils.Gather(ctx, natsConn, "spinifex.node.status", []byte("{}"),
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	var nodes []nodeAllocation
	for _, frame := range frames {
		var status types.NodeStatusResponse
		if err := json.Unmarshal(frame, &status); err != nil {
			slog.DebugContext(ctx, "queryNodeCapacity: failed to unmarshal response", "err", err)
			continue
		}
		if status.Node == "" {
			slog.DebugContext(ctx, "queryNodeCapacity: skipping response with empty node ID")
			continue
		}

		for _, cap := range status.InstanceTypes {
			if cap.Name == instanceType && cap.Available >= 1 {
				nodes = append(nodes, nodeAllocation{
					NodeID:    status.Node,
					Available: cap.Available,
				})
				break
			}
		}
	}

	// Random shuffle then stable-sort: fair distribution among equal-capacity nodes.
	rand.Shuffle(len(nodes), func(i, j int) {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	})
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodes[i].Available > nodes[j].Available
	})

	return nodes, nil
}

// spreadAllocate distributes count instances: round 1 assigns 1 per node,
// subsequent rounds pack remaining instances onto nodes with most remaining capacity.
func spreadAllocate(nodes []nodeAllocation, count int) []nodeAllocation {
	allocs := make([]nodeAllocation, len(nodes))
	copy(allocs, nodes)

	remaining := count

	for i := range allocs {
		if remaining <= 0 {
			break
		}
		allocs[i].Assigned = 1
		remaining--
	}

	// Pack remaining: pick node with most spare capacity; break ties by fewest assigned.
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

// launchOnNodes sends targeted RunInstances to each node in parallel with MinCount=MaxCount=assigned.
func launchOnNodes(ctx context.Context, allocations []nodeAllocation, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string) []nodeLaunchResult {
	instanceType := aws.StringValue(input.InstanceType)

	results := make([]nodeLaunchResult, len(allocations))
	var wg sync.WaitGroup

	for i, alloc := range allocations {
		wg.Add(1)
		go func(idx int, a nodeAllocation) {
			defer wg.Done()

			nodeInput := *input
			nodeInput.MinCount = aws.Int64(int64(a.Assigned))
			nodeInput.MaxCount = aws.Int64(int64(a.Assigned))

			topic := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, a.NodeID)
			reservation, err := utils.NATSRequest[ec2.Reservation](ctx, natsConn, topic, &nodeInput, 5*time.Minute, accountID)
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

// aggregateResults merges successful launches; rolls back and returns
// InsufficientInstanceCapacity when launched < minCount.
func aggregateResults(ctx context.Context, results []nodeLaunchResult, minCount int, natsConn *nats.Conn, accountID string) (*ec2.Reservation, error) {
	var allInstances []*ec2.Instance
	var reservationID *string

	for _, r := range results {
		if r.Err != nil {
			slog.WarnContext(ctx, "distributeInstances: node launch failed", "node", r.NodeID, "err", r.Err)
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
		if totalLaunched > 0 {
			rollbackInstances(ctx, allInstances, natsConn, accountID)
		}
		// Propagate specific client errors (e.g. InvalidAMIID.NotFound) over the generic capacity error.
		if clientErr := extractClientError(results); clientErr != nil {
			return nil, clientErr
		}
		// A launch that was attempted and failed is not a capacity shortage; surface the real cause.
		if failErr := launchFailure(results); failErr != nil {
			slog.ErrorContext(ctx, "distributeInstances: launch fell short of minCount", "minCount", minCount, "launched", totalLaunched, "err", failErr)
			return nil, failErr
		}
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	return &ec2.Reservation{
		ReservationId: reservationID,
		Instances:     allInstances,
	}, nil
}

// distributeInstancesSpread implements strict 1-per-node spread: queries capacity,
// atomically reserves nodes via CAS, launches 1 per node, then finalizes or rolls back.
func distributeInstancesSpread(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string, groupName string, expectedNodes int) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)

	nodes, err := queryNodeCapacity(ctx, natsConn, instanceType, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	eligibleNodeIDs := make([]string, len(nodes))
	for i, n := range nodes {
		eligibleNodeIDs[i] = n.NodeID
	}

	reserveOut, err := pgSvc.ReserveSpreadNodes(ctx, &handlers_ec2_placementgroup.ReserveSpreadNodesInput{
		GroupName:     groupName,
		EligibleNodes: eligibleNodeIDs,
		MinCount:      minCount,
		MaxCount:      maxCount,
	}, accountID)
	if err != nil {
		return nil, err
	}

	reservedNodes := reserveOut.ReservedNodes

	allocations := make([]nodeAllocation, len(reservedNodes))
	for i, nodeID := range reservedNodes {
		allocations[i] = nodeAllocation{NodeID: nodeID, Assigned: 1}
	}

	results := launchOnNodes(ctx, allocations, input, natsConn, accountID)

	var allInstances []*ec2.Instance
	var reservationID *string
	nodeInstances := make(map[string][]string)
	var failedNodes []string

	for _, r := range results {
		if r.Err != nil {
			slog.WarnContext(ctx, "distributeInstancesSpread: node launch failed", "node", r.NodeID, "err", r.Err)
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
		if totalLaunched > 0 {
			rollbackInstances(ctx, allInstances, natsConn, accountID)
		}
		if _, err := pgSvc.ReleaseSpreadNodes(ctx, &handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     reservedNodes,
		}, accountID); err != nil {
			slog.ErrorContext(ctx, "distributeInstancesSpread: failed to release nodes after rollback", "err", err)
		}
		if clientErr := extractClientError(results); clientErr != nil {
			return nil, clientErr
		}
		if failErr := launchFailure(results); failErr != nil {
			slog.ErrorContext(ctx, "distributeInstancesSpread: launch fell short of minCount", "minCount", minCount, "launched", totalLaunched, "err", failErr)
			return nil, failErr
		}
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Finalize: replace placeholders with instance IDs; roll back on failure.
	if _, err := pgSvc.FinalizeSpreadInstances(ctx, &handlers_ec2_placementgroup.FinalizeSpreadInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, accountID); err != nil {
		slog.ErrorContext(ctx, "distributeInstancesSpread: finalize failed, rolling back instances", "err", err)
		rollbackInstances(ctx, allInstances, natsConn, accountID)
		allReleaseNodes := append(reservedNodes[:0:0], reservedNodes...)
		if _, releaseErr := pgSvc.ReleaseSpreadNodes(ctx, &handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     allReleaseNodes,
		}, accountID); releaseErr != nil {
			slog.ErrorContext(ctx, "distributeInstancesSpread: failed to release nodes after finalize rollback", "err", releaseErr)
		}
		return nil, fmt.Errorf("failed to finalize placement group record: %w", err)
	}

	if len(failedNodes) > 0 {
		if _, err := pgSvc.ReleaseSpreadNodes(ctx, &handlers_ec2_placementgroup.ReleaseSpreadNodesInput{
			GroupName: groupName,
			Nodes:     failedNodes,
		}, accountID); err != nil {
			slog.ErrorContext(ctx, "distributeInstancesSpread: failed to release failed nodes", "err", err)
		}
	}

	return &ec2.Reservation{
		ReservationId: reservationID,
		Instances:     allInstances,
	}, nil
}

// distributeInstancesCluster pins all instances to a single node.
// Subsequent launches on an existing group reuse the same node; first launch picks highest capacity.
func distributeInstancesCluster(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID string, groupName string, expectedNodes int) (*ec2.Reservation, error) {
	instanceType := aws.StringValue(input.InstanceType)
	minCount := int(aws.Int64Value(input.MinCount))
	maxCount := int(aws.Int64Value(input.MaxCount))

	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)

	nodes, err := queryNodeCapacity(ctx, natsConn, instanceType, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	// Already sorted by capacity desc.
	eligibleNodeIDs := make([]string, len(nodes))
	for i, n := range nodes {
		eligibleNodeIDs[i] = n.NodeID
	}

	reserveOut, err := pgSvc.ReserveClusterNode(ctx, &handlers_ec2_placementgroup.ReserveClusterNodeInput{
		GroupName:     groupName,
		EligibleNodes: eligibleNodeIDs,
	}, accountID)
	if err != nil {
		return nil, err
	}

	targetNode := reserveOut.TargetNode

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

	launchCount := min(maxCount, targetCapacity)

	allocations := []nodeAllocation{{
		NodeID:   targetNode,
		Assigned: launchCount,
	}}
	results := launchOnNodes(ctx, allocations, input, natsConn, accountID)

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

	nodeInstances := make(map[string][]string)
	for _, inst := range reservation.Instances {
		if inst.InstanceId != nil {
			nodeInstances[targetNode] = append(nodeInstances[targetNode], *inst.InstanceId)
		}
	}

	if _, err := pgSvc.FinalizeClusterInstances(ctx, &handlers_ec2_placementgroup.FinalizeClusterInstancesInput{
		GroupName:     groupName,
		NodeInstances: nodeInstances,
	}, accountID); err != nil {
		slog.ErrorContext(ctx, "distributeInstancesCluster: finalize failed, rolling back instances", "err", err)
		rollbackInstances(ctx, reservation.Instances, natsConn, accountID)
		return nil, fmt.Errorf("failed to finalize cluster placement group record: %w", err)
	}

	return reservation, nil
}

// extractClientError returns the first specific client error from results
// (e.g. InvalidAMIID.NotFound) to propagate over the generic capacity error.
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
		case awserrors.ErrorInsufficientInstanceCapacity,
			awserrors.ErrorInvalidAMIIDNotFound,
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

// launchFailure returns the first node launch error, or nil when no node errored.
// A sub-minCount launch whose nodes actually errored is not a capacity shortage:
// genuine capacity is caught pre-launch in distributeInstances, and a node-side
// capacity race is surfaced by extractClientError. Surfacing the real error (RPC
// timeout, network/OVN failure) stops it masquerading as
// InsufficientInstanceCapacity — the mislabel that derails triage.
func launchFailure(results []nodeLaunchResult) error {
	for _, r := range results {
		if r.Err != nil {
			return r.Err
		}
	}
	return nil
}

// rollbackInstances terminates all instances from a failed multi-node launch.
func rollbackInstances(ctx context.Context, instances []*ec2.Instance, natsConn *nats.Conn, accountID string) {
	var ids []*string
	for _, inst := range instances {
		if inst.InstanceId != nil {
			ids = append(ids, inst.InstanceId)
		}
	}
	if len(ids) == 0 {
		return
	}

	slog.WarnContext(ctx, "distributeInstances: rolling back launched instances", "count", len(ids))
	_, err := TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: ids,
	}, natsConn, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "distributeInstances: rollback failed", "err", err)
	}
}
