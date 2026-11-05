//go:build e2e

package eks

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The private-endpoint NIC is the cross-account ENI the cluster's endpoint
	// NLB carries after its data and management NICs.
	endpointNIC = "net2"

	// Workers must be Ready again this long after the NIC is disabled.
	endpointHealBudget = 10 * time.Minute
)

// runEndpointNICSelfHeal disables the private-endpoint NIC of the cluster's
// endpoint NLB, the failure that left every worker NotReady, and asserts the
// platform repairs it: every worker is Ready again within ten minutes, the
// cluster stays ACTIVE, and the NLB guest is the same instance.
func runEndpointNICSelfHeal(t *testing.T, c *harness.AWSClient, env *harness.Env, artifacts string, fx *clusterFixture) {
	nodeRoleArn, _ := ensureNodeRole(t, c, fx)
	createWorkerNodegroup(t, c, fx, "nic-heal-ng", nodeRoleArn, nil)

	kc := harness.NewKubectl(t, writeKubeconfig(t, artifacts, fx.Cluster), getTokenEnv(t, env))
	requireWorkersReady(t, kc, 5*time.Minute)

	sys := harness.SystemAWSClient(t, env)
	sysInstance, node := findEndpointNLBGuest(t, sys, fx)
	harness.Detail(t, "endpoint_nlb_instance", sysInstance, "node", node.Name)

	procBefore, err := harness.ProcessIdentity(node, sysInstance)
	require.NoError(t, err, "the endpoint NLB's guest must be running on %s", node.Name)

	faultAt := time.Now()
	harness.Step(t, "disable %s of %s on %s", endpointNIC, sysInstance, node.Name)
	// A NIC that cannot be reached through QMP stops the step: the criterion
	// then needs the person's decision rather than a weaker test.
	require.NoError(t, harness.LBSetLink(t, node, sysInstance, endpointNIC, false),
		"the private-endpoint NIC could not be disabled through QMP; stop and ask whether to revise the criterion")

	harness.Step(t, "every worker is Ready again within %s", endpointHealBudget)
	sawNotReady := false
	harness.EventuallyErr(t, func() error {
		cl, derr := c.EKS.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(fx.ClusterName)})
		if derr != nil {
			return fmt.Errorf("describe-cluster: %w", derr)
		}
		if s := aws.StringValue(cl.Cluster.Status); s != eks.ClusterStatusActive {
			return fmt.Errorf("cluster is %s, want %s throughout", s, eks.ClusterStatusActive)
		}
		ready, rerr := workersReady(kc)
		if rerr != nil {
			sawNotReady = true
			return rerr
		}
		if !ready {
			sawNotReady = true
			return fmt.Errorf("a worker is not Ready")
		}
		if !sawNotReady && time.Since(faultAt) < 2*time.Minute {
			return fmt.Errorf("workers still Ready before the fault has had any effect")
		}
		return nil
	}, endpointHealBudget, 10*time.Second)

	procAfter, err := harness.ProcessIdentity(node, sysInstance)
	require.NoError(t, err, "the endpoint NLB's guest must be running after the repair")
	assert.NotEqual(t, procBefore, procAfter, "the endpoint NLB's guest process is unchanged, so nothing rebooted it")

	again, _ := findEndpointNLBGuest(t, sys, fx)
	assert.Equal(t, sysInstance, again, "the endpoint NLB changed guest instead of rebooting it")

	cl, err := c.EKS.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String(fx.ClusterName)})
	require.NoError(t, err)
	assert.Equal(t, eks.ClusterStatusActive, aws.StringValue(cl.Cluster.Status))

	started := harness.DaemonLogLines(clusterNodes(t), faultAt.Add(-time.Minute), "ELBv2 data-path repair started", sysInstance)
	total := 0
	for _, lines := range started {
		total += len(lines)
	}
	assert.Equalf(t, 1, total, "want exactly one repair of %s, got %d: %v", sysInstance, total, started)
}

// workersReady reports whether every registered node is Ready, and that at
// least one node exists.
func workersReady(kc *harness.Kubectl) (bool, error) {
	out, err := kc.Run(30*time.Second, "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
	if err != nil {
		return false, fmt.Errorf("kubectl get nodes: %v\n%s", err, out)
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return false, fmt.Errorf("no worker has registered")
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, "=True") {
			return false, nil
		}
	}
	return true, nil
}

func requireWorkersReady(t *testing.T, kc *harness.Kubectl, budget time.Duration) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		ready, err := workersReady(kc)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("a worker is not Ready")
		}
		return nil
	}, budget, 10*time.Second)
}

func clusterNodes(t *testing.T) *harness.Cluster {
	t.Helper()
	cluster, err := harness.ClusterFromEnv()
	require.NoError(t, err, "this check needs node SSH to disable a NIC and read the repair line")
	return cluster
}

// findEndpointNLBGuest returns the guest running the cluster's endpoint NLB and
// the node it runs on. The NLB belongs to the system account, so it is found
// there, by the cluster tag.
func findEndpointNLBGuest(t *testing.T, sys *harness.AWSClient, fx *clusterFixture) (string, harness.Node) {
	t.Helper()
	lbs, err := sys.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{})
	require.NoError(t, err, "describe load balancers as the system account")

	for _, lb := range lbs.LoadBalancers {
		if aws.StringValue(lb.Type) != elbv2.LoadBalancerTypeEnumNetwork {
			continue
		}
		tagOut, terr := sys.ELBv2.DescribeTags(&elbv2.DescribeTagsInput{ResourceArns: []*string{lb.LoadBalancerArn}})
		if terr != nil {
			continue
		}
		owned := false
		for _, td := range tagOut.TagDescriptions {
			for _, tag := range td.Tags {
				if strings.Contains(aws.StringValue(tag.Value), fx.ClusterName) {
					owned = true
				}
			}
		}
		if !owned {
			continue
		}
		parts := strings.Split(aws.StringValue(lb.LoadBalancerArn), "/")
		desc := fmt.Sprintf("ELB %s/%s/%s", parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1])
		enis, eerr := sys.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{Name: aws.String("description"), Values: []*string{aws.String(desc)}}},
		})
		require.NoError(t, eerr, "describe the endpoint NLB's interfaces")
		for _, eni := range enis.NetworkInterfaces {
			if eni.Attachment == nil || aws.StringValue(eni.Attachment.InstanceId) == "" {
				continue
			}
			id := aws.StringValue(eni.Attachment.InstanceId)
			host := harness.InstanceHostingNode(t, clusterNodes(t), id)
			require.NotNilf(t, host, "no node is running the endpoint NLB's guest %s", id)
			return id, *host
		}
	}
	t.Fatalf("no endpoint NLB found for cluster %s in the system account", fx.ClusterName)
	return "", harness.Node{}
}
