//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/require"
)

// TestRunInstances_CapacityDistributionAcrossNodes proves the default
// (non-placement-group) path spreads a multi-instance launch across
// ExpectedNodes proportionally to each node's spare capacity
// (placement.go's spreadAllocate: 1-per-node first round, then remaining
// instances packed onto whichever node has the most spare capacity). Node A
// reports 3 available, node B reports 1: requesting MaxCount=3 must land 2 on
// A and 1 on B, proving genuine per-node dispatch counts rather than a single
// node absorbing the whole request.
func TestRunInstances_CapacityDistributionAcrossNodes(t *testing.T) {
	gw := StartGateway(t, withExpectedNodes(2))

	const (
		instanceType = "t3.micro"
		nodeA        = "capacity-node-a"
		nodeB        = "capacity-node-b"
	)

	stubNodeCapacity(t, gw, nodeA, instanceType, 3)
	stubNodeCapacity(t, gw, nodeB, instanceType, 1)
	chA := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeA)
	chB := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeB)

	out, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String(instanceType),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(3),
	})
	require.NoError(t, err, "run-instances across two nodes of unequal capacity")
	require.Len(t, out.Instances, 2, "aggregateResults merges one reservation frame per node dispatched, regardless of each node's Assigned count")

	gotA := awaitLaunchTemplateNodeInput(t, chA)
	gotB := awaitLaunchTemplateNodeInput(t, chB)
	require.Equal(t, int64(2), aws.Int64Value(gotA.MinCount), "the higher-capacity node must absorb the extra instance beyond the 1-per-node round")
	require.Equal(t, int64(2), aws.Int64Value(gotA.MaxCount))
	require.Equal(t, int64(1), aws.Int64Value(gotB.MinCount), "the lower-capacity node must only get its 1-per-node share")
	require.Equal(t, int64(1), aws.Int64Value(gotB.MaxCount))
}
