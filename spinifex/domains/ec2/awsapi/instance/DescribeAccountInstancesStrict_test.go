package instance

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strictInstance(id, state string) *ec2.Instance {
	return &ec2.Instance{InstanceId: aws.String(id), State: &ec2.InstanceState{Name: aws.String(state)}}
}

func strictReservations(instances ...*ec2.Instance) *ec2.DescribeInstancesOutput {
	out := &ec2.DescribeInstancesOutput{}
	for _, inst := range instances {
		out.Reservations = append(out.Reservations, &ec2.Reservation{
			ReservationId: aws.String("r-" + aws.StringValue(inst.InstanceId)),
			OwnerId:       aws.String("123456789012"),
			Instances:     []*ec2.Instance{inst},
		})
	}
	return out
}

func subscribeAccountRecords(t *testing.T, nc *nats.Conn, data []byte) {
	t.Helper()
	_, err := nc.Subscribe(accountInstanceRecordsSubject, func(msg *nats.Msg) { _ = msg.Respond(data) })
	require.NoError(t, err)
}

const strictAccount = "123456789012"

func strictList(nc *nats.Conn, nodes ...string) (*ec2.DescribeInstancesOutput, error) {
	return DescribeAccountInstancesStrict(context.Background(), nc, nodes, strictAccount, WithFanoutTimeout(2*time.Second))
}

// Existence comes from the record store, so an instance no node holds is still
// listed, with the state the record carries.
func TestDescribeAccountInstancesStrict_UnheldRecordIsReturnedWithItsRecordedState(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-unheld", "stopped"))))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	inst := out.Reservations[0].Instances[0]
	assert.Equal(t, "i-unheld", aws.StringValue(inst.InstanceId))
	assert.Equal(t, "stopped", aws.StringValue(inst.State.Name))
}

// A node's answer replaces the recorded view of the instance it holds, and an
// instance only a node reports is added rather than dropped.
func TestDescribeAccountInstancesStrict_NodeAnswersAreJoinedOntoRecords(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-held", "pending"))))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", mustMarshalDescribeOutput(t,
		strictReservations(strictInstance("i-held", "running"), strictInstance("i-node-only", "running"))))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	require.NoError(t, err)
	states := map[string]string{}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			states[aws.StringValue(inst.InstanceId)] = aws.StringValue(inst.State.Name)
		}
	}
	assert.Equal(t, map[string]string{"i-held": "running", "i-node-only": "running"}, states)
}

// A frame from a node outside the configured set means the expected set is
// wrong, and a listing built on it could omit that node's instances.
func TestDescribeAccountInstancesStrict_FrameFromUnconfiguredNodeFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-stray", mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-on-stray", "running"))))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}

func TestDescribeAccountInstancesStrict_ConfiguredNodeSilentFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc, "node-1", "node-2")

	assertIncompleteListing(t, out, err)
}

func TestDescribeAccountInstancesStrict_RecordStoreErrorFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}

func TestDescribeAccountInstancesStrict_RecordStoreSilentFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}

func TestDescribeAccountInstancesStrict_UnknownNodeSetFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc)

	assertIncompleteListing(t, out, err)
}

func TestDescribeAccountInstancesStrict_UnattributedFrameFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "", []byte(`{}`))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}
