package handlers_imds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstReservationInstance_NilAndEmpty(t *testing.T) {
	for _, out := range []*ec2.DescribeInstancesOutput{
		nil,
		{},
		{Reservations: []*ec2.Reservation{nil, {Instances: []*ec2.Instance{nil}}}},
	} {
		res, inst := firstReservationInstance(out)
		assert.Nil(t, res)
		assert.Nil(t, inst)
	}
}

// firstReservationInstance skips nil reservations and nil instance slots and
// returns the first concrete instance with its owning reservation.
func TestFirstReservationInstance_SkipsNilsAndReturnsFirst(t *testing.T) {
	out := &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{
			nil,
			{Instances: []*ec2.Instance{nil}},
			{
				ReservationId: aws.String("r-123"),
				Instances:     []*ec2.Instance{{InstanceId: aws.String("i-first")}, {InstanceId: aws.String("i-second")}},
			},
		},
	}
	res, inst := firstReservationInstance(out)
	require.NotNil(t, inst)
	assert.Equal(t, "i-first", aws.StringValue(inst.InstanceId))
	require.NotNil(t, res)
	assert.Equal(t, "r-123", aws.StringValue(res.ReservationId))
}
