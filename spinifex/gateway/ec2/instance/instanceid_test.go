package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveInstanceBucket answers topic with a describe output holding ids, the
// shape the daemon's stopped/terminated bucket handlers reply with.
func serveInstanceBucket(t *testing.T, nc *nats.Conn, topic string, ids ...string) {
	t.Helper()
	var instances []*ec2.Instance
	for _, id := range ids {
		instances = append(instances, &ec2.Instance{InstanceId: aws.String(id)})
	}
	out := ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{}}
	if len(instances) > 0 {
		out.Reservations = append(out.Reservations, &ec2.Reservation{Instances: instances})
	}
	data, err := json.Marshal(out)
	require.NoError(t, err)
	_, err = nc.Subscribe(topic, func(msg *nats.Msg) { _ = msg.Respond(data) })
	require.NoError(t, err)
}

// serveEmptyBuckets makes both instance buckets reachable and empty, the only
// condition under which an unowned instance may be reported NotFound.
func serveEmptyBuckets(t *testing.T, nc *nats.Conn) {
	t.Helper()
	serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
	serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
}

func TestValidateInstanceIDFormat(t *testing.T) {
	t.Parallel()
	valid := []string{"i-0123456789abcdef0", "i-1a2b3c4d"}
	for _, id := range valid {
		assert.NoError(t, validateInstanceIDFormat([]*string{aws.String(id)}), id)
	}
	malformed := []string{
		"i-9decec9b6a07590efasdasdsd",
		"i-abc",
		"i-0123456789ABCDEF0",
		"i-0123456789abcdef",
		"ami-bb529e224722bd834",
		"0123456789abcdef0",
		"",
	}
	for _, id := range malformed {
		err := validateInstanceIDFormat([]*string{aws.String(id)})
		require.Error(t, err, id)
		assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error(), id)
	}
	err := validateInstanceIDFormat([]*string{aws.String("i-0123456789abcdef0"), aws.String("i-abc")})
	require.Error(t, err, "one malformed id fails the whole batch")
	assert.NoError(t, validateInstanceIDFormat([]*string{nil, aws.String("i-0123456789abcdef0")}))
}

// A malformed id must be rejected before the gateway asks any daemon about it.
func TestLifecycleVerbs_MalformedIDRejectedBeforeNATS(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	var requests atomic.Int32
	_, err := nc.Subscribe("ec2.>", func(msg *nats.Msg) {
		requests.Add(1)
		_ = msg.Respond([]byte(`{}`))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	ids := []*string{aws.String("i-0123456789abcdef0"), aws.String("i-9decec9b6a07590efasdasdsd")}
	ctx := context.Background()
	calls := map[string]func() error{
		"terminate": func() error {
			_, err := TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}, nc, "123456789012")
			return err
		},
		"stop": func() error {
			_, err := StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: ids}, nc, "123456789012")
			return err
		},
		"start": func() error {
			_, err := StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: ids}, nc, "123456789012")
			return err
		},
		"reboot": func() error {
			_, err := RebootInstances(ctx, &ec2.RebootInstancesInput{InstanceIds: ids}, nc, "123456789012")
			return err
		},
	}
	for name, call := range calls {
		err := call()
		require.Error(t, err, name)
		assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error(), name)
	}
	require.NoError(t, nc.Flush())
	assert.Zero(t, requests.Load(), "no NATS request may be sent for a batch holding a malformed id")
}

func TestClassifyUnownedInstance(t *testing.T) {
	t.Parallel()
	const id = "i-0123456789abcdef0"
	ctx := context.Background()

	t.Run("terminated", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances", id)
		serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
		state, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.Equal(t, bucketStateTerminated, state)
		assert.True(t, ok)
	})
	t.Run("stopped", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
		serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances", id)
		state, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.Equal(t, bucketStateStopped, state)
		assert.True(t, ok)
	})
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveEmptyBuckets(t, nc)
		state, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.Equal(t, bucketStateAbsent, state)
		assert.True(t, ok)
	})
	t.Run("another instance in the bucket is not a hit", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances", "i-0fedcba9876543210")
		serveInstanceBucket(t, nc, "ec2.DescribeStoppedInstances")
		state, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.Equal(t, bucketStateAbsent, state)
		assert.True(t, ok)
	})
	t.Run("one bucket unreachable cannot prove absence", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
		state, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.Equal(t, bucketStateAbsent, state)
		assert.False(t, ok)
	})
	t.Run("bucket error payload cannot prove absence", func(t *testing.T) {
		t.Parallel()
		_, nc := startTestNATSServer(t)
		serveInstanceBucket(t, nc, "ec2.DescribeTerminatedInstances")
		_, err := nc.Subscribe("ec2.DescribeStoppedInstances", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"code":"ServerInternal","message":"kv down"}`))
		})
		require.NoError(t, err)
		_, ok := classifyUnownedInstance(ctx, nc, id, "123456789012")
		assert.False(t, ok)
	})
}
