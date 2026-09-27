//go:build e2e

package lbrecovery

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	// Its own CIDR, not the lb suite's 10.200.0.0/16: a run that overlapped one
	// left behind by a failed lb run would fail on the create rather than on
	// anything this suite is about.
	lbrVPCCIDR    = "10.210.0.0/16"
	lbrSubnetCIDR = "10.210.1.0/24"

	lbrLBName   = "lbr-e2e-alb"
	lbrTGName   = "lbr-e2e-tg"
	lbrHTTPPort = int64(80)

	// Two targets, on nodes other than the ALB's. One would not distinguish
	// "the ALB is serving" from "the ALB is serving the one backend it has".
	lbrTargets = 2

	// Probes per round. Enough that a round-robin across two backends is
	// unambiguous, small enough that a round costs seconds rather than a minute
	// while a recovery is in flight.
	lbrProbes = 12

	// How many times an LB that lands in a terminal state is recreated before
	// the suite gives up, matching the lb suite: on a loaded cluster this is
	// usually the previous run's sys.micro slot still being reclaimed.
	lbrLBAttempts = 3
)

//go:embed testdata/target-userdata.sh
var targetUserData string

// albFixture is the topology the scenario needs: one internet-facing ALB with a
// node to itself, and its backends on other nodes, so taking the ALB's node away
// leaves the backends untouched and the only thing that has to move is the ALB.
type albFixture struct {
	VPCID, SubnetID, IGWID, RouteTableID string
	SGID, LBSGID                         string
	AMIID, InstanceType, KeyName         string

	TGArn, LBArn, LBID, ListenerArn string
	DNSName, PublicIP               string
	ENIID, SysInstanceID            string
	Host                            harness.Node

	TargetIDs []string
}

// setupALBFixture builds the VPC, the ALB, and then the targets — in that order
// because the ALB's node is not chosen by the test, it is discovered, and the
// targets are the half of the topology that can be placed around it.
func setupALBFixture(t *testing.T, fix *Fixture) *albFixture {
	t.Helper()
	f := &albFixture{}

	harness.Phase(t, "Building the ALB fixture")
	instanceType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	f.InstanceType = instanceType
	f.AMIID = harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	f.KeyName, _ = harness.EnsureKeyPair(t, fix.Harness)

	lbrCreateVPC(t, fix.AWS, f)
	lbrCreateIGW(t, fix.AWS, f)
	lbrCreateSubnet(t, fix.AWS, f)
	lbrAddPublicRoute(t, fix.AWS, f)
	lbrOpenDefaultSG(t, fix.AWS, f)

	f.TGArn = lbrCreateTargetGroup(t, fix.AWS, f)
	lbrCreateActiveLB(t, fix.AWS, f)
	lbrDiscoverLBPlacement(t, fix, f)

	lbrLaunchTargetsAvoiding(t, fix, f)
	lbrRegisterTargets(t, fix.AWS, f)
	harness.WaitForTargetsHealthy(t, fix.AWS, f.TGArn, lbrTargets, "ALB targets", targetsHealthyBudget)

	return f
}

func lbrCreateVPC(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	// The ALB needs a public subnet of its own, and a shared fixture VPC cannot
	// be given one without changing what every other suite sees.
	// e2e:allow-create
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(lbrVPCCIDR)})
	require.NoError(t, err, "create VPC %s", lbrVPCCIDR)
	f.VPCID = aws.StringValue(out.Vpc.VpcId)
	harness.Detail(t, "vpc", f.VPCID)
	t.Cleanup(func() {
		if _, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(f.VPCID)}); err != nil {
			t.Logf("delete VPC %s: %v", f.VPCID, err)
		}
	})
}

func lbrCreateIGW(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	// Belongs to this suite's own VPC.
	// e2e:allow-create
	out, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create IGW")
	f.IGWID = aws.StringValue(out.InternetGateway.InternetGatewayId)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(f.IGWID),
		VpcId:             aws.String(f.VPCID),
	})
	require.NoError(t, err, "attach IGW %s to %s", f.IGWID, f.VPCID)
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(f.IGWID),
			VpcId:             aws.String(f.VPCID),
		})
		if _, err := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(f.IGWID),
		}); err != nil {
			t.Logf("delete IGW %s: %v", f.IGWID, err)
		}
	})
}

func lbrCreateSubnet(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	// See lbrCreateVPC.
	// e2e:allow-create
	out, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(f.VPCID),
		CidrBlock: aws.String(lbrSubnetCIDR),
	})
	require.NoError(t, err, "create subnet %s", lbrSubnetCIDR)
	f.SubnetID = aws.StringValue(out.Subnet.SubnetId)
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(f.SubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "set MapPublicIpOnLaunch on %s", f.SubnetID)
	harness.Detail(t, "subnet", f.SubnetID)
	t.Cleanup(func() {
		if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(f.SubnetID)}); err != nil {
			t.Logf("delete subnet %s: %v", f.SubnetID, err)
		}
	})
}

// lbrAddPublicRoute installs 0.0.0.0/0 to the IGW on the main route table, which
// is what makes the daemon classify the subnet as public. Without it the logical
// router gets an egress DROP and the ALB is unreachable for a reason that has
// nothing to do with recovery.
func lbrAddPublicRoute(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(f.VPCID)}},
			{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
		},
	})
	require.NoError(t, err, "describe main route table")
	require.NotEmpty(t, out.RouteTables, "VPC %s has no main route table", f.VPCID)
	f.RouteTableID = aws.StringValue(out.RouteTables[0].RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(f.RouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(f.IGWID),
	})
	require.NoError(t, err, "create default route to %s", f.IGWID)
	t.Cleanup(func() {
		if _, err := c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
			RouteTableId:         aws.String(f.RouteTableID),
			DestinationCidrBlock: aws.String("0.0.0.0/0"),
		}); err != nil {
			t.Logf("delete default route: %v", err)
		}
	})
}

// lbrOpenDefaultSG opens HTTP on the VPC's default security group, which is what
// the targets get, and mints a separate one for the ALB itself.
//
// The structured IpPermissions form is required: vpcd ignores the top-level
// shorthand fields, so the OVN ACL would never be installed.
func lbrOpenDefaultSG(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(f.VPCID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoError(t, err, "describe default SG")
	require.NotEmpty(t, out.SecurityGroups, "VPC %s has no default SG", f.VPCID)
	f.SGID = aws.StringValue(out.SecurityGroups[0].GroupId)
	lbrAuthorizeHTTP(t, c, f.SGID)

	// The ALB's own group, so a rule change on the targets cannot silently
	// change what reaches the load balancer.
	// e2e:allow-create
	sg, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(f.VPCID),
		GroupName:   aws.String("lbrecovery-e2e-lb-sg"),
		Description: aws.String("lbrecovery e2e ALB-facing security group"),
	})
	require.NoError(t, err, "create LB SG")
	f.LBSGID = aws.StringValue(sg.GroupId)
	lbrAuthorizeHTTP(t, c, f.LBSGID)
	t.Cleanup(func() {
		if _, err := c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
			GroupId: aws.String(f.LBSGID),
		}); err != nil {
			t.Logf("delete LB SG %s: %v", f.LBSGID, err)
		}
	})
}

func lbrAuthorizeHTTP(t *testing.T, c *harness.AWSClient, sgID string) {
	t.Helper()
	_, err := c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(lbrHTTPPort),
			ToPort:     aws.Int64(lbrHTTPPort),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	})
	if err == nil {
		return
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) && aerr.Code() == "InvalidPermission.Duplicate" {
		return
	}
	t.Fatalf("authorize tcp/%d on %s: %v", lbrHTTPPort, sgID, err)
}

func lbrCreateTargetGroup(t *testing.T, c *harness.AWSClient, f *albFixture) string {
	t.Helper()
	// e2e:allow-create
	out, err := c.ELBv2.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String(lbrTGName),
		Protocol:                   aws.String("HTTP"),
		Port:                       aws.Int64(lbrHTTPPort),
		VpcId:                      aws.String(f.VPCID),
		HealthCheckPath:            aws.String("/index.html"),
		HealthCheckIntervalSeconds: aws.Int64(5),
		HealthyThresholdCount:      aws.Int64(2),
		UnhealthyThresholdCount:    aws.Int64(2),
	})
	require.NoError(t, err, "create target group")
	arn := aws.StringValue(out.TargetGroups[0].TargetGroupArn)
	harness.Detail(t, "target_group", arn)
	t.Cleanup(func() {
		if _, err := c.ELBv2.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
			TargetGroupArn: aws.String(arn),
		}); err != nil {
			t.Logf("delete target group: %v", err)
		}
	})
	return arn
}

// lbrCreateActiveLB creates an internet-facing ALB with an HTTP listener and
// waits for it to go active, recreating it when it lands in a terminal state.
func lbrCreateActiveLB(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	var lastErr error
	for attempt := 1; attempt <= lbrLBAttempts; attempt++ {
		if attempt > 1 {
			wait := time.Duration(attempt-1) * 15 * time.Second
			harness.Step(t, "waiting %s before recreating the ALB", wait)
			time.Sleep(wait)
		}
		lbrCreateLB(t, c, f)
		lbrCreateListener(t, c, f)
		lastErr = harness.WaitForLBActiveErr(t, c, f.LBArn, "recovery ALB", albActiveBudget)
		if lastErr == nil {
			harness.Detail(t, "load_balancer", f.LBArn, "dns_name", f.DNSName)
			return
		}
		if !errors.Is(lastErr, harness.ErrLBTerminalFailed) && !errors.Is(lastErr, harness.ErrLBProvisioningTimeout) {
			t.Fatalf("ALB never became active: %v", lastErr)
		}
		t.Logf("ALB attempt %d/%d: %v — tearing down and retrying", attempt, lbrLBAttempts, lastErr)
		lbrDeleteListener(t, c, f)
		lbrDeleteLB(t, c, f)
	}
	t.Fatalf("ALB never became active in %d attempts: %v", lbrLBAttempts, lastErr)
}

func lbrCreateLB(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	// The load balancer's own placement and recovery are the subject, so it can
	// never be one another test is sharing.
	// e2e:allow-create
	out, err := c.ELBv2.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String(lbrLBName),
		Subnets:        []*string{aws.String(f.SubnetID)},
		Scheme:         aws.String("internet-facing"),
		SecurityGroups: []*string{aws.String(f.LBSGID)},
	})
	require.NoError(t, err, "create load balancer")
	require.NotEmpty(t, out.LoadBalancers, "CreateLoadBalancer returned nothing")
	lb := out.LoadBalancers[0]
	f.LBArn = aws.StringValue(lb.LoadBalancerArn)
	f.DNSName = aws.StringValue(lb.DNSName)
	parts := strings.Split(f.LBArn, "/")
	f.LBID = parts[len(parts)-1]
	t.Cleanup(func() { lbrDeleteLB(t, c, f) })
}

func lbrDeleteLB(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	if f.LBArn == "" {
		return
	}
	if _, err := c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(f.LBArn),
	}); err != nil {
		t.Logf("delete LB %s: %v", f.LBArn, err)
	}
}

func lbrCreateListener(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	// e2e:allow-create
	out, err := c.ELBv2.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(f.LBArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(lbrHTTPPort),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(f.TGArn),
		}},
	})
	require.NoError(t, err, "create listener")
	f.ListenerArn = aws.StringValue(out.Listeners[0].ListenerArn)
	t.Cleanup(func() { lbrDeleteListener(t, c, f) })
}

func lbrDeleteListener(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	if f.ListenerArn == "" {
		return
	}
	if _, err := c.ELBv2.DeleteListener(&elbv2.DeleteListenerInput{
		ListenerArn: aws.String(f.ListenerArn),
	}); err != nil {
		t.Logf("delete listener: %v", err)
	}
}

// lbrDiscoverLBPlacement records where the ALB actually is, which is not the
// test's choice: a load balancer is launched over a queue group and lands on
// whichever node answers. Everything after this is arranged around the answer.
//
// The ENI is how the guest is found at all — a system instance is filtered out
// of DescribeInstances, so its id comes from the attachment on the ENI the LB
// describes itself by.
func lbrDiscoverLBPlacement(t *testing.T, fix *Fixture, f *albFixture) {
	t.Helper()
	desc := fmt.Sprintf("ELB app/%s/%s", lbrLBName, f.LBID)

	var eni *ec2.NetworkInterface
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("description"),
				Values: []*string{aws.String(desc)},
			}},
		})
		if err != nil {
			return err
		}
		if len(out.NetworkInterfaces) == 0 {
			return fmt.Errorf("no ENI described as %q", desc)
		}
		eni = out.NetworkInterfaces[0]
		if eni.Attachment == nil || aws.StringValue(eni.Attachment.InstanceId) == "" {
			return fmt.Errorf("ENI %s is not attached to an instance yet", aws.StringValue(eni.NetworkInterfaceId))
		}
		if eni.Association == nil || aws.StringValue(eni.Association.PublicIp) == "" {
			return fmt.Errorf("ENI %s has no public address yet", aws.StringValue(eni.NetworkInterfaceId))
		}
		return nil
	}, 60*time.Second, 3*time.Second)

	f.ENIID = aws.StringValue(eni.NetworkInterfaceId)
	f.SysInstanceID = aws.StringValue(eni.Attachment.InstanceId)
	f.PublicIP = aws.StringValue(eni.Association.PublicIp)

	host := harness.InstanceHostingNode(t, fix.Cluster, f.SysInstanceID)
	require.NotNilf(t, host, "the ALB is active but no node is running %s, so there is nothing to take away",
		f.SysInstanceID)
	f.Host = *host

	harness.Detail(t, "lb_instance", f.SysInstanceID, "lb_eni", f.ENIID,
		"lb_public_ip", f.PublicIP, "lb_node", f.Host.Name)
}

// lbrLaunchTargetsAvoiding launches the backends on nodes other than the ALB's,
// relaunching one that lands beside it.
//
// The whole point of the topology: a backend on the node that is about to be
// taken away would be recovered too, so the run could not tell an ALB that came
// back serving from one that came back with nothing to serve.
func lbrLaunchTargetsAvoiding(t *testing.T, fix *Fixture, f *albFixture) {
	t.Helper()
	harness.Step(t, "launch %d backends on nodes other than %s", lbrTargets, f.Host.Name)

	for len(f.TargetIDs) < lbrTargets {
		placed := false
		for attempt := 1; attempt <= 4 && !placed; attempt++ {
			id := lbrLaunchTarget(t, fix, f)
			harness.WaitForInstanceRunning(t, fix.AWS, id, 3*time.Minute)
			host := harness.InstanceHostingNode(t, fix.Cluster, id)
			if host != nil && host.Name != f.Host.Name {
				harness.Detail(t, "target", id, "target_node", host.Name)
				f.TargetIDs = append(f.TargetIDs, id)
				placed = true
				continue
			}
			where := "nowhere"
			if host != nil {
				where = host.Name
			}
			t.Logf("target %s landed on %s, which is the ALB's node — terminating and launching another", id, where)
			lbrTerminate(t, fix.AWS, []string{id})
		}
		require.Truef(t, placed,
			"no backend landed on a node other than %s in four attempts, so this cluster cannot host the topology "+
				"this scenario needs", f.Host.Name)
	}
}

func lbrLaunchTarget(t *testing.T, fix *Fixture, f *albFixture) string {
	t.Helper()
	// A backend has to be placed relative to the ALB's node, which a shared
	// instance fixture cannot express.
	// e2e:allow-create
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(f.AMIID),
		InstanceType:     aws.String(f.InstanceType),
		KeyName:          aws.String(f.KeyName),
		SubnetId:         aws.String(f.SubnetID),
		SecurityGroupIds: []*string{aws.String(f.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(targetUserData))),
	})
	require.NoError(t, err, "launch a backend")
	require.NotEmpty(t, out.Instances, "RunInstances returned no instance")
	id := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() { lbrTerminate(t, fix.AWS, []string{id}) })
	return id
}

func lbrTerminate(t *testing.T, c *harness.AWSClient, ids []string) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	awsIDs := make([]*string, len(ids))
	for i, id := range ids {
		awsIDs[i] = aws.String(id)
	}
	if _, err := c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: awsIDs}); err != nil {
		t.Logf("terminate %v: %v", ids, err)
		return
	}
	harness.WaitForInstanceTerminated(t, c, ids, 2*time.Minute)
}

func lbrRegisterTargets(t *testing.T, c *harness.AWSClient, f *albFixture) {
	t.Helper()
	targets := make([]*elbv2.TargetDescription, len(f.TargetIDs))
	for i, id := range f.TargetIDs {
		targets[i] = &elbv2.TargetDescription{Id: aws.String(id)}
	}
	_, err := c.ELBv2.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(f.TGArn),
		Targets:        targets,
	})
	require.NoError(t, err, "register targets")
	t.Cleanup(func() {
		if _, err := c.ELBv2.DeregisterTargets(&elbv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(f.TGArn),
			Targets:        targets,
		}); err != nil {
			t.Logf("deregister targets: %v", err)
		}
	})
}
