package handlers_imds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstInstance_NilAndEmpty(t *testing.T) {
	assert.Nil(t, firstInstance(nil))
	assert.Nil(t, firstInstance(&ec2.DescribeInstancesOutput{}))
	assert.Nil(t, firstInstance(&ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{nil, {Instances: []*ec2.Instance{nil}}},
	}))
}

// firstInstance skips nil reservations and nil instance slots and returns the
// first concrete instance it finds.
func TestFirstInstance_SkipsNilsAndReturnsFirst(t *testing.T) {
	out := &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{
			nil,
			{Instances: []*ec2.Instance{nil}},
			{Instances: []*ec2.Instance{{InstanceId: aws.String("i-first")}, {InstanceId: aws.String("i-second")}}},
		},
	}
	got := firstInstance(out)
	require.NotNil(t, got)
	assert.Equal(t, "i-first", aws.StringValue(got.InstanceId))
}
