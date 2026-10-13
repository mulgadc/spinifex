package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopInstances_Success(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0123456789abcdef0"

	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		var cmd types.EC2InstanceCommand
		err := json.Unmarshal(msg.Data, &cmd)
		require.NoError(t, err)

		assert.Equal(t, instanceID, cmd.ID)
		assert.True(t, cmd.Attributes.StopInstance)
		assert.False(t, cmd.Attributes.TerminateInstance)

		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := StopInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.StoppingInstances, 1)

	sc := output.StoppingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(64), *sc.CurrentState.Code)
	assert.Equal(t, "stopping", *sc.CurrentState.Name)
	assert.Equal(t, int64(16), *sc.PreviousState.Code)
	assert.Equal(t, "running", *sc.PreviousState.Name)
}

func TestStopInstances_MultipleInstances(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	ids := []string{"i-06794c450e770fbc0", "i-70aee04c61ac4d963"}

	for _, id := range ids {
		nc.Subscribe("ec2.cmd."+id, func(msg *nats.Msg) {
			msg.Respond([]byte(`{"return":{}}`))
		})
	}

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(ids[0]), aws.String(ids[1])},
	}

	output, err := StopInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.StoppingInstances, 2)

	for i, sc := range output.StoppingInstances {
		assert.Equal(t, ids[i], *sc.InstanceId)
		assert.Equal(t, "stopping", *sc.CurrentState.Name)
		assert.Equal(t, "running", *sc.PreviousState.Name)
	}
}

func TestStopInstances_EmptyInstanceIds(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := StopInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestStopInstances_NilInstanceIdSkipped(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-476e190637b456de0"
	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{nil, aws.String(instanceID)},
	}

	output, err := StopInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	assert.Len(t, output.StoppingInstances, 1)
	assert.Equal(t, instanceID, *output.StoppingInstances[0].InstanceId)
}

// The reported bug: an id the system never issued was answered with a
// fabricated running/running state change instead of NotFound.
func TestStopInstances_NeverExistedIsNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveEmptyBuckets(t, nc)

	output, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String("i-d2d5fe795b6b57882")},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, output)
}

// Stopping an already-stopped instance is a successful no-op on AWS.
func TestStopInstances_StoppedInstanceIsNoOp(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0aa1bb2cc3dd4ee5f"
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances", instanceID)
	serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")

	output, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.StoppingInstances, 1)
	sc := output.StoppingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(80), *sc.CurrentState.Code)
	assert.Equal(t, "stopped", *sc.CurrentState.Name)
	assert.Equal(t, int64(80), *sc.PreviousState.Code)
	assert.Equal(t, "stopped", *sc.PreviousState.Name)
}

func TestStopInstances_TerminatedInstanceIsIncorrectState(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0bb1cc2dd3ee4ff50"
	serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances", instanceID)
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")

	_, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestStopInstances_BucketsUnreachableIsServerError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	output, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String("i-0cc1dd2ee3ff40516")},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Nil(t, output)
}

// A live owner that does not answer in time is not evidence the instance is
// missing, and must not be reported as a state change either.
func TestStopInstances_SlowOwnerIsServerError(t *testing.T) {
	_, nc := startTestNATSServer(t)

	previous := instanceCmdTimeout
	instanceCmdTimeout = 50 * time.Millisecond
	t.Cleanup(func() { instanceCmdTimeout = previous })

	instanceID := "i-0dd1ee2ff30415263"
	_, err := nc.Subscribe("ec2.cmd."+instanceID, func(*nats.Msg) {})
	require.NoError(t, err)
	serveEmptyBuckets(t, nc)

	output, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Nil(t, output)
}

func TestStopInstances_MixedBatchWithUnknownIDFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	goodID := "i-fd00438116c1f2ce2"
	unknownID := "i-587b21c4147f9d9d8"

	nc.Subscribe("ec2.cmd."+goodID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})
	serveEmptyBuckets(t, nc)

	output, err := StopInstances(context.Background(), &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(goodID), aws.String(unknownID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, output)
}
