package accountlifecycle

//test:in-package — the instance reaper is unexported, and whether its listing
// can be mistaken for an empty account is the safety property under test.

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

var inventoryNodes = []string{"node-1", "node-2"}

// answerAsNode replies on subject the way a daemon does, carrying the node ID
// header the fan-out attributes frames by.
func answerAsNode(t *testing.T, nc *nats.Conn, subject, nodeID string, payload []byte) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		reply := nats.NewMsg(msg.Reply)
		reply.Data = payload
		if nodeID != "" {
			reply.Header.Set(natsmsg.NodeIDHeader, nodeID)
		}
		_ = msg.RespondMsg(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func describeOutput(t *testing.T, instanceIDs ...string) []byte {
	t.Helper()
	out := ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}
	if len(instanceIDs) > 0 {
		reservation := &ec2.Reservation{ReservationId: aws.String("r-" + instanceIDs[0])}
		for _, id := range instanceIDs {
			reservation.Instances = append(reservation.Instances, &ec2.Instance{
				InstanceId: aws.String(id),
				State:      &ec2.InstanceState{Name: aws.String("running")},
			})
		}
		out.Reservations = append(out.Reservations, reservation)
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)
	return payload
}

// emptyInstanceBuckets answers the stopped and terminated bucket queries with
// nothing, so only the running-instance fan-out decides completeness.
func emptyInstanceBuckets(t *testing.T, nc *nats.Conn) {
	t.Helper()
	answerAsNode(t, nc, "ec2.DescribeStoppedInstances", "", describeOutput(t))
	answerAsNode(t, nc, "ec2.DescribeTerminatedInstances", "", describeOutput(t))
}

