package instance

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetInstanceUefiData(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	subscribeEmptyInstanceBuckets(t, nc)
	const known = "i-0123456789abcdef0"
	data, err := json.Marshal(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{
		ReservationId: aws.String("r-1"),
		Instances:     []*ec2.Instance{{InstanceId: aws.String(known)}},
	}}})
	require.NoError(t, err)
	subscribeAsNode(t, nc, "ec2.DescribeInstances", "node-1", data)

	call := func(id string) (*ec2.GetInstanceUefiDataOutput, error) {
		return GetInstanceUefiData(context.Background(), &ec2.GetInstanceUefiDataInput{InstanceId: aws.String(id)}, nc, []string{"node-1"}, "123456789012")
	}

	_, err = call("ami-0123456789abcdef0")
	code, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, code)
	assert.Equal(t, `Invalid id: "ami-0123456789abcdef0"`, msg)

	_, err = call("i-0123456789abcdef1")
	code, _, ok = awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "err = %v", err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, code)

	out, err := call(known)
	require.NoError(t, err)
	assert.Equal(t, known, aws.StringValue(out.InstanceId))
	assert.Nil(t, out.UefiData)

	_, err = GetInstanceUefiData(context.Background(), &ec2.GetInstanceUefiDataInput{}, nc, nil, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}
