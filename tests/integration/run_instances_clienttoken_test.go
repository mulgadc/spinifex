//go:build integration

package integration

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// TestRunInstances_ClientTokenIdempotency proves ClientToken dedup
// (gateway/ec2/instance/RunInstances.go's ClientTokenStore wrapping) runs for
// real through the full gateway: a replayed token must not reach the daemon a
// second time, and a different token must launch again.
//
// getClientTokenStore (clienttoken.go) binds its JetStream KV bucket via a
// process-wide sync.Once, so the very first test in this package's binary
// that supplies a ClientToken permanently decides which test's NATS account
// backs the store for the rest of the run. That is harmless here because
// every key is namespaced by accountID+token and this test uses ClientToken
// values that appear nowhere else in the suite, so no other test's launch can
// collide with this one's regardless of run order (including -shuffle).
func TestRunInstances_ClientTokenIdempotency(t *testing.T) {
	gw := StartGateway(t)

	const (
		instanceType = "t3.micro"
		nodeID       = "ct-node"
	)

	statusResp := mustMarshal(t, &types.NodeStatusResponse{
		Node: nodeID,
		InstanceTypes: []types.InstanceTypeCap{
			{Name: instanceType, Available: 10},
		},
	})
	gw.StubSubject(t, "spinifex.node.status", statusResp)

	// dispatchCount records how many times the daemon-side per-node launch
	// subject actually fired, so a replayed ClientToken that (incorrectly)
	// dispatched again is caught even though the SDK call still succeeds.
	var dispatchCount atomic.Int64
	launchSubject := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, nodeID)
	sub, err := gw.NATSConn.Subscribe(launchSubject, func(msg *nats.Msg) {
		n := dispatchCount.Add(1)
		_ = msg.Respond(mustMarshal(t, &ec2.Reservation{
			ReservationId: aws.String(fmt.Sprintf("r-ct-%d", n)),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String(fmt.Sprintf("i-ct-%d", n))},
			},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	baseInput := func(token string) *ec2.RunInstancesInput {
		return &ec2.RunInstancesInput{
			ImageId:      aws.String("ami-0123456789abcdef0"),
			InstanceType: aws.String(instanceType),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
			ClientToken:  aws.String(token),
		}
	}

	const tokenA = "integration-test-clienttoken-a"
	res1, err := gw.EC2Client(t).RunInstances(baseInput(tokenA))
	require.NoError(t, err, "first launch with token A")
	require.Len(t, res1.Instances, 1)
	require.Equal(t, int64(1), dispatchCount.Load(), "first call must dispatch to the daemon once")
	firstInstanceID := aws.StringValue(res1.Instances[0].InstanceId)
	firstReservationID := aws.StringValue(res1.ReservationId)

	// Replay: same token, same params. Must return the identical reservation
	// without a second daemon dispatch.
	res2, err := gw.EC2Client(t).RunInstances(baseInput(tokenA))
	require.NoError(t, err, "replay with token A")
	require.Equal(t, int64(1), dispatchCount.Load(), "replaying the same token must not dispatch again")
	require.Equal(t, firstReservationID, aws.StringValue(res2.ReservationId), "replay must return the original reservation")
	require.Len(t, res2.Instances, 1)
	require.Equal(t, firstInstanceID, aws.StringValue(res2.Instances[0].InstanceId), "replay must return the original instance")

	// A different token for the same account must launch again, independently.
	const tokenB = "integration-test-clienttoken-b"
	res3, err := gw.EC2Client(t).RunInstances(baseInput(tokenB))
	require.NoError(t, err, "launch with token B")
	require.Equal(t, int64(2), dispatchCount.Load(), "a different token must dispatch a fresh launch")
	require.Len(t, res3.Instances, 1)
	require.NotEqual(t, firstInstanceID, aws.StringValue(res3.Instances[0].InstanceId),
		"a different token must not replay token A's instance")
}
