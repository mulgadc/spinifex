package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spreadHarness records what the placement-group service and the node daemons
// were asked to do, so tests can assert on reservation, launch and release
// traffic rather than only on the returned reservation.
type spreadHarness struct {
	mu sync.Mutex

	reserveInput   handlers_ec2_placementgroup.ReserveSpreadNodesInput
	finalizeInput  handlers_ec2_placementgroup.FinalizeSpreadInstancesInput
	releaseInputs  []handlers_ec2_placementgroup.ReleaseSpreadNodesInput
	launchCounts   map[string]int64
	terminatedIDs  []string
	finalizeCalled bool
}

// mockSpreadCluster wires node.status, the three placement-group subjects and a
// per-node RunInstances responder. daemonResp maps a node ID to the payload that
// node replies with; a node absent from the map is left without a responder.
func mockSpreadCluster(t *testing.T, nc *nats.Conn, capacity map[string]int, reserved []string, daemonResp map[string][]byte, finalizeErr string) *spreadHarness {
	t.Helper()
	h := &spreadHarness{launchCounts: map[string]int64{}}

	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		for node, avail := range capacity {
			data, _ := json.Marshal(types.NodeStatusResponse{
				Node:          node,
				InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: avail}},
			})
			_ = nc.Publish(msg.Reply, data)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = statusSub.Unsubscribe() })

	reserveSub, err := nc.QueueSubscribe("ec2.ReserveSpreadNodes", "spinifex-workers", func(msg *nats.Msg) {
		h.mu.Lock()
		_ = json.Unmarshal(msg.Data, &h.reserveInput)
		h.mu.Unlock()
		data, _ := json.Marshal(handlers_ec2_placementgroup.ReserveSpreadNodesOutput{ReservedNodes: reserved})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reserveSub.Unsubscribe() })

	finalizeSub, err := nc.QueueSubscribe("ec2.FinalizeSpreadInstances", "spinifex-workers", func(msg *nats.Msg) {
		h.mu.Lock()
		h.finalizeCalled = true
		_ = json.Unmarshal(msg.Data, &h.finalizeInput)
		h.mu.Unlock()
		if finalizeErr != "" {
			_ = msg.Respond(utils.GenerateErrorPayload(finalizeErr))
			return
		}
		data, _ := json.Marshal(handlers_ec2_placementgroup.FinalizeSpreadInstancesOutput{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = finalizeSub.Unsubscribe() })

	releaseSub, err := nc.QueueSubscribe("ec2.ReleaseSpreadNodes", "spinifex-workers", func(msg *nats.Msg) {
		var in handlers_ec2_placementgroup.ReleaseSpreadNodesInput
		_ = json.Unmarshal(msg.Data, &in)
		h.mu.Lock()
		h.releaseInputs = append(h.releaseInputs, in)
		h.mu.Unlock()
		data, _ := json.Marshal(handlers_ec2_placementgroup.ReleaseSpreadNodesOutput{})
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = releaseSub.Unsubscribe() })

	for node, resp := range daemonResp {
		sub, err := nc.Subscribe("ec2.RunInstances.t3.micro."+node, func(msg *nats.Msg) {
			var reqInput ec2.RunInstancesInput
			_ = json.Unmarshal(msg.Data, &reqInput)
			h.mu.Lock()
			h.launchCounts[node] = aws.Int64Value(reqInput.MaxCount)
			h.mu.Unlock()
			_ = msg.Respond(resp)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	termSub, err := nc.Subscribe("ec2.cmd.>", func(msg *nats.Msg) {
		var cmd types.EC2InstanceCommand
		_ = json.Unmarshal(msg.Data, &cmd)
		h.mu.Lock()
		h.terminatedIDs = append(h.terminatedIDs, cmd.ID)
		h.mu.Unlock()
		_ = msg.Respond([]byte(`{"return":{}}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = termSub.Unsubscribe() })

	require.NoError(t, nc.Flush())
	return h
}

// spreadReservation is the payload a node daemon returns for a 1-instance launch.
func spreadReservation(t *testing.T, reservationID, instanceID string) []byte {
	t.Helper()
	data, err := json.Marshal(ec2.Reservation{
		ReservationId: aws.String(reservationID),
		Instances:     []*ec2.Instance{{InstanceId: aws.String(instanceID)}},
	})
	require.NoError(t, err)
	return data
}

func spreadInput(minCount, maxCount int64) *ec2.RunInstancesInput {
	return &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-test"),
		InstanceType: aws.String("t3.micro"),
		MinCount:     aws.Int64(minCount),
		MaxCount:     aws.Int64(maxCount),
	}
}

func TestDistributeInstancesSpread_OnePerNode(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	h := mockSpreadCluster(t, nc,
		map[string]int{"node-1": 4, "node-2": 3, "node-3": 2},
		[]string{"node-1", "node-2", "node-3"},
		map[string][]byte{
			"node-1": spreadReservation(t, "r-spread", "i-n1"),
			"node-2": spreadReservation(t, "r-spread2", "i-n2"),
			"node-3": spreadReservation(t, "r-spread3", "i-n3"),
		}, "")

	reservation, err := distributeInstancesSpread(context.Background(), spreadInput(3, 3), nc, "test-account", "my-spread", 3)
	require.NoError(t, err)
	require.Len(t, reservation.Instances, 3)
	assert.NotNil(t, reservation.ReservationId)

	h.mu.Lock()
	defer h.mu.Unlock()

	// Strict spread: exactly one instance requested per reserved node.
	assert.Equal(t, map[string]int64{"node-1": 1, "node-2": 1, "node-3": 1}, h.launchCounts)

	assert.Equal(t, "my-spread", h.reserveInput.GroupName)
	assert.ElementsMatch(t, []string{"node-1", "node-2", "node-3"}, h.reserveInput.EligibleNodes)
	assert.Equal(t, 3, h.reserveInput.MinCount)
	assert.Equal(t, 3, h.reserveInput.MaxCount)

	assert.Equal(t, map[string][]string{
		"node-1": {"i-n1"},
		"node-2": {"i-n2"},
		"node-3": {"i-n3"},
	}, h.finalizeInput.NodeInstances)

	assert.Empty(t, h.releaseInputs, "no release when every reserved node launched")
	assert.Empty(t, h.terminatedIDs, "no rollback on success")
}

func TestDistributeInstancesSpread_NoEligibleNodes(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// Capacity exists for a different instance type only, so nothing is eligible.
	h := mockSpreadCluster(t, nc, map[string]int{}, nil, nil, "")

	_, err := distributeInstancesSpread(context.Background(), spreadInput(2, 2), nc, "test-account", "my-spread", 2)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.Empty(t, h.reserveInput.GroupName, "reservation must not be attempted with no eligible nodes")
}

func TestDistributeInstancesSpread_ReserveErrorPropagates(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	statusSub, err := nc.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		data, _ := json.Marshal(types.NodeStatusResponse{
			Node:          "node-1",
			InstanceTypes: []types.InstanceTypeCap{{Name: "t3.micro", Available: 2}},
		})
		_ = nc.Publish(msg.Reply, data)
	})
	require.NoError(t, err)
	defer statusSub.Unsubscribe()

	// The CAS layer rejects the reservation (e.g. group already at capacity).
	reserveSub, err := nc.QueueSubscribe("ec2.ReserveSpreadNodes", "spinifex-workers", func(msg *nats.Msg) {
		_ = msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorInsufficientInstanceCapacity))
	})
	require.NoError(t, err)
	defer reserveSub.Unsubscribe()

	launched := false
	daemonSub, err := nc.Subscribe("ec2.RunInstances.t3.micro.node-1", func(msg *nats.Msg) {
		launched = true
		_ = msg.Respond(spreadReservation(t, "r-x", "i-x"))
	})
	require.NoError(t, err)
	defer daemonSub.Unsubscribe()

	require.NoError(t, nc.Flush())

	_, err = distributeInstancesSpread(context.Background(), spreadInput(1, 1), nc, "test-account", "my-spread", 1)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
	assert.False(t, launched, "no node may be launched on when the reservation failed")
}

func TestDistributeInstancesSpread_PartialSuccessMeetsMinCount(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// node-3 is reserved but has no responder, so its launch fails.
	h := mockSpreadCluster(t, nc,
		map[string]int{"node-1": 2, "node-2": 2, "node-3": 2},
		[]string{"node-1", "node-2", "node-3"},
		map[string][]byte{
			"node-1": spreadReservation(t, "r-p1", "i-p1"),
			"node-2": spreadReservation(t, "r-p2", "i-p2"),
			"node-3": utils.GenerateErrorPayload(awserrors.ErrorServerInternal),
		}, "")

	reservation, err := distributeInstancesSpread(context.Background(), spreadInput(2, 3), nc, "test-account", "my-spread", 3)
	require.NoError(t, err)
	require.Len(t, reservation.Instances, 2)

	h.mu.Lock()
	defer h.mu.Unlock()

	assert.Equal(t, map[string][]string{"node-1": {"i-p1"}, "node-2": {"i-p2"}}, h.finalizeInput.NodeInstances)

	// Only the node that failed to launch gets its slot back.
	require.Len(t, h.releaseInputs, 1)
	assert.Equal(t, "my-spread", h.releaseInputs[0].GroupName)
	assert.Equal(t, []string{"node-3"}, h.releaseInputs[0].Nodes)
	assert.Empty(t, h.terminatedIDs, "successful instances are kept when minCount is met")
}

func TestDistributeInstancesSpread_BelowMinCountRollsBackAndReleases(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	h := mockSpreadCluster(t, nc,
		map[string]int{"node-1": 2, "node-2": 2},
		[]string{"node-1", "node-2"},
		map[string][]byte{
			"node-1": spreadReservation(t, "r-b1", "i-b1"),
			"node-2": utils.GenerateErrorPayload(awserrors.ErrorServerInternal),
		}, "")

	_, err := distributeInstancesSpread(context.Background(), spreadInput(2, 2), nc, "test-account", "my-spread", 2)
	require.Error(t, err)
	// A node that errored is a real failure, not a capacity shortage, and the
	// message names the node that failed.
	assert.Equal(t, "launch on node-2: "+awserrors.ErrorServerInternal, err.Error())

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.False(t, h.finalizeCalled, "a short launch must not be finalized")
	assert.Equal(t, []string{"i-b1"}, h.terminatedIDs, "the one launched instance is rolled back")
	require.Len(t, h.releaseInputs, 1)
	assert.Equal(t, []string{"node-1", "node-2"}, h.releaseInputs[0].Nodes,
		"every reserved node is released, not just the failed one")
}

func TestDistributeInstancesSpread_PropagatesClientError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	h := mockSpreadCluster(t, nc,
		map[string]int{"node-1": 2},
		[]string{"node-1"},
		map[string][]byte{
			"node-1": utils.GenerateErrorPayload(awserrors.ErrorInvalidAMIIDNotFound),
		}, "")

	_, err := distributeInstancesSpread(context.Background(), spreadInput(1, 1), nc, "test-account", "my-spread", 1)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error(),
		"a bad AMI must not be reported as InsufficientInstanceCapacity")

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.releaseInputs, 1)
	assert.Equal(t, []string{"node-1"}, h.releaseInputs[0].Nodes)
}

func TestDistributeInstancesSpread_FinalizeFailureRollsBack(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	h := mockSpreadCluster(t, nc,
		map[string]int{"node-1": 2, "node-2": 2},
		[]string{"node-1", "node-2"},
		map[string][]byte{
			"node-1": spreadReservation(t, "r-f1", "i-f1"),
			"node-2": spreadReservation(t, "r-f2", "i-f2"),
		}, awserrors.ErrorServerInternal)

	_, err := distributeInstancesSpread(context.Background(), spreadInput(2, 2), nc, "test-account", "my-spread", 2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to finalize placement group record")
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)

	h.mu.Lock()
	defer h.mu.Unlock()
	assert.ElementsMatch(t, []string{"i-f1", "i-f2"}, h.terminatedIDs,
		"instances launched before a failed finalize must be terminated")
	require.Len(t, h.releaseInputs, 1)
	assert.Equal(t, []string{"node-1", "node-2"}, h.releaseInputs[0].Nodes)
}
