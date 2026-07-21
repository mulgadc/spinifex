//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// stubNodeCapacity registers one of several simultaneous spinifex.node.status
// responders (NATS core fan-out: every non-queue subscriber on the subject
// independently receives and replies to the same request), so a test can
// simulate a multi-node cluster where each node reports distinct capacity.
// StubSubject cannot do this: a second call on the same subject would add a
// duplicate responder replying with the same single payload, not a second
// node's distinct status.
func stubNodeCapacity(t *testing.T, gw *Gateway, node, instanceType string, available int) {
	t.Helper()
	sub, err := gw.NATSConn.Subscribe("spinifex.node.status", func(msg *nats.Msg) {
		if msg.Reply == "" {
			return
		}
		_ = msg.Respond(mustMarshal(t, &types.NodeStatusResponse{
			Node:          node,
			InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: available}},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// withExpectedNodes overrides StartGateway's default ExpectedNodes: 1 pin,
// which utils.Gather uses to stop fanning out once that many nodes reply
// rather than always waiting the full 3s timeout.
func withExpectedNodes(n int) Option {
	return func(cfg *gateway.GatewayConfig) { cfg.ExpectedNodes = n }
}

// TestRunInstances_PlacementGroupSpreadRouting proves a spread-strategy group
// (RunInstances.go's placement-group switch -> distributeInstancesSpread in
// placement.go) genuinely reserves and dispatches to distinct nodes via the
// real PlacementGroupServiceImpl, rather than falling through to the default
// distributeInstances path: two equal-capacity nodes, MinCount=MaxCount=2,
// must each receive exactly one instance.
func TestRunInstances_PlacementGroupSpreadRouting(t *testing.T) {
	gw := StartGateway(t, withExpectedNodes(2))
	StartPlacementGroupDaemonLite(t, gw)

	const (
		instanceType = "t3.micro"
		nodeA        = "spread-node-a"
		nodeB        = "spread-node-b"
		groupName    = "pg-spread-routing"
	)

	stubNodeCapacity(t, gw, nodeA, instanceType, 5)
	stubNodeCapacity(t, gw, nodeB, instanceType, 5)
	chA := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeA)
	chB := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeB)

	_, err := gw.EC2Client(t).CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(groupName),
		Strategy:  aws.String(ec2.PlacementStrategySpread),
	})
	require.NoError(t, err, "create-placement-group (spread)")

	out, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String(instanceType),
		MinCount:     aws.Int64(2),
		MaxCount:     aws.Int64(2),
		Placement:    &ec2.Placement{GroupName: aws.String(groupName)},
	})
	require.NoError(t, err, "run-instances into spread group")
	require.Len(t, out.Instances, 2)

	gotA := awaitLaunchTemplateNodeInput(t, chA)
	gotB := awaitLaunchTemplateNodeInput(t, chB)
	require.Equal(t, int64(1), aws.Int64Value(gotA.MinCount), "spread must assign exactly 1 instance to node A")
	require.Equal(t, int64(1), aws.Int64Value(gotA.MaxCount))
	require.Equal(t, int64(1), aws.Int64Value(gotB.MinCount), "spread must assign exactly 1 instance to node B")
	require.Equal(t, int64(1), aws.Int64Value(gotB.MaxCount))
}

// TestRunInstances_PlacementGroupClusterRouting proves a cluster-strategy
// group (distributeInstancesCluster) pins the entire launch to a single node
// rather than spreading it: given two nodes of unequal capacity, all
// requested instances must land on the higher-capacity node (ReserveClusterNode
// picks EligibleNodes[0], which queryNodeCapacity sorts by capacity desc), and
// the low-capacity node must receive zero dispatches.
func TestRunInstances_PlacementGroupClusterRouting(t *testing.T) {
	gw := StartGateway(t, withExpectedNodes(2))
	StartPlacementGroupDaemonLite(t, gw)

	const (
		instanceType = "t3.micro"
		nodeHigh     = "cluster-node-high"
		nodeLow      = "cluster-node-low"
		groupName    = "pg-cluster-routing"
	)

	stubNodeCapacity(t, gw, nodeHigh, instanceType, 5)
	stubNodeCapacity(t, gw, nodeLow, instanceType, 1)
	chHigh := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeHigh)

	// No responder is stubbed on nodeLow's own launch subject via
	// captureLaunchTemplateNodeInput; instead a raw counting subscriber proves
	// it never fires, rather than merely absence-of-error.
	var lowDispatchCount int
	lowSub, err := gw.NATSConn.Subscribe("ec2.RunInstances."+instanceType+"."+nodeLow, func(msg *nats.Msg) {
		lowDispatchCount++
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lowSub.Unsubscribe() })

	_, err = gw.EC2Client(t).CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(groupName),
		Strategy:  aws.String(ec2.PlacementStrategyCluster),
	})
	require.NoError(t, err, "create-placement-group (cluster)")

	out, err := gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String(instanceType),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(3),
		Placement:    &ec2.Placement{GroupName: aws.String(groupName)},
	})
	require.NoError(t, err, "run-instances into cluster group")
	// captureLaunchTemplateNodeInput's mock always replies with 1 instance
	// regardless of the dispatched Assigned count; the count actually assigned
	// is asserted below via the captured RunInstancesInput's MinCount/MaxCount.
	require.Len(t, out.Instances, 1)

	gotHigh := awaitLaunchTemplateNodeInput(t, chHigh)
	require.Equal(t, int64(3), aws.Int64Value(gotHigh.MinCount), "cluster must pin all 3 instances to the high-capacity node")
	require.Equal(t, int64(3), aws.Int64Value(gotHigh.MaxCount))
	require.Equal(t, 0, lowDispatchCount, "cluster must never dispatch to the low-capacity node")
}
