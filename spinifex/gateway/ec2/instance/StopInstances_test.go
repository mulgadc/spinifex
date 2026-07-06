package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopInstances_Success(t *testing.T) {
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
	_, nc := startTestNATSServer(t)

	ids := []string{"i-001", "i-002"}

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
	_, nc := startTestNATSServer(t)

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := StopInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestStopInstances_NilInstanceIdSkipped(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-valid"
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

func TestStopInstances_NATSRequestFails(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-nosubscriber"

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := StopInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.StoppingInstances, 1)

	// On NATS failure, state should reflect "still running"
	sc := output.StoppingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(16), *sc.CurrentState.Code)
	assert.Equal(t, "running", *sc.CurrentState.Name)
	assert.Equal(t, int64(16), *sc.PreviousState.Code)
	assert.Equal(t, "running", *sc.PreviousState.Name)
}

func TestStopInstances_MixedSuccessAndFailure(t *testing.T) {
	_, nc := startTestNATSServer(t)

	goodID := "i-good"
	badID := "i-bad"

	nc.Subscribe("ec2.cmd."+goodID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(goodID), aws.String(badID)},
	}

	output, err := StopInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.StoppingInstances, 2)

	// First: success → stopping
	assert.Equal(t, goodID, *output.StoppingInstances[0].InstanceId)
	assert.Equal(t, "stopping", *output.StoppingInstances[0].CurrentState.Name)

	// Second: failure → still running
	assert.Equal(t, badID, *output.StoppingInstances[1].InstanceId)
	assert.Equal(t, "running", *output.StoppingInstances[1].CurrentState.Name)
}
