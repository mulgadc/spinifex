//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// TestRunInstances_MultiCountExpansion asserts that a RunInstances call with
// MinCount=MaxCount=2 returns 2 instances with non-empty InstanceIds. That
// expansion is gateway-side logic — distributeInstances in
// spinifex/gateway/ec2/instance/placement.go queries node capacity over NATS,
// computes how many instances to launch, and dispatches a single targeted
// launch per node — resolved entirely before the daemon-facing NATS hop, so a
// static stub of the daemon side is sufficient; no real guest is needed.
//
// The live test's WaitForInstanceState(running)/TerminateInstances/
// WaitForInstanceState(terminated) sequence is left behind: it is teardown
// hygiene for a real throwaway VM, not part of what this test asserts.
//
// To prove the expansion is genuinely exercised rather than short-circuited by
// the zero-daemon degenerate path (placement.go's queryNodeCapacity returning
// no eligible nodes, which also yields InsufficientInstanceCapacity — see
// placement.go:44), this test subscribes to the per-node launch subject
// directly instead of using StubSubject's static responder, and asserts the
// daemon actually received MinCount=MaxCount=2 on that targeted request.
func TestRunInstances_MultiCountExpansion(t *testing.T) {
	gw := StartGateway(t)

	const (
		instanceType = "t3.micro"
		nodeID       = "node-1"
	)

	// A single node reporting capacity for 2 of instanceType — enough for
	// distributeInstances to resolve MinCount=MaxCount=2 onto one node.
	statusResp := mustMarshal(t, &types.NodeStatusResponse{
		Node: nodeID,
		InstanceTypes: []types.InstanceTypeCap{
			{Name: instanceType, Available: 2},
		},
	})
	gw.StubSubject(t, "spinifex.node.status", statusResp)

	var gotMinCount, gotMaxCount int64
	launchSubject := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, nodeID)
	sub, err := gw.NATSConn.Subscribe(launchSubject, func(msg *nats.Msg) {
		var nodeInput ec2.RunInstancesInput
		if jsonErr := json.Unmarshal(msg.Data, &nodeInput); jsonErr != nil {
			t.Errorf("unmarshal per-node RunInstances request: %v", jsonErr)
			return
		}
		gotMinCount = aws.Int64Value(nodeInput.MinCount)
		gotMaxCount = aws.Int64Value(nodeInput.MaxCount)
		_ = msg.Respond(mustMarshal(t, &ec2.Reservation{
			ReservationId: aws.String("r-multi-count"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-multi-count-1")},
				{InstanceId: aws.String("i-multi-count-2")},
			},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String(instanceType),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
	})
	require.NoError(t, err, "run-instances --count 2")
	require.Lenf(t, out.Instances, 2, "expected 2 instances from run-instances, got %d", len(out.Instances))
	require.NotEmpty(t, aws.StringValue(out.Instances[0].InstanceId), "first sibling InstanceId empty")
	require.NotEmpty(t, aws.StringValue(out.Instances[1].InstanceId), "second sibling InstanceId empty")

	// Proves the gateway genuinely expanded the count onto the per-node
	// launch, rather than the zero-daemon path (which never reaches this
	// subject at all) or a daemon stub that ignores its input.
	require.Equal(t, int64(2), gotMinCount, "gateway must dispatch MinCount=2 on the per-node launch")
	require.Equal(t, int64(2), gotMaxCount, "gateway must dispatch MaxCount=2 on the per-node launch")
}
