package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestWorkerStillPresent(t *testing.T) {
	t.Parallel()
	inst := func(state string) *ec2.DescribeInstancesOutput {
		return &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{
			{State: &ec2.InstanceState{Name: aws.String(state)}},
		}}}}
	}
	assert.False(t, workerStillPresent(&ec2.DescribeInstancesOutput{}))
	assert.False(t, workerStillPresent(inst(ec2.InstanceStateNameTerminated)))
	assert.True(t, workerStillPresent(inst(ec2.InstanceStateNameShuttingDown)))
	assert.True(t, workerStillPresent(inst(ec2.InstanceStateNameRunning)))
	assert.True(t, workerStillPresent(&ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{{}}}}}))
}
