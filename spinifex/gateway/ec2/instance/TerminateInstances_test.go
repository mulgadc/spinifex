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
	t.Parallel()
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
	t.Parallel()
	_, nc := startTestNATSServer(t)

	ids := []string{"i-06794c450e770fbc0", "i-70aee04c61ac4d963", "i-ed828a4ce70b0199c"}

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
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.TerminateInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := TerminateInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestTerminateInstances_NilInstanceIdSkipped(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-476e190637b456de0"
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

	instanceID := "i-d2d5fe795b6b57882"

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

	goodID := "i-fd00438116c1f2ce2"
	badID := "i-587b21c4147f9d9d8"

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
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-aaca8ef21b7bcf3c1"
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

	instanceID := "i-b727f5c80b8852641"

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

	runningID := "i-eedceec2e5f2de227"
	stoppedID := "i-8d23f24b13d54a30c"

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

// A refusal that is not an absence means the terminate did not happen, so
// reporting "terminated" for it would strand a record the caller believes is
// gone and leave a Terraform destroy waiting on it forever.
func TestTerminateInstances_RefusalIsNotReportedAsTerminated(t *testing.T) {
	_, nc := startTestNATSServer(t)

	instanceID := "i-7488538f97c0a8654"

	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"code":"` + awserrors.ErrorIncorrectInstanceState + `","message":"wrong state"}`))
	})
	// A reachable terminated bucket that does not hold the instance is what
	// makes the idempotent-absence branch look applicable when it is not.
	nc.Subscribe("ec2.DescribeTerminatedInstances", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"Reservations":[]}`))
	})

	output, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
	assert.Nil(t, output)
}

// The reported bug: an id the system never issued was answered with a
// fabricated terminated/terminated state change instead of NotFound.
func TestTerminateInstances_NeverExistedIsNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-d419ad3c922169e6e"

	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"code":"` + awserrors.ErrorInvalidInstanceIDNotFound + `","message":"not found"}`))
	})
	serveEmptyBuckets(t, nc)

	output, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, output)
}

// A repeat terminate of an instance still in the terminated bucket converges,
// which is what lets a retried destroy finish.
func TestTerminateInstances_AlreadyTerminatedStaysIdempotent(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0aa1bb2cc3dd4ee5f"

	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"code":"` + awserrors.ErrorInvalidInstanceIDNotFound + `","message":"not found"}`))
	})
	serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances", instanceID)
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")

	output, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.NoError(t, err)
	require.Len(t, output.TerminatingInstances, 1)
	sc := output.TerminatingInstances[0]
	assert.Equal(t, int64(48), *sc.CurrentState.Code)
	assert.Equal(t, "terminated", *sc.CurrentState.Name)
	assert.Equal(t, int64(48), *sc.PreviousState.Code)
	assert.Equal(t, "terminated", *sc.PreviousState.Name)
}

// With the buckets unreadable absence cannot be proved, so the call fails
// with neither a fabricated terminate nor a NotFound.
func TestTerminateInstances_BucketsUnreachableIsNeitherSuccessNorNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0bb1cc2dd3ee4ff50"

	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"code":"` + awserrors.ErrorInvalidInstanceIDNotFound + `","message":"not found"}`))
	})
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")

	output, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, output)
}

// NotFound needs the stopped-instance path's own NotFound. When ec2.terminate
// does not answer, empty buckets alone could be hiding a stopped instance.
func TestTerminateInstances_NoStoppedPathAnswerIsNotNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	serveEmptyBuckets(t, nc)

	_, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String("i-0cc1dd2ee3ff40516")},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestTerminateInstances_MixedBatchWithUnknownIDFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	runningID := "i-0dd1ee2ff30415263"
	unknownID := "i-0ee1ff203142536a7"

	nc.Subscribe("ec2.cmd."+runningID, func(msg *nats.Msg) {
		msg.Respond([]byte(`{}`))
	})
	nc.QueueSubscribe("ec2.terminate", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"code":"` + awserrors.ErrorInvalidInstanceIDNotFound + `","message":"not found"}`))
	})
	serveEmptyBuckets(t, nc)

	output, err := TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(runningID), aws.String(unknownID)},
	}, nc, "123456789012")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, output)
}
