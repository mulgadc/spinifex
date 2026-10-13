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

	ids := []string{"i-06794c450e770fbc0", "i-70aee04c61ac4d963"}

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

func TestRebootInstances_NeverExistedIsNotFound(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	serveEmptyBuckets(t, nc)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String("i-d2d5fe795b6b57882")},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestRebootInstances_BucketsUnreachableIsServerError(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String("i-0cc1dd2ee3ff40516")},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestRebootInstances_TerminatedInstanceIsIncorrectState(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	instanceID := "i-0bb1cc2dd3ee4ff50"
	serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances", instanceID)
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")

	input := &ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}

	_, err := RebootInstances(context.Background(), input, nc, "123456789012")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectInstanceState, err.Error())
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

	instanceID := "i-6ed2716331719b275"
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

	instanceID := "i-42798f10e29985019"

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
			instanceID := "i-156a87e2dd55d1967"

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
