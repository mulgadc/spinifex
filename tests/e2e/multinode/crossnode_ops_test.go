//go:build e2e

package multinode

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runCrossNodeOps stops and starts the first trio instance through gateways on
// OTHER nodes than the one hosting it, proving cross-node control path routing.
// Instance is guaranteed to end running so sibling tests sharing the trio are unaffected.
func runCrossNodeOps(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Cross-Node Stop/Start")

	trio := needInstanceTrio(t, fix)
	require.NotEmpty(t, trio, "trio required")
	target := trio[0]

	hostNode := harness.InstanceHostingNode(t, fix.Cluster, target)
	require.NotNil(t, hostNode, "no node hosts %s", target)
	harness.Detail(t, "target", target, "hosting_node", hostNode.Name)

	stopGW, startGW := otherTwoGateways(fix.Cluster, hostNode)
	harness.Detail(t, "stop_via", stopGW.Name, "start_via", startGW.Name)

	harness.Step(t, "stop %s via %s", target, stopGW.Name)
	stopCli := harness.AWSClientForGateway(t, fix.Env, *stopGW)
	_, err := stopCli.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: []*string{aws.String(target)},
	})
	require.NoErrorf(t, err, "stop-instances via %s", stopGW.Name)
	harness.WaitForInstanceState(t, stopCli, target, "stopped",
		harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))

	harness.Step(t, "start %s via %s", target, startGW.Name)
	startCli := harness.AWSClientForGateway(t, fix.Env, *startGW)
	_, err = startCli.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(target)},
	})
	require.NoErrorf(t, err, "start-instances via %s", startGW.Name)
	harness.WaitForInstanceState(t, startCli, target, "running",
		harness.WithTimeout(60*time.Second), harness.WithPoll(2*time.Second))
}

// otherTwoGateways returns two distinct nodes neither of which is host.
// Falls back to (other, other) on a 2-node cluster (no third gateway
// available) — matches the bash fallback.
func otherTwoGateways(c *harness.Cluster, host *harness.Node) (stop, start *harness.Node) {
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Index == host.Index {
			continue
		}
		if stop == nil {
			stop = n
			continue
		}
		start = n
		break
	}
	if start == nil {
		start = stop
	}
	return stop, start
}
