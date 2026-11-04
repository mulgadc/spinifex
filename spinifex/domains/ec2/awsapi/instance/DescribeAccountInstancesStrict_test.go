package instance

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
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

// A node's answer replaces the recorded view of the instance it holds.
func TestDescribeAccountInstancesStrict_NodeAnswersAreJoinedOntoRecords(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-held", "pending"))))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", mustMarshalDescribeOutput(t,
		strictReservations(strictInstance("i-held", "running"))))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	require.NoError(t, err)
	states := map[string]string{}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			states[aws.StringValue(inst.InstanceId)] = aws.StringValue(inst.State.Name)
		}
	}
	assert.Equal(t, map[string]string{"i-held": "running"}, states)
}

// An instance a node reports with no record can come from a lost record write.
// Returning it or dropping it would both answer for an unproven account.
func TestDescribeAccountInstancesStrict_NodeOnlyInstanceFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-held", "running"))))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", mustMarshalDescribeOutput(t,
		strictReservations(strictInstance("i-held", "running"), strictInstance("i-node-only", "running"))))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	require.Error(t, err)
	assert.Nil(t, out)
	code, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, code)
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

// A stray node that answers with an error is still a node outside the set, and
// its instances are unaccounted for.
func TestDescribeAccountInstancesStrict_ErrorFromUnconfiguredNodeFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-stray", awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal))
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}

// The configured nodes answer at once, so a reply that lands later but inside
// the fan-out deadline must still be seen.
func TestDescribeAccountInstancesStrict_LateFrameFromUnconfiguredNodeFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations()))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
	late := mustMarshalDescribeOutput(t, strictReservations(strictInstance("i-on-stray", "running")))
	_, err := nc.Subscribe("ec2.DescribeInstances", func(msg *nats.Msg) {
		time.AfterFunc(600*time.Millisecond, func() {
			reply := nats.NewMsg(msg.Reply)
			reply.Data = late
			reply.Header.Set(natsmsg.NodeIDHeader, "node-stray")
			_ = msg.RespondMsg(reply)
		})
	})
	require.NoError(t, err)
	nc.Flush()

	out, err := strictList(nc, "node-1")

	assertIncompleteListing(t, out, err)
}

// A record with no instance ID cannot be listed or counted, so it must fail
// the read rather than vanish from it.
func TestDescribeAccountInstancesStrict_RecordWithoutInstanceIDFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeAccountRecords(t, nc, mustMarshalDescribeOutput(t, strictReservations(&ec2.Instance{State: &ec2.InstanceState{Name: aws.String("stopped")}})))
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", []byte(`{}`))
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
