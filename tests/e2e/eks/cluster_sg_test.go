//go:build e2e

package eks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sgIngressName = "lbc-e2e-pub"
	sgNodePort    = 30082
)

// clusterSecurityGroupID returns the id DescribeCluster reports for the cluster.
func clusterSecurityGroupID(t *testing.T, fx *clusterFixture) string {
	t.Helper()
	require.NotNil(t, fx.Cluster.ResourcesVpcConfig, "ACTIVE cluster must expose resourcesVpcConfig")
	id := aws.StringValue(fx.Cluster.ResourcesVpcConfig.ClusterSecurityGroupId)
	require.NotEmpty(t, id, "DescribeCluster must report clusterSecurityGroupId")
	return id
}

// runInstanceTargetIngress reconciles an internet-facing ALB Ingress with
// target-type instance and proves it reaches the workers with no hand-written
// security group rule: the controller adds its own rule to the cluster security
// group, and the target turns healthy through it. It also proves exec and logs
// work, which run over the kubelet port from the control plane.
func runInstanceTargetIngress(t *testing.T, c *harness.AWSClient, kc *harness.Kubectl, artifacts string, fx *clusterFixture) {
	sgID := clusterSecurityGroupID(t, fx)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: lbc-e2e-np
  namespace: default
spec:
  type: NodePort
  selector: {app: lbc-e2e}
  ports: [{port: 80, targetPort: 80, nodePort: %d}]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s
  namespace: default
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: instance
    alb.ingress.kubernetes.io/subnets: %s
spec:
  ingressClassName: alb
  defaultBackend:
    service:
      name: lbc-e2e-np
      port: {number: 80}
`, sgNodePort, sgIngressName, fx.Egress.PubSubnetID)
	path := filepath.Join(artifacts, "lbc-ingress-instance.yaml")
	require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	t.Cleanup(func() {
		_, _ = kc.Run(2*time.Minute, "delete", "-f", path, "--ignore-not-found", "--wait=true")
	})
	out, err := kc.Run(60*time.Second, "apply", "-f", path)
	require.NoErrorf(t, err, "apply instance-target ingress:\n%s", out)

	var dns string
	harness.EventuallyErr(t, func() error {
		got, gerr := kc.Run(30*time.Second, "-n", "default", "get", "ingress", sgIngressName, "-o",
			"jsonpath={.status.loadBalancer.ingress[0].hostname}")
		if gerr != nil {
			return fmt.Errorf("get ingress: %v\n%s", gerr, got)
		}
		if strings.TrimSpace(got) == "" {
			return fmt.Errorf("ingress has no load balancer address yet")
		}
		dns = strings.TrimSpace(got)
		return nil
	}, 8*time.Minute, 10*time.Second)

	var lbArn string
	harness.EventuallyErr(t, func() error {
		lbs, lerr := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{})
		if lerr != nil {
			return fmt.Errorf("describe-load-balancers: %w", lerr)
		}
		for _, lb := range lbs.LoadBalancers {
			if aws.StringValue(lb.DNSName) != dns {
				continue
			}
			if st := aws.StringValue(lb.State.Code); st != elbv2.LoadBalancerStateEnumActive {
				return fmt.Errorf("ALB %s state %q, want active", dns, st)
			}
			assert.Equal(t, elbv2.LoadBalancerSchemeEnumInternetFacing, aws.StringValue(lb.Scheme))
			lbArn = aws.StringValue(lb.LoadBalancerArn)
			return nil
		}
		return fmt.Errorf("no ALB with DNS name %s", dns)
	}, 5*time.Minute, 10*time.Second)

	// The controller, not the test, opened the NodePort on the cluster security
	// group, from the load balancer's group.
	harness.EventuallyErr(t, func() error {
		sg, derr := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{GroupIds: aws.StringSlice([]string{sgID})})
		if derr != nil {
			return fmt.Errorf("describe cluster security group: %w", derr)
		}
		require.Len(t, sg.SecurityGroups, 1)
		for _, perm := range sg.SecurityGroups[0].IpPermissions {
			if aws.Int64Value(perm.FromPort) <= sgNodePort && sgNodePort <= aws.Int64Value(perm.ToPort) && len(perm.UserIdGroupPairs) > 0 {
				return nil
			}
		}
		return fmt.Errorf("cluster security group %s has no group-sourced rule for NodePort %d", sgID, sgNodePort)
	}, 3*time.Minute, 10*time.Second)

	harness.EventuallyErr(t, func() error {
		tgs, terr := c.ELBv2.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{LoadBalancerArn: aws.String(lbArn)})
		if terr != nil {
			return fmt.Errorf("describe-target-groups: %w", terr)
		}
		if len(tgs.TargetGroups) == 0 {
			return fmt.Errorf("ALB has no target group yet")
		}
		for _, tg := range tgs.TargetGroups {
			assert.Equal(t, elbv2.TargetTypeEnumInstance, aws.StringValue(tg.TargetType))
			health, herr := c.ELBv2.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{TargetGroupArn: tg.TargetGroupArn})
			if herr != nil {
				return fmt.Errorf("describe-target-health: %w", herr)
			}
			if len(health.TargetHealthDescriptions) == 0 {
				return fmt.Errorf("target group %s has no targets", aws.StringValue(tg.TargetGroupArn))
			}
			for _, d := range health.TargetHealthDescriptions {
				if st := aws.StringValue(d.TargetHealth.State); st != elbv2.TargetHealthStateEnumHealthy {
					return fmt.Errorf("target %s is %q, want healthy", aws.StringValue(d.Target.Id), st)
				}
			}
		}
		return nil
	}, 6*time.Minute, 10*time.Second)

	// exec and logs reach the kubelet on 10250 from the control plane.
	pod, err := kc.Run(30*time.Second, "-n", "default", "get", "pods", "-l", "app=lbc-e2e",
		"-o", "jsonpath={.items[0].metadata.name}")
	require.NoErrorf(t, err, "find lbc-e2e pod:\n%s", pod)
	pod = strings.TrimSpace(pod)
	out, err = kc.Run(60*time.Second, "-n", "default", "exec", pod, "--", "true")
	assert.NoErrorf(t, err, "kubectl exec must work with only the cluster security group:\n%s", out)
	out, err = kc.Run(60*time.Second, "-n", "default", "logs", pod, "--tail=5")
	assert.NoErrorf(t, err, "kubectl logs must work with only the cluster security group:\n%s", out)
}

// createSecondSecurityGroup adds a security group the cluster does not own to
// the cluster's VPC: one rule open to a CIDR, one that references the cluster
// security group. DeleteCluster must remove only the second.
func createSecondSecurityGroup(t *testing.T, c *harness.AWSClient, fx *clusterFixture, clusterSG string) string {
	t.Helper()
	out, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(fx.ClusterName + "-bystander"),
		Description: aws.String("unrelated customer security group"),
		VpcId:       aws.String(fx.VPCID),
	})
	require.NoError(t, err, "create bystander security group")
	id := aws.StringValue(out.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)})
	})
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(id),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int64(8443), ToPort: aws.Int64(8443),
				IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.210.0.0/16")}},
			},
			{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int64(9443), ToPort: aws.Int64(9443),
				UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(clusterSG)}},
			},
		},
	})
	require.NoError(t, err, "authorize bystander ingress")
	return id
}

// assertBystanderRulesAfterDelete checks the cluster security group is gone,
// the bystander kept its CIDR rule, and lost only the rule naming the cluster group.
func assertBystanderRulesAfterDelete(t *testing.T, c *harness.AWSClient, bystander, clusterSG string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		_, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{GroupIds: aws.StringSlice([]string{clusterSG})})
		if err == nil {
			return fmt.Errorf("cluster security group %s still exists", clusterSG)
		}
		return nil
	}, 5*time.Minute, 5*time.Second)

	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{GroupIds: aws.StringSlice([]string{bystander})})
	require.NoError(t, err, "bystander security group must survive DeleteCluster")
	require.Len(t, out.SecurityGroups, 1)
	var cidrRule, clusterRef bool
	for _, perm := range out.SecurityGroups[0].IpPermissions {
		for _, r := range perm.IpRanges {
			if aws.StringValue(r.CidrIp) == "10.210.0.0/16" && aws.Int64Value(perm.FromPort) == 8443 {
				cidrRule = true
			}
		}
		for _, p := range perm.UserIdGroupPairs {
			if aws.StringValue(p.GroupId) == clusterSG {
				clusterRef = true
			}
		}
	}
	assert.True(t, cidrRule, "DeleteCluster must not strip the bystander's unrelated CIDR rule")
	assert.False(t, clusterRef, "DeleteCluster must remove the bystander rule that references the cluster security group")
}
