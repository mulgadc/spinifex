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

// phase7_CrossNodeOps is the Go port of phase 7 from
// run-multinode-e2e.sh:854-895. Picks the first trio instance, finds the
// node currently hosting it, then drives stop+start through gateways on
// OTHER nodes — proving the cross-node control path actually reaches the
// hosting daemon, not just the local one.
//
// State guarantee: instance ends running. Other Test*s may share the trio
// (phase 4 SSH, etc.) so leaving it stopped would cascade-fail downstream
// tests. Bash phase 7 has the same guarantee implicitly via the start
// poll at the end.
func phase7_CrossNodeOps(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode Phase 7 — Cross-Node Stop/Start")

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
