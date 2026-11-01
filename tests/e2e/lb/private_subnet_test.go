//go:build e2e

package lb

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	privVPCCIDR    = "10.201.0.0/16"
	privSubnetCIDR = "10.201.1.0/24"
	privProbes     = 10

	// systemAccountID owns every platform-managed LB VM.
	systemAccountID = "000000000000"
)

// TestInternalALBPrivateSubnet launches an internal ALB in a private subnet of a
// non-default VPC that has no internet gateway and no public route. The LB must reach
// active, its ENI must record the platform account as the attached instance's owner, and
// an instance inside the VPC must be served by it. The runner cannot reach the VPC, so the
// in-VPC client reports its results on its serial console.
func TestInternalALBPrivateSubnet(t *testing.T) {
	env := harness.LoadEnv(t)
	skipIfDevNetworking(t, env)

	c := harness.NewAWSClient(t, env)
	artifacts := harness.ArtifactDir(t, env)
	f := &sharedFixture{}

	harness.Phase(t, "Discovering cluster capacity")
	f.InstanceType = discoverNanoInstanceType(t, c)
	f.AMIID = discoverAMI(t, c)
	ensureKeyPair(t, c)
	t.Cleanup(func() { deleteKeyPair(t, c) })

	harness.Phase(t, "Creating private VPC (%s), no internet gateway", privVPCCIDR)
	vpc, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(privVPCCIDR)})
	require.NoError(t, err)
	f.VPCID = aws.StringValue(vpc.Vpc.VpcId)
	t.Cleanup(func() { deleteVPC(t, c, f) })

	sn, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(f.VPCID),
		CidrBlock: aws.String(privSubnetCIDR),
	})
	require.NoError(t, err)
	f.SubnetID = aws.StringValue(sn.Subnet.SubnetId)
	t.Cleanup(func() { deleteSubnet(t, c, f) })

	sgOut, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(f.VPCID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, sgOut.SecurityGroups, "VPC default SG missing")
	f.SecurityGroup = aws.StringValue(sgOut.SecurityGroups[0].GroupId)
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(f.SecurityGroup),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(httpPort),
			ToPort:     aws.Int64(httpPort),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(privVPCCIDR)}},
		}},
	})
	require.NoError(t, err)

	harness.OnFailure(t, func() { dumpDaemonLogs(t, artifacts, "private-subnet") })

	harness.Phase(t, "Launching backend in the private subnet")
	backend := runPrivateInstance(t, c, f, appUserData)
	t.Cleanup(func() { terminateInstances(t, c, []string{backend}) })

	tgArn := createTargetGroup(t, c, f, "lb-e2e-priv-tg", "HTTP", httpPort, "/index.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgArn) })
	registerTargets(t, c, tgArn, []string{backend})
	t.Cleanup(func() { deregisterTargets(t, c, tgArn, []string{backend}) })

	label := "ALB internal private subnet"
	lb, _ := createActiveLB(t, c, f, "lb-e2e-priv", "application", "internal", "HTTP", httpPort, tgArn, label, []string{f.SecurityGroup})
	harness.WaitForTargetsHealthy(t, c, tgArn, 1, label, 2*time.Minute)

	eni := lbENI(t, c, "app", lb)
	require.NotNil(t, eni.Attachment, "%s: LB ENI must be attached", label)
	assert.Equal(t, systemAccountID, aws.StringValue(eni.Attachment.InstanceOwnerId),
		"%s: the LB ENI must record the platform account as the owner of its VM", label)
	assert.Empty(t, publicIP(eni), "%s must not have a public IP", label)
	lbIP := privateIP(eni)
	require.NotEmpty(t, lbIP, "%s needs a private IP", label)

	harness.Phase(t, "Probing %s from a client inside the VPC", lbIP)
	client := runPrivateInstance(t, c, f, privateProbeUserData(lbIP))
	t.Cleanup(func() { terminateInstances(t, c, []string{client}) })

	var served int
	harness.OnFailure(t, func() {
		harness.DumpInstanceConsole(t, c, client, artifacts, "private-client-console.log")
	})
	harness.Eventually(t, func() bool {
		out, err := harness.InstanceConsole(c, client)
		if err != nil {
			return false
		}
		served = strings.Count(out, "LBPROBE ok")
		return strings.Contains(out, "LBPROBE done")
	}, 5*time.Minute, 5*time.Second, "client never reported its probe results on the console")
	assert.Equal(t, privProbes, served, "%s: every in-VPC request must be served", label)
}

func runPrivateInstance(t *testing.T, c *harness.AWSClient, f *sharedFixture, userData string) string {
	t.Helper()
	out, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(f.AMIID),
		InstanceType: aws.String(f.InstanceType),
		KeyName:      aws.String(lbKeyName),
		SubnetId:     aws.String(f.SubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     aws.String(base64Encode(userData)),
	})
	require.NoError(t, err, "run-instances in private subnet")
	require.NotEmpty(t, out.Instances)
	id := aws.StringValue(out.Instances[0].InstanceId)
	harness.WaitForInstanceRunning(t, c, id, 120*time.Second)
	t.Logf("private instance: %s", id)
	return id
}

// privateProbeUserData returns a boot script that requests the LB privProbes times and
// writes one console line per served request, then a terminating line.
func privateProbeUserData(lbIP string) string {
	return fmt.Sprintf(`#!/bin/bash
for i in $(seq 1 %d); do
  for try in $(seq 1 30); do
    if curl -sf --max-time 5 http://%s/index.html | grep -q instance_id; then
      echo "LBPROBE ok $i" > /dev/console
      break
    fi
    sleep 5
  done
done
echo "LBPROBE done" > /dev/console
`, privProbes, lbIP)
}
