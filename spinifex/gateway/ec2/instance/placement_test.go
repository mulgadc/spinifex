package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- spreadAllocate tests (pure algorithm) ---

func TestSpreadAllocate_EqualDistribution(t *testing.T) {
	// 3 instances across 3 nodes → 1 per node
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 4},
		{NodeID: "B", Available: 3},
		{NodeID: "C", Available: 2},
	}
	result := spreadAllocate(nodes, 3)

	assert.Len(t, result, 3)
	for _, a := range result {
		assert.Equal(t, 1, a.Assigned, "node %s should get exactly 1", a.NodeID)
	}
}

func TestSpreadAllocate_SpreadThenPack(t *testing.T) {
	// 5 instances across 3 nodes (capacities: A=4, B=3, C=2)
	// Round 1: A=1, B=1, C=1 (3 assigned, 2 remaining)
	// Round 2: A gets 1 (remaining cap 3), B gets 1 (remaining cap 2)
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 4},
		{NodeID: "B", Available: 3},
		{NodeID: "C", Available: 2},
	}
	result := spreadAllocate(nodes, 5)

	assert.Len(t, result, 3)
	byNode := make(map[string]int)
	for _, a := range result {
		byNode[a.NodeID] = a.Assigned
	}
	assert.Equal(t, 2, byNode["A"], "node A (cap 4) should get 2")
	assert.Equal(t, 2, byNode["B"], "node B (cap 3) should get 2")
	assert.Equal(t, 1, byNode["C"], "node C (cap 2) should get 1")
}

func TestSpreadAllocate_SingleNode(t *testing.T) {
	// All 3 instances on 1 node
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 5},
	}
	result := spreadAllocate(nodes, 3)

	assert.Len(t, result, 1)
	assert.Equal(t, "A", result[0].NodeID)
	assert.Equal(t, 3, result[0].Assigned)
}

func TestSpreadAllocate_MoreNodesThanInstances(t *testing.T) {
	// 2 instances across 5 nodes → only 2 get assigned
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 4},
		{NodeID: "B", Available: 3},
		{NodeID: "C", Available: 2},
		{NodeID: "D", Available: 2},
		{NodeID: "E", Available: 1},
	}
	result := spreadAllocate(nodes, 2)

	assert.Len(t, result, 2)
	totalAssigned := 0
	for _, a := range result {
		assert.Equal(t, 1, a.Assigned)
		totalAssigned += a.Assigned
	}
	assert.Equal(t, 2, totalAssigned)
}

func TestSpreadAllocate_HeavyPacking(t *testing.T) {
	// 10 instances across 2 nodes (A=8, B=6)
	// Round 1: A=1, B=1
	// Packing: each round picks node with most remaining
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 8},
		{NodeID: "B", Available: 6},
	}
	result := spreadAllocate(nodes, 10)

	assert.Len(t, result, 2)
	byNode := make(map[string]int)
	for _, a := range result {
		byNode[a.NodeID] = a.Assigned
	}
	total := byNode["A"] + byNode["B"]
	assert.Equal(t, 10, total)
	// A has more capacity so should get more
	assert.GreaterOrEqual(t, byNode["A"], byNode["B"])
}

func TestSpreadAllocate_ExactCapacity(t *testing.T) {
	// Request exactly matches total capacity
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 2},
		{NodeID: "B", Available: 1},
	}
	result := spreadAllocate(nodes, 3)

	assert.Len(t, result, 2)
	byNode := make(map[string]int)
	for _, a := range result {
		byNode[a.NodeID] = a.Assigned
	}
	assert.Equal(t, 2, byNode["A"])
	assert.Equal(t, 1, byNode["B"])
}

func TestSpreadAllocate_ZeroCount(t *testing.T) {
	nodes := []nodeAllocation{
		{NodeID: "A", Available: 4},
	}
	result := spreadAllocate(nodes, 0)
	assert.Len(t, result, 0)
}

func TestSpreadAllocate_EmptyNodes(t *testing.T) {
	result := spreadAllocate(nil, 3)
	assert.Len(t, result, 0)
}

// --- queryNodeCapacity tests (NATS-based) ---

func TestQueryNodeCapacity_FiltersEligibleNodes(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Simulate 3 daemons responding to spinifex.node.status
	sub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		// Respond 3 times with different node statuses
		responses := []types.NodeStatusResponse{
			{
				Node: "node-1",
				InstanceTypes: []types.InstanceTypeCap{
					{Name: "t3.micro", Available: 4},
					{Name: "t3.small", Available: 2},
				},
			},
			{
				Node: "node-2",
				InstanceTypes: []types.InstanceTypeCap{
					{Name: "t3.micro", Available: 0}, // no capacity
					{Name: "t3.small", Available: 3},
				},
			},
			{
				Node: "node-3",
				InstanceTypes: []types.InstanceTypeCap{
					{Name: "t3.micro", Available: 2},
				},
			},
		}
		for _, resp := range responses {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	// Query for t3.micro — should get node-1 (cap 4) and node-3 (cap 2), not node-2 (cap 0)
	nodes, err := queryNodeCapacity(nc, "t3.micro")
	require.NoError(t, err)

	assert.Len(t, nodes, 2)
	// Should be sorted by capacity descending
	assert.Equal(t, "node-1", nodes[0].NodeID)
	assert.Equal(t, 4, nodes[0].Available)
	assert.Equal(t, "node-3", nodes[1].NodeID)
	assert.Equal(t, 2, nodes[1].Available)
}

func TestQueryNodeCapacity_NoNodes(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// No subscribers → timeout, empty result
	nodes, err := queryNodeCapacity(nc, "t3.micro")
	require.NoError(t, err)
	assert.Len(t, nodes, 0)
}

// --- aggregateResults tests ---

func TestAggregateResults_AllSucceed(t *testing.T) {
	results := []nodeLaunchResult{
		{
			NodeID: "node-1",
			Reservation: &ec2.Reservation{
				ReservationId: aws.String("r-abc"),
				Instances: []*ec2.Instance{
					{InstanceId: aws.String("i-001")},
				},
			},
		},
		{
			NodeID: "node-2",
			Reservation: &ec2.Reservation{
				ReservationId: aws.String("r-def"),
				Instances: []*ec2.Instance{
					{InstanceId: aws.String("i-002")},
					{InstanceId: aws.String("i-003")},
				},
			},
		},
	}

	// No NATS needed — all succeed, no rollback
	reservation, err := aggregateResults(results, 2, nil, "")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 3)
	assert.Equal(t, "r-abc", aws.StringValue(reservation.ReservationId))
}

func TestAggregateResults_PartialSuccessMeetsMinCount(t *testing.T) {
	results := []nodeLaunchResult{
		{
			NodeID: "node-1",
			Reservation: &ec2.Reservation{
				ReservationId: aws.String("r-abc"),
				Instances: []*ec2.Instance{
					{InstanceId: aws.String("i-001")},
					{InstanceId: aws.String("i-002")},
				},
			},
		},
		{
			NodeID: "node-2",
			Err:    assert.AnError,
		},
	}

	// MinCount=2, got 2 from node-1 → success
	reservation, err := aggregateResults(results, 2, nil, "")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2)
}

func TestAggregateResults_PartialFailureBelowMinCount(t *testing.T) {
	_, nc := startTestNATSServer(t)

	results := []nodeLaunchResult{
		{
			NodeID: "node-1",
			Reservation: &ec2.Reservation{
				Instances: []*ec2.Instance{
					{InstanceId: aws.String("i-001")},
				},
			},
		},
		{
			NodeID: "node-2",
			Err:    assert.AnError,
		},
	}

	// MinCount=3, only got 1 → should fail with InsufficientInstanceCapacity
	// Note: rollback will attempt to terminate i-001 but we don't have a
	// daemon responding, so it will fail silently — that's OK for this test
	_, err := aggregateResults(results, 3, nc, "test-account")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestAggregateResults_AllFail(t *testing.T) {
	results := []nodeLaunchResult{
		{NodeID: "node-1", Err: assert.AnError},
		{NodeID: "node-2", Err: assert.AnError},
	}

	_, err := aggregateResults(results, 1, nil, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

// --- extractClientError tests ---

func TestExtractClientError_NoErrors(t *testing.T) {
	results := []nodeLaunchResult{
		{NodeID: "node-1", Reservation: &ec2.Reservation{}},
	}
	assert.Nil(t, extractClientError(results))
}

func TestExtractClientError_GenericError(t *testing.T) {
	results := []nodeLaunchResult{
		{NodeID: "node-1", Err: assert.AnError},
	}
	assert.Nil(t, extractClientError(results))
}

func TestExtractClientError_AMINotFound(t *testing.T) {
	// Simulate the error wrapping that launchOnNodes does
	inner := errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	wrapped := fmt.Errorf("launch on node-1: %w", inner)
	results := []nodeLaunchResult{
		{NodeID: "node-1", Err: wrapped},
	}
	err := extractClientError(results)
	require.NotNil(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestExtractClientError_KeyPairNotFound(t *testing.T) {
	inner := errors.New(awserrors.ErrorInvalidKeyPairNotFound)
	wrapped := fmt.Errorf("launch on node-1: %w", inner)
	results := []nodeLaunchResult{
		{NodeID: "node-1", Err: wrapped},
	}
	err := extractClientError(results)
	require.NotNil(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

func TestAggregateResults_PropagatesClientError(t *testing.T) {
	inner := errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	wrapped := fmt.Errorf("launch on node-1: %w", inner)
	results := []nodeLaunchResult{
		{NodeID: "node-1", Err: wrapped},
		{NodeID: "node-2", Err: wrapped},
	}

	_, err := aggregateResults(results, 1, nil, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

// --- distributeInstances integration tests (end-to-end with mock daemons) ---

func TestDistributeInstances_SuccessfulSpread(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Mock node.status responder
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, resp := range []types.NodeStatusResponse{
			{Node: "node-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}}},
			{Node: "node-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}}},
		} {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock daemon on node-1
	sub1, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-test1"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-n1")}},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub1.Unsubscribe()

	// Mock daemon on node-2
	sub2, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-2", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-test2"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-n2")}},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub2.Unsubscribe()

	time.Sleep(50 * time.Millisecond) // let subscriptions propagate

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}

	reservation, err := distributeInstances(input, nc, "test-account")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2)

	// Verify instances came from different nodes
	ids := make(map[string]bool)
	for _, inst := range reservation.Instances {
		ids[aws.StringValue(inst.InstanceId)] = true
	}
	assert.True(t, ids["i-n1"], "should have instance from node-1")
	assert.True(t, ids["i-n2"], "should have instance from node-2")
}

func TestDistributeInstances_InsufficientCapacity(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Mock node.status with only 1 available slot total
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 1}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(3),
		MaxCount:     aws.Int64(3),
	}

	_, err = distributeInstances(input, nc, "test-account")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestDistributeInstances_PropagatesAMINotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Mock node.status with capacity available
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock daemon responds with InvalidAMIID.NotFound (AMI doesn't exist)
	sub1, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInvalidAMIIDNotFound))
	})
	require.NoError(t, err)
	defer sub1.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0000000000000dead"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}

	_, err = distributeInstances(input, nc, "test-account")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error(),
		"should propagate InvalidAMIID.NotFound, not InsufficientInstanceCapacity")
}

// TestDistributeInstances_PropagatesSGValidationErrors verifies that
// SG-related boundary errors from the daemon (`InvalidGroup.NotFound`,
// `InvalidParameterValue` for cross-VPC SGs, and
// `SecurityGroupsPerInterfaceLimitExceeded` for >5 SGs) reach the caller
// instead of being collapsed into `InsufficientInstanceCapacity`.
// Terraform / SDK consumers branch on the specific code; masking it as
// "no capacity" makes a typo'd SG ID look like a transient cluster issue.
func TestDistributeInstances_PropagatesSGValidationErrors(t *testing.T) {
	cases := []struct {
		name          string
		daemonErrCode string
	}{
		{"invalid-group-not-found", awserrors.ErrorInvalidGroupNotFound},
		{"cross-vpc-invalid-param-value", awserrors.ErrorInvalidParameterValue},
		{"too-many-sgs", awserrors.ErrorSecurityGroupsPerInterfaceLimitExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc := startTestNATSServer(t)

			statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
				resp := types.NodeStatusResponse{
					Node:          "node-1",
					InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}},
				}
				data, _ := json.Marshal(resp)
				_ = nc.Publish(msg.Reply, data)
			})
			require.NoError(t, err)
			defer statusSub.Unsubscribe()

			daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
				_ = msg.Respond(utils.GenerateErrorPayload(tc.daemonErrCode))
			})
			require.NoError(t, err)
			defer daemonSub.Unsubscribe()

			time.Sleep(50 * time.Millisecond)

			input := &ec2.RunInstancesInput{
				ImageId:      aws.String("ami-test"),
				InstanceType: aws.String("t3.micro"),
				MinCount:     aws.Int64(1),
				MaxCount:     aws.Int64(1),
			}

			_, err = distributeInstances(input, nc, "test-account")
			require.Error(t, err)
			assert.Equal(t, tc.daemonErrCode, err.Error(),
				"daemon SG validation error must be surfaced, not InsufficientInstanceCapacity")
		})
	}
}

func TestDistributeInstances_LaunchCountCappedToMaxCount(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// 3 nodes with capacity, but MaxCount=2
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, resp := range []types.NodeStatusResponse{
			{Node: "node-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
			{Node: "node-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 3}}},
			{Node: "node-3", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}}},
		} {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock daemons — each returns 1 instance
	for _, nodeID := range []string{"node-1", "node-2"} {
		sub, err := nc.Subscribe("ec2.RunInstances.t3.micro."+nodeID, func(msg *nats.Msg) {
			reservation := ec2.Reservation{
				ReservationId: aws.String("r-" + nodeID),
				Instances:     []*ec2.Instance{{InstanceId: aws.String("i-" + nodeID)}},
			}
			data, _ := json.Marshal(reservation)
			_ = msg.Respond(data)
		})
		require.NoError(t, err)
		defer sub.Unsubscribe()
	}

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(2),
	}

	reservation, err := distributeInstances(input, nc, "test-account")
	require.NoError(t, err)
	// Should launch exactly 2 (MaxCount), not 3 (total capacity)
	assert.Len(t, reservation.Instances, 2)
}

func TestDistributeInstances_NoNodesAvailable(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// No responders to node.status
	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}

	_, err := distributeInstances(input, nc, "test-account")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

// --- RunInstances routing tests ---

func TestRunInstances_SingleInstanceDistributes(t *testing.T) {
	// For MinCount=MaxCount=1, RunInstances should still query node capacity
	// and route to a specific node via targeted topic (not queue group).
	_, nc := startTestNATSServer(t)

	// Mock node status response (1 node with capacity)
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock the node-specific handler (targeted topic)
	nodeSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-single"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-single")}},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer nodeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("test-key"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}

	reservation, err := RunInstances(input, nc, "test-account")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 1)
	assert.Equal(t, "i-single", aws.StringValue(reservation.Instances[0].InstanceId))
}

// --- placementGroupName tests ---

func TestPlacementGroupName_WithGroupName(t *testing.T) {
	input := &ec2.RunInstancesInput{
		Placement: &ec2.Placement{
			GroupName: aws.String("my-group"),
		},
	}
	assert.Equal(t, "my-group", placementGroupName(input))
}

func TestPlacementGroupName_NilPlacement(t *testing.T) {
	input := &ec2.RunInstancesInput{}
	assert.Equal(t, "", placementGroupName(input))
}

func TestPlacementGroupName_NilGroupName(t *testing.T) {
	input := &ec2.RunInstancesInput{
		Placement: &ec2.Placement{},
	}
	assert.Equal(t, "", placementGroupName(input))
}

func TestPlacementGroupName_EmptyGroupName(t *testing.T) {
	input := &ec2.RunInstancesInput{
		Placement: &ec2.Placement{
			GroupName: aws.String(""),
		},
	}
	assert.Equal(t, "", placementGroupName(input))
}

// --- lookupPlacementGroupStrategy tests ---

func TestLookupPlacementGroupStrategy_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Mock DescribePlacementGroups responder
	sub, err := nc.QueueSubscribe("ec2.DescribePlacementGroups", "spinifex-workers", func(msg *nats.Msg) {
		out := ec2.DescribePlacementGroupsOutput{
			PlacementGroups: []*ec2.PlacementGroup{
				{
					GroupName: aws.String("my-group"),
					Strategy:  aws.String("spread"),
					State:     aws.String("available"),
				},
			},
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	strategy, err := lookupPlacementGroupStrategy(nc, "test-account", "my-group")
	require.NoError(t, err)
	assert.Equal(t, "spread", strategy)
}

func TestLookupPlacementGroupStrategy_NotFound(t *testing.T) {
	_, nc := startTestNATSServer(t)

	// Mock responder returns error
	sub, err := nc.QueueSubscribe("ec2.DescribePlacementGroups", "spinifex-workers", func(msg *nats.Msg) {
		errPayload := `{"Code":"InvalidPlacementGroup.Unknown","Message":"not found"}`
		_ = msg.Respond([]byte(errPayload))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	_, err = lookupPlacementGroupStrategy(nc, "test-account", "ghost-group")
	require.Error(t, err)
}

func TestLookupPlacementGroupStrategy_NotAvailable(t *testing.T) {
	_, nc := startTestNATSServer(t)

	sub, err := nc.QueueSubscribe("ec2.DescribePlacementGroups", "spinifex-workers", func(msg *nats.Msg) {
		out := ec2.DescribePlacementGroupsOutput{
			PlacementGroups: []*ec2.PlacementGroup{
				{
					GroupName: aws.String("my-group"),
					Strategy:  aws.String("spread"),
					State:     aws.String("deleting"),
				},
			},
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	_, err = lookupPlacementGroupStrategy(nc, "test-account", "my-group")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

func TestLookupPlacementGroupStrategy_EmptyResult(t *testing.T) {
	_, nc := startTestNATSServer(t)

	sub, err := nc.QueueSubscribe("ec2.DescribePlacementGroups", "spinifex-workers", func(msg *nats.Msg) {
		out := ec2.DescribePlacementGroupsOutput{
			PlacementGroups: []*ec2.PlacementGroup{},
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	_, err = lookupPlacementGroupStrategy(nc, "test-account", "my-group")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

// --- distributeInstancesCluster tests ---

func TestDistributeInstancesCluster_FirstLaunchPicksBestNode(t *testing.T) {
	// First launch on empty cluster group should pick node with most capacity
	_, nc := startTestNATSServer(t)

	// Mock node.status: node-1 has 4, node-2 has 6 (node-2 should be picked)
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, resp := range []types.NodeStatusResponse{
			{Node: "node-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 4}}},
			{Node: "node-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 6}}},
		} {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock ReserveClusterNode — returns node-2 (highest capacity)
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-2"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	// Mock daemon on node-2
	daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-2", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-cluster"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-c1")},
				{InstanceId: aws.String("i-c2")},
				{InstanceId: aws.String("i-c3")},
			},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer daemonSub.Unsubscribe()

	// Mock FinalizeClusterInstances
	finalizeSub, err := nc.QueueSubscribe("ec2.FinalizeClusterInstances", "spinifex-workers", func(msg *nats.Msg) {
		data, _ := json.Marshal(struct{}{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer finalizeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(3),
		MaxCount:     aws.Int64(3),
	}

	reservation, err := distributeInstancesCluster(input, nc, "test-account", "my-cluster-group")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 3)

	// All instances should be from node-2
	for _, inst := range reservation.Instances {
		assert.NotNil(t, inst.InstanceId)
	}
}

func TestDistributeInstancesCluster_SubsequentLaunchPinsToExistingNode(t *testing.T) {
	// Subsequent launch should go to the same node the group already uses
	_, nc := startTestNATSServer(t)

	// Mock node.status: both nodes have capacity
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, resp := range []types.NodeStatusResponse{
			{Node: "node-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 5}}},
			{Node: "node-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 3}}},
		} {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock ReserveClusterNode — returns node-2 (existing pinned node, even though node-1 has more capacity)
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-2"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	// Mock daemon on node-2 only — node-1 should NOT be contacted
	node1Contacted := false
	node1Sub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		node1Contacted = true
	})
	require.NoError(t, err)
	defer node1Sub.Unsubscribe()

	daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-2", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-cluster2"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-c4")},
				{InstanceId: aws.String("i-c5")},
			},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer daemonSub.Unsubscribe()

	// Mock FinalizeClusterInstances
	finalizeSub, err := nc.QueueSubscribe("ec2.FinalizeClusterInstances", "spinifex-workers", func(msg *nats.Msg) {
		data, _ := json.Marshal(struct{}{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer finalizeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}

	reservation, err := distributeInstancesCluster(input, nc, "test-account", "my-cluster-group")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2)
	assert.False(t, node1Contacted, "cluster should only contact the pinned node")
}

func TestDistributeInstancesCluster_InsufficientCapacityOnPinnedNode(t *testing.T) {
	// Pinned node doesn't have enough capacity → InsufficientInstanceCapacity
	_, nc := startTestNATSServer(t)

	// Mock node.status: pinned node-2 has only 1 slot, node-1 has plenty
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for _, resp := range []types.NodeStatusResponse{
			{Node: "node-1", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 10}}},
			{Node: "node-2", InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 1}}},
		} {
			data, _ := json.Marshal(resp)
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock ReserveClusterNode — returns node-2 (existing pinned node)
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-2"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(3),
		MaxCount:     aws.Int64(3),
	}

	_, err = distributeInstancesCluster(input, nc, "test-account", "my-cluster-group")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestDistributeInstancesCluster_PinnedNodeNotInCapacityResults(t *testing.T) {
	// Pinned node has no capacity at all (not in fan-out results) → InsufficientInstanceCapacity
	_, nc := startTestNATSServer(t)

	// Mock node.status: only node-1 has capacity (pinned node-2 is at 0)
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 5}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock ReserveClusterNode — returns node-2 (pinned but no capacity)
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-2"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}

	_, err = distributeInstancesCluster(input, nc, "test-account", "my-cluster-group")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestDistributeInstancesCluster_LaunchCountCappedByCapacityAndMaxCount(t *testing.T) {
	// Target node has 3 available but MaxCount=2 → should launch 2
	_, nc := startTestNATSServer(t)

	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 3}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-1"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	// Daemon returns 2 instances (matching assigned count)
	daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		var reqInput ec2.RunInstancesInput
		_ = json.Unmarshal(msg.Data, &reqInput)
		count := int(aws.Int64Value(reqInput.MaxCount))
		instances := make([]*ec2.Instance, count)
		for i := range instances {
			instances[i] = &ec2.Instance{InstanceId: aws.String(fmt.Sprintf("i-%d", i))}
		}
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-capped"),
			Instances:     instances,
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer daemonSub.Unsubscribe()

	finalizeSub, err := nc.QueueSubscribe("ec2.FinalizeClusterInstances", "spinifex-workers", func(msg *nats.Msg) {
		data, _ := json.Marshal(struct{}{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer finalizeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(2),
	}

	reservation, err := distributeInstancesCluster(input, nc, "test-account", "my-cluster-group")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2, "should launch min(MaxCount=2, capacity=3) = 2")
}

func TestRunInstances_ClusterPlacementGroupRouting(t *testing.T) {
	// RunInstances with a cluster placement group should route through
	// the cluster path (lookupPlacementGroupStrategy → distributeInstancesCluster).
	_, nc := startTestNATSServer(t)

	// Mock DescribePlacementGroups (for lookupPlacementGroupStrategy)
	pgSub, err := nc.QueueSubscribe("ec2.DescribePlacementGroups", "spinifex-workers", func(msg *nats.Msg) {
		out := ec2.DescribePlacementGroupsOutput{
			PlacementGroups: []*ec2.PlacementGroup{
				{
					GroupName: aws.String("my-cluster"),
					Strategy:  aws.String("cluster"),
					State:     aws.String("available"),
				},
			},
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer pgSub.Unsubscribe()

	// Mock node.status
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 5}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock ReserveClusterNode
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveClusterNode", "spinifex-workers", func(msg *nats.Msg) {
		out := struct {
			TargetNode string `json:"target_node"`
		}{TargetNode: "node-1"}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	// Mock daemon
	daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-cluster"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-c1")},
				{InstanceId: aws.String("i-c2")},
			},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer daemonSub.Unsubscribe()

	// Mock FinalizeClusterInstances
	finalizeSub, err := nc.QueueSubscribe("ec2.FinalizeClusterInstances", "spinifex-workers", func(msg *nats.Msg) {
		data, _ := json.Marshal(struct{}{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer finalizeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("test-key"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
		Placement: &ec2.Placement{
			GroupName: aws.String("my-cluster"),
		},
	}

	reservation, err := RunInstances(input, nc, "test-account")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2)
}

func TestRunInstances_MultiInstanceUsesDistribution(t *testing.T) {
	// For MaxCount > 1, RunInstances should use the distribution path,
	// which queries spinifex.node.status.
	_, nc := startTestNATSServer(t)

	statusQueried := false
	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		statusQueried = true
		resp := types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 3}},
		}
		data, _ := json.Marshal(resp)
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// Mock node-specific handler
	nodeSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		reservation := ec2.Reservation{
			ReservationId: aws.String("r-multi"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-001")},
				{InstanceId: aws.String("i-002")},
			},
		}
		data, _ := json.Marshal(reservation)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	defer nodeSub.Unsubscribe()

	time.Sleep(50 * time.Millisecond)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("test-key"),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	}

	reservation, err := RunInstances(input, nc, "test-account")
	require.NoError(t, err)
	assert.Len(t, reservation.Instances, 2)
	assert.True(t, statusQueried, "multi-instance launch should query node status")
}
