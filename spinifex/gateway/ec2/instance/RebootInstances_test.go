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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebootInstances_Success(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0123456789abcdef0"

	nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		var cmd types.EC2InstanceCommand
		err := json.Unmarshal(msg.Data, &cmd)
		require.NoError(t, err)

		assert.Equal(t, instanceID, cmd.ID)
		assert.True(t, cmd.Attributes.RebootInstance)

		msg.Respond([]byte(`{}`))
	})

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	output, err := RebootInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
}

func TestRebootInstances_MultipleInstances(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	ids := []string{"i-001", "i-002"}

	for _, id := range ids {
		nc.Subscribe("ec2.cmd."+id, func(msg *nats.Msg) {
			msg.Respond([]byte(`{}`))
		})
	}

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(ids[0]), aws.String(ids[1])},
	}

	output, err := RebootInstances(context.Background(), input, nc, "123456789012")

	require.NoError(t, err)
	require.NotNil(t, output)
}

func TestRebootInstances_EmptyInstanceIds(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestRebootInstances_NATSRequestFails(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String("i-nosubscriber")},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// TestRebootInstances_SlowDaemonIsNotAMissingInstance covers a daemon that
// holds the request open. Reporting that as NotFound names a fault the caller
// does not have and hides the one it does: the instance is there, the node
// answering for it is not answering.
func TestRebootInstances_SlowDaemonIsNotAMissingInstance(t *testing.T) {
	_, nc := startTestNATSServer(t)

	previous := rebootInstancesTimeout
	rebootInstancesTimeout = 50 * time.Millisecond
	t.Cleanup(func() { rebootInstancesTimeout = previous })

	instanceID := "i-slow"
	// A live subscriber that never replies forces a real ErrTimeout rather
	// than the immediate ErrNoResponders a missing subscriber would give.
	_, err := nc.Subscribe("ec2.cmd."+instanceID, func(*nats.Msg) {})
	require.NoError(t, err)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}
	_, err = RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestRebootInstances_StoppedInstance(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-stopped"

	// No ec2.cmd subscription — simulates a stopped instance with no daemon listener.
	// Subscribe to the stopped-instances describe topic to return the instance.
	nc.Subscribe("ec2.DescribeStoppedInstances", func(msg *nats.Msg) {
		resp := ec2.DescribeInstancesOutput{
			Reservations: []*ec2.Reservation{
				{Instances: []*ec2.Instance{{InstanceId: aws.String(instanceID)}}},
			},
		}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
}

func TestRebootInstances_DaemonError(t *testing.T) {
	t.Parallel()
	for _, code := range []string{
		awserrors.ErrorIncorrectInstanceState,
		awserrors.ErrorServerInternal,
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			_, nc := startTestNATSServer(t)
			instanceID := "i-error"

			nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
				msg.Respond(utils.GenerateErrorPayload(code))
			})

			input := &ec2.RebootInstancesInput{
				InstanceIds: []*string{aws.String(instanceID)},
			}
			_, err := RebootInstances(context.Background(), input, nc, "123456789012")
			require.Error(t, err)
			assert.Equal(t, code, err.Error())
		})
	}
}
