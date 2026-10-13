package instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartInstances_Success(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0123456789abcdef0"

	// Mock subscriber for the ec2.start queue group topic
	nc.QueueSubscribe("ec2.start", "spinifex-workers", func(msg *nats.Msg) {
		var req startStoppedInstanceRequest
		err := json.Unmarshal(msg.Data, &req)
		require.NoError(t, err)

		assert.Equal(t, instanceID, req.InstanceID)

		msg.Respond([]byte(`{"status":"running","instanceId":"` + instanceID + `"}`))
	})

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := StartInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	require.Len(t, output.StartingInstances, 1)

	sc := output.StartingInstances[0]
	assert.Equal(t, instanceID, *sc.InstanceId)
	assert.Equal(t, int64(0), *sc.CurrentState.Code)
	assert.Equal(t, "pending", *sc.CurrentState.Name)
	assert.Equal(t, int64(80), *sc.PreviousState.Code)
	assert.Equal(t, "stopped", *sc.PreviousState.Name)
}

func TestStartInstances_MultipleInstances(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	ids := []string{"i-06794c450e770fbc0", "i-70aee04c61ac4d963", "i-ed828a4ce70b0199c"}

	// Single subscriber handles all start requests via queue group
	nc.QueueSubscribe("ec2.start", "spinifex-workers", func(msg *nats.Msg) {
		var req startStoppedInstanceRequest
		json.Unmarshal(msg.Data, &req)
		msg.Respond([]byte(`{"status":"running","instanceId":"` + req.InstanceID + `"}`))
	})

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(ids[0]), aws.String(ids[1]), aws.String(ids[2])},
	}

	output, err := StartInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	assert.Len(t, output.StartingInstances, 3)

	for i, sc := range output.StartingInstances {
		assert.Equal(t, ids[i], *sc.InstanceId)
		assert.Equal(t, "pending", *sc.CurrentState.Name)
	}
}

func TestStartInstances_EmptyInstanceIds(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := StartInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestStartInstances_NilInstanceIdSkipped(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-476e190637b456de0"
	nc.QueueSubscribe("ec2.start", "spinifex-workers", func(msg *nats.Msg) {
		msg.Respond([]byte(`{"status":"running","instanceId":"` + instanceID + `"}`))
	})

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{nil, aws.String(instanceID), nil},
	}

	output, err := StartInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
	// Only the valid ID should produce a state change
	assert.Len(t, output.StartingInstances, 1)
	assert.Equal(t, instanceID, *output.StartingInstances[0].InstanceId)
}

func TestStartInstances_NATSRequestFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// No subscriber for ec2.start, so NATS request will fail
	instanceID := "i-d2d5fe795b6b57882"

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := StartInstances(context.Background(), input, nc, "123456789012")

	// No daemon answered, so nothing is known about the instance: neither a
	// fabricated stopped/stopped change nor NotFound.
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Nil(t, output)
}

func TestStartInstances_MixedSuccessAndFailure(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	goodID := "i-fd00438116c1f2ce2"
	badID := "i-587b21c4147f9d9d8"

	// Subscribe to ec2.start but only respond for goodID
	nc.QueueSubscribe("ec2.start", "spinifex-workers", func(msg *nats.Msg) {
		var req startStoppedInstanceRequest
		json.Unmarshal(msg.Data, &req)
		if req.InstanceID == goodID {
			msg.Respond([]byte(`{"status":"running","instanceId":"` + goodID + `"}`))
		} else {
			// Simulate error for bad instance
			msg.Respond([]byte(`{"Code":"InvalidInstanceID.NotFound"}`))
		}
	})

	input := &ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(goodID), aws.String(badID)},
	}

	_, err := StartInstances(context.Background(), input, nc, "123456789012")

	// Daemon error for any instance should fail the whole call
	require.Error(t, err)
	assert.Equal(t, "InvalidInstanceID.NotFound", err.Error())
}
