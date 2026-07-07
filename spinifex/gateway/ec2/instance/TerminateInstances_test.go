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

func TestTerminateInstances_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-0123456789abcdef0"

	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		var cmd types.EC2InstanceCommand
		err := json.Unmarshal(msg.Data, &cmd)
		require.NoError(t, err)

		assert.Equal(t, instanceID, cmd.ID)
		assert.True(t, cmd.Attributes.StopInstance)
		assert.True(t, cmd.Attributes.TerminateInstance)

		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := TerminateInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.TerminatingInstances, 1)

	sc := output.TerminatingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(32), *sc.CurrentState.Code)
	assert.Equal(t, "shutting-down", *sc.CurrentState.Name)
	assert.Equal(t, int64(16), *sc.PreviousState.Code)
	assert.Equal(t, "running", *sc.PreviousState.Name)
}

func TestTerminateInstances_MultipleInstances(t *testing.T) {
	_, nc := startTestNATSServer(t)

	ids := []string{"i-001", "i-002", "i-003"}

	for _, id := range ids {
		nc.Subscribe("ec2.cmd."+id, func(msg *nats.Msg) {
			msg.Respond([]byte(`{"return":{}}`))
		})
	}

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(ids[0]), aws.String(ids[1]), aws.String(ids[2])},
	}

	output, err := TerminateInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.TerminatingInstances, 3)

	for i, sc := range output.TerminatingInstances {
		assert.Equal(t, ids[i], *sc.InstanceId)
		assert.Equal(t, "shutting-down", *sc.CurrentState.Name)
		assert.Equal(t, "running", *sc.PreviousState.Name)
	}
}

func TestTerminateInstances_EmptyInstanceIds(t *testing.T) {
	_, nc := startTestNATSServer(t)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := TerminateInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestTerminateInstances_NilInstanceIdSkipped(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-valid"
	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{nil, aws.String(instanceID), nil},
	}

	output, err := TerminateInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	assert.Len(t, output.TerminatingInstances, 1)
	assert.Equal(t, instanceID, *output.TerminatingInstances[0].InstanceId)
}

func TestTerminateInstances_NATSRequestFails(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-nosubscriber"

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	// When no subscriber exists and all fallback paths fail, an error is
	// returned so the caller knows the terminate did not succeed.
	_, err := TerminateInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Contains(t, err.Error(), instanceID)
}

func TestTerminateInstances_MixedSuccessAndFailure(t *testing.T) {
	_, nc := startTestNATSServer(t)

	goodID := "i-good"
	badID := "i-bad"

	nc.Subscribe("ec2.cmd."+goodID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(goodID), aws.String(badID)},
	}

	// When a running instance cannot be reached (ErrNoResponders exhausted after
	// retries), TerminateInstances returns an error rather than silently
	// reporting the instance as "running".
	_, err := TerminateInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Contains(t, err.Error(), badID)
}

func TestTerminateInstances_VerifiesQMPAttributes(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-verify"
	var receivedCmd types.EC2InstanceCommand

	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		json.Unmarshal(msg.Data, &receivedCmd)
		msg.Respond([]byte(`{"return":{}}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	_, err := TerminateInstances(context.Background(), input, nc, "123456789012")
	require.NoError(t, err)

	// Terminate should set both stop and terminate flags
	assert.True(t, receivedCmd.Attributes.StopInstance, "StopInstance should be true for terminate")
	assert.True(t, receivedCmd.Attributes.TerminateInstance, "TerminateInstance should be true")
	assert.False(t, receivedCmd.Attributes.StartInstance, "StartInstance should be false")
}

func TestTerminateInstances_StoppedInstanceFallback(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-stopped-term"

	// No ec2.cmd.<id> subscriber — simulate stopped instance with no daemon owning it.
	// Subscribe to ec2.terminate to handle the fallback.
	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		var req terminateStoppedInstanceRequest
		json.Unmarshal(msg.Data, &req)
		assert.Equal(t, instanceID, req.InstanceID)
		msg.Respond([]byte(`{"status":"terminated","instanceId":"` + instanceID + `"}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := TerminateInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.TerminatingInstances, 1)

	sc := output.TerminatingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(32), *sc.CurrentState.Code)
	assert.Equal(t, "shutting-down", *sc.CurrentState.Name)
	assert.Equal(t, int64(80), *sc.PreviousState.Code)
	assert.Equal(t, "stopped", *sc.PreviousState.Name)
}

func TestTerminateInstances_MixedRunningAndStopped(t *testing.T) {
	_, nc := startTestNATSServer(t)

	runningID := "i-running-mix"
	stoppedID := "i-stopped-mix"

	// Running instance responds on ec2.cmd.<id>
	nc.Subscribe("ec2.cmd."+runningID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{"return":{}}`))
	})

	// Stopped instance: no ec2.cmd subscriber, but ec2.terminate responds
	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		var req terminateStoppedInstanceRequest
		json.Unmarshal(msg.Data, &req)
		msg.Respond([]byte(`{"status":"terminated","instanceId":"` + req.InstanceID + `"}`))
	})

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(runningID), aws.String(stoppedID)},
	}

	output, err := TerminateInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.TerminatingInstances, 2)

	// Running instance: previous=running, current=shutting-down
	assert.Equal(t, runningID, *output.TerminatingInstances[0].InstanceId)
	assert.Equal(t, int64(32), *output.TerminatingInstances[0].CurrentState.Code)
	assert.Equal(t, "shutting-down", *output.TerminatingInstances[0].CurrentState.Name)
	assert.Equal(t, int64(16), *output.TerminatingInstances[0].PreviousState.Code)
	assert.Equal(t, "running", *output.TerminatingInstances[0].PreviousState.Name)

	// Stopped instance: previous=stopped, current=shutting-down
	assert.Equal(t, stoppedID, *output.TerminatingInstances[1].InstanceId)
	assert.Equal(t, int64(32), *output.TerminatingInstances[1].CurrentState.Code)
	assert.Equal(t, "shutting-down", *output.TerminatingInstances[1].CurrentState.Name)
	assert.Equal(t, int64(80), *output.TerminatingInstances[1].PreviousState.Code)
	assert.Equal(t, "stopped", *output.TerminatingInstances[1].PreviousState.Name)
}
