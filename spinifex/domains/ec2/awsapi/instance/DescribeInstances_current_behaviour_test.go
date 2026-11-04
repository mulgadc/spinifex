package instance

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pins today: the completeness judge merges the reservations of a frame from a
// node outside the configured set and still reports the listing complete. The
// stray node is never named, so a silent unconfigured node cannot be told from
// one that does not exist.
func TestCurrentBehaviour_JudgeIdentityCompleteness_FrameFromUnconfiguredNodeIsMerged(t *testing.T) {
	t.Parallel()
	stray := mustMarshalDescribeOutput(t, &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{ReservationId: aws.String("r-stray"), Instances: []*ec2.Instance{{InstanceId: aws.String("i-on-stray")}}}},
	})
	frames := []natsmsg.Frame{
		{NodeID: "node-1", Data: []byte(`{}`)},
		{NodeID: "node-stray", Data: stray},
	}

	reservations, complete := judgeIdentityCompleteness(context.Background(), frames, natsmsg.Summary{}, []string{"node-1"})

	assert.True(t, complete, "CURRENT: a node outside nodeIDs does not make the listing incomplete")
	require.Len(t, reservations, 1)
	assert.Equal(t, "i-on-stray", *reservations[0].Instances[0].InstanceId,
		"CURRENT: the unconfigured node's instances are merged into the result")
}

// Pins today: the fan-out counts every identified frame toward the expected
// responders, so a stray frame can end collection before a configured node has
// answered. The listing then refuses, which fails closed, but for a node that
// was only slow.
func TestCurrentBehaviour_DescribeAccountInstancesComplete_StrayFrameConsumesAResponderSlot(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-stray", []byte(`{}`))
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		time.AfterFunc(500*time.Millisecond, func() {
			reply := nats.NewMsg(msg.Reply)
			reply.Data = []byte(`{}`)
			reply.Header.Set(natsmsg.NodeIDHeader, "node-2")
			_ = msg.RespondMsg(reply)
		})
	})
	require.NoError(t, err)
	nc.Flush()

	output, err := DescribeAccountInstancesComplete(context.Background(), nc, []string{"node-1", "node-2"}, "123456789012",
		WithFanoutTimeout(3*time.Second))

	assertIncompleteListing(t, output, err)
}
