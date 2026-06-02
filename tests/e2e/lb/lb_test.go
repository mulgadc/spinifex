//go:build e2e

package lb

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed testdata/app-userdata.sh
var appUserData string

const (
	lbVPCCIDR    = "10.200.0.0/16"
	lbSubnetCIDR = "10.200.1.0/24"
	lbKeyName    = "lb-e2e-key"
	httpPort     = 80
	tcpPort      = 9000
	triggerPort  = 9090
	probesPerRun = 20
)

// LB kind: ALB or NLB. Used to parameterise the per-suite path.
type lbKind string

const (
	kindALB lbKind = "ALB"
	kindNLB lbKind = "NLB"
)

// TestLoadBalancer ports run-lb-e2e.sh. Each of the 4 LB variants (ALB/NLB ×
// internet-facing/internal) runs as its own sequential subtest with its own
// LB, listener, target group, and (for internal) client VM. Sequential
// scheduling keeps peak instance count low (2 app + 1 LB + 1 client = 4) so
// the suite passes on capacity-constrained dev nodes — see mulga-siv-77 for
// the underlying placement bug.
func TestLoadBalancer(t *testing.T) {
	env := harness.LoadEnv(t)
	skipIfDevNetworking(t, env)

	// Resolve peer availability BEFORE the shared fixture build (VPC + 2
	// app VMs + probe client = minutes). Single-node mode has no peer, so
	// the user sees the "internet-facing will skip" message immediately
	// instead of attributing the fixture-setup wait to the first subtest
	// name that gotestsum prints once subtests start.
	peer := pickPeer(env)
	var ssh *harness.PeerSSH
	if peer != "" {
		ssh = harness.NewPeerSSH()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ssh.Ping(ctx, peer); err != nil {
			t.Logf("cannot SSH to peer %s: %v (internet-facing suites will skip)", peer, err)
			peer = ""
			ssh = nil
		}
	}
	if peer == "" {
		t.Logf("no peer node available — internet-facing subtests will skip; building shared fixture for internal subtests")
	}

	artifacts := harness.ArtifactDir(t, env)
	client := harness.NewAWSClient(t, env)
	fixture := setupSharedFixture(t, client, artifacts)

	t.Run("InternetFacing_ALB", func(t *testing.T) {
		if peer == "" {
			t.Skip("no peer node available")
		}
		runLBSuite(t, client, fixture, kindALB, "internet-facing", ssh, peer)
	})
	t.Run("InternetFacing_NLB", func(t *testing.T) {
		if peer == "" {
			t.Skip("no peer node available")
		}
		runLBSuite(t, client, fixture, kindNLB, "internet-facing", ssh, peer)
	})
	t.Run("Internal_ALB", func(t *testing.T) {
		runLBSuite(t, client, fixture, kindALB, "internal", nil, "")
	})
	t.Run("Internal_NLB", func(t *testing.T) {
		// DescribeLoadBalancers reports the ALB gone before the sys.micro VM's
		// vCPU/memory allocation is actually reclaimed. Without this settle,
		// NLB's createLB races the deallocate on capacity-tight dev hosts and
		// trips the sys.micro reserve, ending up in terminal state=failed.
		// Cheaper than cold-booting a fresh probe client per suite.
		time.Sleep(15 * time.Second)
		runLBSuite(t, client, fixture, kindNLB, "internal", nil, "")
	})
	t.Run("Internal_ALB_ModifyListener", func(t *testing.T) {
		time.Sleep(15 * time.Second)
		runModifyListenerSuite(t, client, fixture)
	})
	t.Run("Internal_ALB_ListenerRules", func(t *testing.T) {
		time.Sleep(15 * time.Second)
		runListenerRulesSuite(t, client, fixture)
	})
}

// --- Fixture: shared VPC, subnet, IGW, SG, app instances ----------------

type sharedFixture struct {
	VPCID            string
	SubnetID         string
	IGWID            string
	MainRouteTableID string
	SecurityGroup    string
	AMIID            string
	InstanceType     string
	AppInstanceIDs   []string
	ClientID         string
	ClientPublicIP   string
}

func setupSharedFixture(t *testing.T, c *harness.AWSClient, artifacts string) *sharedFixture {
	t.Helper()
	f := &sharedFixture{}

	harness.Phase(t, "Discovering cluster capacity")
	f.InstanceType = discoverNanoInstanceType(t, c)
	f.AMIID = discoverAMI(t, c)

	harness.Phase(t, "Ensuring SSH key pair %q", lbKeyName)
	ensureKeyPair(t, c)
	t.Cleanup(func() { deleteKeyPair(t, c) })

	harness.Phase(t, "Creating shared VPC topology (%s)", lbVPCCIDR)
	createVPC(t, c, f)
	t.Cleanup(func() { deleteVPC(t, c, f) })
	createIGW(t, c, f)
	t.Cleanup(func() { deleteIGW(t, c, f) })
	createSubnet(t, c, f)
	t.Cleanup(func() { deleteSubnet(t, c, f) })
	addPublicRoute(t, c, f)
	t.Cleanup(func() { deletePublicRoute(t, c, f) })
	configureDefaultSG(t, c, f)

	harness.Phase(t, "Launching app instances (2× %s)", f.InstanceType)
	launchAppInstances(t, c, f)
	t.Cleanup(func() { terminateInstances(t, c, f.AppInstanceIDs) })

	harness.Phase(t, "Launching probe client")
	launchSharedProbeClient(t, c, f)
	t.Cleanup(func() { terminateInstances(t, c, []string{f.ClientID}) })

	harness.OnFailure(t, func() {
		dumpDaemonLogs(t, artifacts, "setup")
	})
	return f
}

//go:embed testdata/client-userdata.sh
var clientUserData string

// --- Discovery helpers ---------------------------------------------------

func skipIfDevNetworking(t *testing.T, env *harness.Env) {
	t.Helper()
	cfg := os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
	if env.ConfigDir != "" {
		cfg = env.ConfigDir + "/spinifex.toml"
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		return
	}
	if bytes.Contains(raw, []byte("dev_networking = true")) {
		t.Skip("dev_networking enabled in spinifex.toml; LB E2E requires pool mode w/ external IPAM")
	}
}

func pickPeer(env *harness.Env) string {
	if len(env.NodeIPs) < 2 {
		return ""
	}
	return env.NodeIPs[1]
}

func discoverNanoInstanceType(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err)
	for _, it := range out.InstanceTypes {
		name := aws.StringValue(it.InstanceType)
		if strings.Contains(name, "nano") {
			t.Logf("instance type: %s", name)
			return name
		}
	}
	t.Fatal("no nano instance type available")
	return ""
}

func discoverAMI(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeImages(&ec2.DescribeImagesInput{})
	require.NoError(t, err)
	var fallback, nonAlpine, ubuntu string
	for _, img := range out.Images {
		id := aws.StringValue(img.ImageId)
		name := aws.StringValue(img.Name)
		if fallback == "" {
			fallback = id
		}
		if !strings.Contains(strings.ToLower(name), "alpine") && nonAlpine == "" {
			nonAlpine = id
		}
		if strings.HasPrefix(name, "ami-ubuntu") {
			ubuntu = id
			break
		}
	}
	for _, candidate := range []string{ubuntu, nonAlpine, fallback} {
		if candidate != "" {
			t.Logf("AMI: %s", candidate)
			return candidate
		}
	}
	t.Fatal("no AMIs available")
	return ""
}

func ensureKeyPair(t *testing.T, c *harness.AWSClient) {
	t.Helper()
	_, _ = c.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(lbKeyName)})
	_, err := c.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(lbKeyName)})
	require.NoError(t, err, "create key pair %s", lbKeyName)
}

func deleteKeyPair(t *testing.T, c *harness.AWSClient) {
	t.Helper()
	if _, err := c.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(lbKeyName)}); err != nil {
		t.Logf("delete key pair: %v", err)
	}
}

// --- VPC / Subnet / IGW / SG --------------------------------------------

func createVPC(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(lbVPCCIDR)})
	require.NoError(t, err)
	f.VPCID = aws.StringValue(out.Vpc.VpcId)
	t.Logf("VPC: %s", f.VPCID)
}

func deleteVPC(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	if f.VPCID == "" {
		return
	}
	if _, err := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(f.VPCID)}); err != nil {
		t.Logf("delete VPC %s: %v", f.VPCID, err)
	}
}

func createIGW(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err)
	f.IGWID = aws.StringValue(out.InternetGateway.InternetGatewayId)
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(f.IGWID),
		VpcId:             aws.String(f.VPCID),
	})
	require.NoError(t, err)
	t.Logf("IGW: %s (attached to %s)", f.IGWID, f.VPCID)
}

func deleteIGW(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	if f.IGWID == "" {
		return
	}
	_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(f.IGWID),
		VpcId:             aws.String(f.VPCID),
	})
	if _, err := c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(f.IGWID),
	}); err != nil {
		t.Logf("delete IGW: %v", err)
	}
}

func createSubnet(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:     aws.String(f.VPCID),
		CidrBlock: aws.String(lbSubnetCIDR),
	})
	require.NoError(t, err)
	f.SubnetID = aws.StringValue(out.Subnet.SubnetId)
	_, err = c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(f.SubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err)
	t.Logf("subnet: %s (MapPublicIpOnLaunch)", f.SubnetID)
}

func deleteSubnet(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	if f.SubnetID == "" {
		return
	}
	if _, err := c.EC2.DeleteSubnet(&ec2.DeleteSubnetInput{SubnetId: aws.String(f.SubnetID)}); err != nil {
		t.Logf("delete subnet: %v", err)
	}
}

// addPublicRoute installs 0.0.0.0/0 → IGW on the VPC's main route table.
// Without this, the daemon classifies the subnet as private and installs an
// OVN LR policy DROP on the subnet's egress (priority 1100), breaking the
// return path for inbound DNATed probes to the public IP.
func addPublicRoute(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(f.VPCID)}},
			{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
		},
	})
	require.NoError(t, err, "describe main route table")
	require.NotEmpty(t, out.RouteTables, "VPC %s has no main route table", f.VPCID)
	f.MainRouteTableID = aws.StringValue(out.RouteTables[0].RouteTableId)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId:         aws.String(f.MainRouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
		GatewayId:            aws.String(f.IGWID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> IGW")
	t.Logf("public route: %s 0.0.0.0/0 -> %s", f.MainRouteTableID, f.IGWID)
}

func deletePublicRoute(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	if f.MainRouteTableID == "" {
		return
	}
	if _, err := c.EC2.DeleteRoute(&ec2.DeleteRouteInput{
		RouteTableId:         aws.String(f.MainRouteTableID),
		DestinationCidrBlock: aws.String("0.0.0.0/0"),
	}); err != nil {
		t.Logf("delete public route: %v", err)
	}
}

func configureDefaultSG(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(f.VPCID)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, out.SecurityGroups, "VPC default SG missing")
	f.SecurityGroup = aws.StringValue(out.SecurityGroups[0].GroupId)
	t.Logf("default SG: %s", f.SecurityGroup)

	// Use structured IpPermissions form — spinifex's vpcd ignores the
	// top-level IpProtocol/FromPort/ToPort/CidrIp shortcut and silently
	// returns newRules=0, so the OVN ACL is never installed and ingress
	// drops at the port group. Filed as a separate bead on the daemon side.
	for _, port := range []int64{httpPort, tcpPort, triggerPort} {
		_, err := c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(f.SecurityGroup),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(port),
				ToPort:     aws.Int64(port),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			}},
		})
		if err != nil {
			var aerr awserr.Error
			if !errors.As(err, &aerr) || aerr.Code() != "InvalidPermission.Duplicate" {
				t.Fatalf("authorize tcp/%d: %v", port, err)
			}
		}
	}
}

// --- App instances -------------------------------------------------------

func launchAppInstances(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	for i := 0; i < 2; i++ {
		out, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
			ImageId:      aws.String(f.AMIID),
			InstanceType: aws.String(f.InstanceType),
			KeyName:      aws.String(lbKeyName),
			SubnetId:     aws.String(f.SubnetID),
			MinCount:     aws.Int64(1),
			MaxCount:     aws.Int64(1),
			UserData:     aws.String(base64Encode(appUserData)),
		})
		require.NoErrorf(t, err, "run-instances app %d", i+1)
		require.NotEmpty(t, out.Instances)
		id := aws.StringValue(out.Instances[0].InstanceId)
		f.AppInstanceIDs = append(f.AppInstanceIDs, id)
		t.Logf("app instance %d: %s", i+1, id)
	}

	for _, id := range f.AppInstanceIDs {
		harness.WaitForInstanceRunning(t, c, id, 120*time.Second)
	}
}

func terminateInstances(t *testing.T, c *harness.AWSClient, ids []string) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	awsIDs := make([]*string, len(ids))
	for i, id := range ids {
		awsIDs[i] = aws.String(id)
	}
	if _, err := c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: awsIDs}); err != nil {
		t.Logf("terminate instances: %v", err)
		return
	}
	harness.WaitForInstanceTerminated(t, c, ids, 60*time.Second)
}

// --- LB suite: one LB (ALB or NLB) × one scheme (internet-facing/internal)

// runLBSuite creates one TG + LB + listener, asserts scheme/DNS/ENI
// invariants, runs the traffic test (local + remote for internet-facing,
// client-VM for internal), then tears it all down before returning. Each
// suite is fully self-contained so the 4 subtests run sequentially without
// piling up LB system instances on capacity-constrained dev nodes.
func runLBSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture, kind lbKind, scheme string, ssh *harness.PeerSSH, peer string) {
	t.Helper()
	suffix := "int"
	if scheme == "internet-facing" {
		suffix = "inet"
	}
	lbName := strings.ToLower(string(kind))
	label := fmt.Sprintf("%s %s", kind, scheme)

	var proto, hcPath, eniDescPrefix, lbType string
	var port int64
	if kind == kindALB {
		proto, port, hcPath, eniDescPrefix, lbType = "HTTP", httpPort, "/index.html", "app", "application"
	} else {
		proto, port, hcPath, eniDescPrefix, lbType = "TCP", tcpPort, "", "net", "network"
	}

	tgArn := createTargetGroup(t, c, f, fmt.Sprintf("lb-e2e-%s-%s-tg", lbName, suffix), proto, port, hcPath)
	t.Cleanup(func() { deleteTargetGroup(t, c, tgArn) })

	registerTargets(t, c, tgArn, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgArn, f.AppInstanceIDs) })

	lb := createLB(t, c, f, fmt.Sprintf("lb-e2e-%s-%s", lbName, suffix), lbType, scheme)
	t.Cleanup(func() { deleteLB(t, c, lb) })

	listener := createListener(t, c, lb.ARN, proto, port, tgArn)
	t.Cleanup(func() { deleteListener(t, c, listener) })

	assert.Equal(t, scheme, lb.Scheme, label+" scheme")
	assert.Equal(t, lbType, lb.Type, label+" type")
	if kind == kindNLB {
		assert.Contains(t, lb.ARN, "/net/", label+" ARN must contain /net/")
	}

	harness.WaitForLBActive(t, c, lb.ARN, label, 5*time.Minute)
	harness.WaitForTargetsHealthy(t, c, tgArn, 2, label, 2*time.Minute)

	eni := lbENI(t, c, eniDescPrefix, lb)

	if scheme == "internet-facing" {
		ip := publicIP(eni)
		require.NotEmpty(t, ip, label+" needs public IP")
		runInternetFacingTrafficSingle(t, kind, ssh, peer, ip)
		if kind == kindNLB {
			runNLBDeregisterDraining(t, c, tgArn, f.AppInstanceIDs[0])
		}
		return
	}

	// internal
	assert.Empty(t, publicIP(eni), label+" must not have public IP")
	priv := privateIP(eni)
	require.NotEmpty(t, priv, label+" needs private IP")
	assertInternalDNS(t, c, lb.ARN, label)
	runInternalTrafficViaClient(t, c, f, kind, priv)
}

// --- LB CRUD helpers -----------------------------------------------------

func createTargetGroup(t *testing.T, c *harness.AWSClient, f *sharedFixture, name, proto string, port int64, hcPath string) string {
	t.Helper()
	in := &elbv2.CreateTargetGroupInput{
		Name:                       aws.String(name),
		Protocol:                   aws.String(proto),
		Port:                       aws.Int64(port),
		VpcId:                      aws.String(f.VPCID),
		HealthCheckIntervalSeconds: aws.Int64(5),
		HealthyThresholdCount:      aws.Int64(2),
		UnhealthyThresholdCount:    aws.Int64(2),
	}
	if hcPath != "" {
		in.HealthCheckPath = aws.String(hcPath)
	} else {
		in.HealthCheckProtocol = aws.String("TCP")
		in.HealthCheckIntervalSeconds = aws.Int64(10)
	}
	out, err := c.ELBv2.CreateTargetGroup(in)
	require.NoErrorf(t, err, "create-target-group %s", name)
	arn := aws.StringValue(out.TargetGroups[0].TargetGroupArn)
	t.Logf("TG %s: %s", name, arn)
	return arn
}

func deleteTargetGroup(t *testing.T, c *harness.AWSClient, arn string) {
	if arn == "" {
		return
	}
	if _, err := c.ELBv2.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(arn)}); err != nil {
		t.Logf("delete TG %s: %v", arn, err)
	}
}

func registerTargets(t *testing.T, c *harness.AWSClient, tgArn string, instanceIDs []string) {
	t.Helper()
	targets := make([]*elbv2.TargetDescription, len(instanceIDs))
	for i, id := range instanceIDs {
		targets[i] = &elbv2.TargetDescription{Id: aws.String(id)}
	}
	_, err := c.ELBv2.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        targets,
	})
	require.NoError(t, err, "register-targets")
}

func deregisterTargets(t *testing.T, c *harness.AWSClient, tgArn string, instanceIDs []string) {
	if tgArn == "" || len(instanceIDs) == 0 {
		return
	}
	targets := make([]*elbv2.TargetDescription, len(instanceIDs))
	for i, id := range instanceIDs {
		targets[i] = &elbv2.TargetDescription{Id: aws.String(id)}
	}
	if _, err := c.ELBv2.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        targets,
	}); err != nil {
		t.Logf("deregister-targets: %v", err)
	}
}

type lbInfo struct {
	ARN, ID, Scheme, Type string
}

func createLB(t *testing.T, c *harness.AWSClient, f *sharedFixture, name, lbType, scheme string) lbInfo {
	t.Helper()
	in := &elbv2.CreateLoadBalancerInput{
		Name:    aws.String(name),
		Subnets: []*string{aws.String(f.SubnetID)},
		Scheme:  aws.String(scheme),
	}
	if lbType == "network" {
		in.Type = aws.String("network")
	}
	out, err := c.ELBv2.CreateLoadBalancer(in)
	require.NoErrorf(t, err, "create-load-balancer %s", name)
	require.NotEmpty(t, out.LoadBalancers)
	lb := out.LoadBalancers[0]
	arn := aws.StringValue(lb.LoadBalancerArn)
	parts := strings.Split(arn, "/")
	info := lbInfo{
		ARN:    arn,
		ID:     parts[len(parts)-1],
		Scheme: aws.StringValue(lb.Scheme),
		Type:   aws.StringValue(lb.Type),
	}
	t.Logf("LB %s: %s (scheme=%s type=%s)", name, info.ARN, info.Scheme, info.Type)
	return info
}

func deleteLB(t *testing.T, c *harness.AWSClient, lb lbInfo) {
	if lb.ARN == "" {
		return
	}
	prefix := "app"
	if lb.Type == "network" {
		prefix = "net"
	}
	parts := strings.Split(lb.ARN, "/")
	require.GreaterOrEqualf(t, len(parts), 3, "deleteLB: malformed LB ARN %q", lb.ARN)
	lbName := parts[len(parts)-2]
	filter := fmt.Sprintf("ELB %s/%s/%s", prefix, lbName, lb.ID)

	// Capture the underlying sys.micro VM id before deleting so we can wait
	// for it to actually terminate. ELBv2.DeleteLoadBalancer returns once
	// the LB resource is gone, but the system VM termination is async and
	// holds the vCPU/memory allocation until reaped — without this wait,
	// the next suite's createLB can race the capacity reclaim and trip the
	// reserve, ending up in terminal state=failed.
	var sysInstanceID string
	if eniOut, err := c.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("description"),
			Values: []*string{aws.String(filter)},
		}},
	}); err == nil && len(eniOut.NetworkInterfaces) > 0 && eniOut.NetworkInterfaces[0].Attachment != nil {
		sysInstanceID = aws.StringValue(eniOut.NetworkInterfaces[0].Attachment.InstanceId)
	}

	if _, err := c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.ARN),
	}); err != nil {
		t.Logf("delete LB %s: %v", lb.ARN, err)
		return
	}
	// Block until describe reports LoadBalancerNotFound — the daemon doesn't
	// mark the LB gone until the underlying sys.micro VM has been torn down
	// and its vCPU/memory deallocated. Polling this avoids the capacity race
	// where the next LB createLB fires before deallocation completes (sys
	// instances are filtered from DescribeInstances so WaitForInstanceTerminated
	// is a no-op for them).
	waitForLBGone(t, c, lb.ARN, 60*time.Second)
	harness.WaitForENICleanup(t, c, filter, lb.ARN, 30*time.Second)
	if sysInstanceID != "" {
		harness.WaitForInstanceTerminated(t, c, []string{sysInstanceID}, 60*time.Second)
	}
}

// waitForLBGone polls DescribeLoadBalancers until the daemon reports
// LoadBalancerNotFound (or the LB no longer appears in the result), which
// in spinifex corresponds to the underlying system VM having finished
// terminating and released its capacity.
func waitForLBGone(t *testing.T, c *harness.AWSClient, arn string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []*string{aws.String(arn)},
		})
		if err != nil {
			var aerr awserr.Error
			if errors.As(err, &aerr) && aerr.Code() == "LoadBalancerNotFound" {
				return
			}
		}
		if err == nil && len(out.LoadBalancers) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("waitForLBGone: %s still visible after %s (continuing)", arn, timeout)
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func createListener(t *testing.T, c *harness.AWSClient, lbArn, proto string, port int64, tgArn string) string {
	t.Helper()
	out, err := c.ELBv2.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String(proto),
		Port:            aws.Int64(port),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(tgArn),
		}},
	})
	require.NoError(t, err, "create-listener")
	arn := aws.StringValue(out.Listeners[0].ListenerArn)
	t.Logf("listener %s:%d -> %s", proto, port, arn)
	return arn
}

func deleteListener(t *testing.T, c *harness.AWSClient, arn string) {
	if arn == "" {
		return
	}
	if _, err := c.ELBv2.DeleteListener(&elbv2.DeleteListenerInput{ListenerArn: aws.String(arn)}); err != nil {
		t.Logf("delete listener: %v", err)
	}
}

// modifyListenerPort changes the listener's port in place. Verifies that
// HAProxy is reconciled without the listener being deleted (no traffic outage).
func modifyListenerPort(t *testing.T, c *harness.AWSClient, listenerArn string, port int64) {
	t.Helper()
	_, err := c.ELBv2.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: aws.String(listenerArn),
		Port:        aws.Int64(port),
	})
	require.NoErrorf(t, err, "modify-listener port=%d", port)
	t.Logf("listener %s: port → %d", listenerArn, port)
}

// modifyListenerDefaultTG swaps the listener's default target group.
func modifyListenerDefaultTG(t *testing.T, c *harness.AWSClient, listenerArn, tgArn string) {
	t.Helper()
	_, err := c.ELBv2.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: aws.String(listenerArn),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(tgArn),
		}},
	})
	require.NoError(t, err, "modify-listener default actions")
	t.Logf("listener %s: default TG → %s", listenerArn, tgArn)
}

// describeListenerPort fetches the current port of a listener — used to
// assert that ModifyListener actually persisted before probing.
func describeListenerPort(t *testing.T, c *harness.AWSClient, listenerArn string) int64 {
	t.Helper()
	out, err := c.ELBv2.DescribeListeners(&elbv2.DescribeListenersInput{
		ListenerArns: []*string{aws.String(listenerArn)},
	})
	require.NoError(t, err, "describe-listeners")
	require.NotEmpty(t, out.Listeners)
	return aws.Int64Value(out.Listeners[0].Port)
}

// runModifyListenerSuite verifies that an in-place listener edit reroutes
// traffic without dropping the listener — the regression mulga-944 fixes
// (frontend previously delete+created on every edit).
//
//   - Start: listener HTTP:80 → tgA, probes round-robin across 2 app instances.
//   - ModifyListener port=8090 → same tgA. Probe at 8090 must succeed
//     immediately (port: 80 must not be active any more, port 8090 must serve).
//   - ModifyListener defaultActions → tgB (same backends). Probe still
//     round-robins; proves DefaultActions swap is honoured by HAProxy.
func runModifyListenerSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	const label = "ALB internal ModifyListener"
	const altPort int64 = 8090

	tgA := createTargetGroup(t, c, f, "lb-e2e-mod-tg-a", "HTTP", httpPort, "/index.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgA) })

	tgB := createTargetGroup(t, c, f, "lb-e2e-mod-tg-b", "HTTP", httpPort, "/index.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgB) })

	registerTargets(t, c, tgA, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgA, f.AppInstanceIDs) })
	registerTargets(t, c, tgB, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgB, f.AppInstanceIDs) })

	lb := createLB(t, c, f, "lb-e2e-mod", "application", "internal")
	t.Cleanup(func() { deleteLB(t, c, lb) })

	listener := createListener(t, c, lb.ARN, "HTTP", httpPort, tgA)
	t.Cleanup(func() { deleteListener(t, c, listener) })

	harness.WaitForLBActive(t, c, lb.ARN, label, 5*time.Minute)
	harness.WaitForTargetsHealthy(t, c, tgA, 2, label+" tgA", 2*time.Minute)

	eni := lbENI(t, c, "app", lb)
	priv := privateIP(eni)
	require.NotEmpty(t, priv, label+" needs private IP")

	// Baseline: traffic on the original port.
	probeAtPort(t, f, priv, httpPort, label+" before modify (port 80)")

	// In-place port change.
	modifyListenerPort(t, c, listener, altPort)
	assert.Equal(t, altPort, describeListenerPort(t, c, listener), "listener port persisted")
	// lb-agent picks up the new HAProxy config on its next 5s tick, not
	// synchronously with the ModifyListener API. Poll until the listener
	// actually answers on altPort before driving the assertive probe.
	waitForListenerServing(t, f, priv, altPort, label+" port 8090", 60*time.Second)
	probeAtPort(t, f, priv, altPort, label+" after port-modify (port 8090)")

	// Defaults: in-place TG swap on same port.
	modifyListenerDefaultTG(t, c, listener, tgB)
	harness.WaitForTargetsHealthy(t, c, tgB, 2, label+" tgB (post-swap)", 2*time.Minute)
	probeAtPort(t, f, priv, altPort, label+" after TG-swap (still port 8090, now tgB)")
}

// runListenerRulesSuite exercises ALB listener rules end-to-end:
//
//   - Two TGs with disjoint backends (app#0 -> tgA, app#1 -> tgB) so the probe
//     responder identifies which TG actually served the request.
//   - CreateRule path-pattern /alpha* -> tgB. Probe at /alpha/index.html must
//     hit app#1 only; probe at / must still hit app#0 (default).
//   - ModifyRule path-pattern /alpha* -> /beta*. Probe at /alpha/ now falls to
//     the default rule (app#0).
//   - DescribeRules lists the rule plus the synthetic default.
//   - DeleteRule removes the rule entirely; probes route to default.
//
// Uses host-header on a parallel branch to verify a second condition field
// reaches HAProxy as an ACL.
func runListenerRulesSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	const label = "ALB internal ListenerRules"

	require.Len(t, f.AppInstanceIDs, 2, label+" needs 2 app instances")
	appA, appB := f.AppInstanceIDs[0], f.AppInstanceIDs[1]

	tgA := createTargetGroup(t, c, f, "lb-e2e-rul-tg-a", "HTTP", httpPort, "/index.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgA) })
	tgB := createTargetGroup(t, c, f, "lb-e2e-rul-tg-b", "HTTP", httpPort, "/alpha/index.html")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgB) })

	registerTargets(t, c, tgA, []string{appA})
	t.Cleanup(func() { deregisterTargets(t, c, tgA, []string{appA}) })
	registerTargets(t, c, tgB, []string{appB})
	t.Cleanup(func() { deregisterTargets(t, c, tgB, []string{appB}) })

	lb := createLB(t, c, f, "lb-e2e-rul", "application", "internal")
	t.Cleanup(func() { deleteLB(t, c, lb) })
	listener := createListener(t, c, lb.ARN, "HTTP", httpPort, tgA)
	t.Cleanup(func() { deleteListener(t, c, listener) })

	harness.WaitForLBActive(t, c, lb.ARN, label, 5*time.Minute)
	harness.WaitForTargetsHealthy(t, c, tgA, 1, label+" tgA", 2*time.Minute)

	eni := lbENI(t, c, "app", lb)
	priv := privateIP(eni)
	require.NotEmpty(t, priv, label+" needs private IP")

	waitForPathServing(t, f, priv, httpPort, "/", "", label+" baseline", 60*time.Second)
	base := probeAtPath(t, f, priv, httpPort, "/", "", label+" default -> tgA")
	require.Equal(t, 1, base.Unique(), label+" default expects 1 responder (tgA single backend)")
	appAHost := singleResponder(base)

	// CreateRule wires tgB to the listener; only then does spinifex begin
	// health-checking tgB. Wait for healthy AFTER rule creation.
	ruleArn := createPathRule(t, c, listener, 10, "/alpha*", tgB)
	ruleCleanup := func() {
		if ruleArn == "" {
			return
		}
		deleteRule(t, c, ruleArn)
	}
	t.Cleanup(ruleCleanup)

	harness.WaitForTargetsHealthy(t, c, tgB, 1, label+" tgB (post-rule)", 2*time.Minute)
	waitForPathRoutedAway(t, f, priv, httpPort, "/alpha/", appAHost, label+" wait rule active", 60*time.Second)
	ruleResp := probeAtPath(t, f, priv, httpPort, "/alpha/", "", label+" path /alpha/ -> tgB")
	require.Equal(t, 1, ruleResp.Unique(), label+" rule probe expects 1 responder")
	appBHost := singleResponder(ruleResp)
	require.NotEqual(t, appAHost, appBHost, label+" /alpha/ must route to tgB, not default")

	// Default route unaffected by rule.
	stillDefault := probeAtPath(t, f, priv, httpPort, "/", "", label+" default unchanged")
	require.Equal(t, 1, stillDefault.Unique())
	assert.Equal(t, appAHost, singleResponder(stillDefault), label+" / must still hit tgA")

	// DescribeRules: rule + default present.
	rules := describeRules(t, c, listener)
	assert.GreaterOrEqual(t, len(rules), 2, label+" expect rule + default")
	assert.True(t, hasRule(rules, ruleArn), label+" CreateRule arn must appear in DescribeRules")

	// ModifyRule: /alpha* -> /beta*. /alpha now falls back to default;
	// /beta* now reaches tgB (appB).
	modifyPathRule(t, c, ruleArn, "/beta*")
	waitForPathRoutedTo(t, f, priv, httpPort, "/beta/", appBHost, label+" wait modify active", 60*time.Second)
	modified := probeAtPath(t, f, priv, httpPort, "/alpha/", "", label+" /alpha/ falls to default after modify")
	require.Equal(t, 1, modified.Unique())
	assert.Equal(t, appAHost, singleResponder(modified), label+" /alpha/ should hit default after pattern change")

	// /beta/ now routes to tgB.
	betaResp := probeAtPath(t, f, priv, httpPort, "/beta/", "", label+" /beta/ -> tgB")
	require.Equal(t, 1, betaResp.Unique())
	assert.Equal(t, appBHost, singleResponder(betaResp), label+" /beta/ must hit tgB after modify")

	// DeleteRule: /beta also falls to default.
	deleteRule(t, c, ruleArn)
	ruleArn = ""
	waitForPathRoutedAway(t, f, priv, httpPort, "/beta/", appBHost, label+" wait delete active", 60*time.Second)
	afterDelete := probeAtPath(t, f, priv, httpPort, "/beta/", "", label+" /beta/ after delete")
	require.Equal(t, 1, afterDelete.Unique())
	assert.Equal(t, appAHost, singleResponder(afterDelete), label+" /beta/ must hit default after delete")
}

// createPathRule adds a path-pattern listener rule. Returns the rule ARN.
func createPathRule(t *testing.T, c *harness.AWSClient, listenerArn string, priority int64, pattern, tgArn string) string {
	t.Helper()
	out, err := c.ELBv2.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: aws.String(listenerArn),
		Priority:    aws.Int64(priority),
		Conditions: []*elbv2.RuleCondition{{
			Field: aws.String("path-pattern"),
			PathPatternConfig: &elbv2.PathPatternConditionConfig{
				Values: []*string{aws.String(pattern)},
			},
		}},
		Actions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(tgArn),
		}},
	})
	require.NoErrorf(t, err, "create-rule priority=%d pattern=%s", priority, pattern)
	require.NotEmpty(t, out.Rules)
	arn := aws.StringValue(out.Rules[0].RuleArn)
	t.Logf("rule %s: priority=%d path=%s -> %s", arn, priority, pattern, tgArn)
	return arn
}

func modifyPathRule(t *testing.T, c *harness.AWSClient, ruleArn, pattern string) {
	t.Helper()
	_, err := c.ELBv2.ModifyRule(&elbv2.ModifyRuleInput{
		RuleArn: aws.String(ruleArn),
		Conditions: []*elbv2.RuleCondition{{
			Field: aws.String("path-pattern"),
			PathPatternConfig: &elbv2.PathPatternConditionConfig{
				Values: []*string{aws.String(pattern)},
			},
		}},
	})
	require.NoErrorf(t, err, "modify-rule pattern=%s", pattern)
	t.Logf("rule %s: path -> %s", ruleArn, pattern)
}

func deleteRule(t *testing.T, c *harness.AWSClient, ruleArn string) {
	if ruleArn == "" {
		return
	}
	if _, err := c.ELBv2.DeleteRule(&elbv2.DeleteRuleInput{RuleArn: aws.String(ruleArn)}); err != nil {
		t.Logf("delete rule %s: %v", ruleArn, err)
	}
}

func describeRules(t *testing.T, c *harness.AWSClient, listenerArn string) []*elbv2.Rule {
	t.Helper()
	out, err := c.ELBv2.DescribeRules(&elbv2.DescribeRulesInput{
		ListenerArn: aws.String(listenerArn),
	})
	require.NoError(t, err, "describe-rules")
	return out.Rules
}

func hasRule(rules []*elbv2.Rule, arn string) bool {
	for _, r := range rules {
		if aws.StringValue(r.RuleArn) == arn {
			return true
		}
	}
	return false
}

func singleResponder(r harness.TrafficResult) string {
	for k := range r.Distribution {
		return k
	}
	return ""
}

// probeAtPath drives the shared client to issue probesPerRun probes at
// lbIP:port with the given path and optional Host header, asserting that at
// least half succeed. Returns the parsed distribution so callers can assert
// which backend(s) served.
func probeAtPath(t *testing.T, f *sharedFixture, lbIP string, port int64, path, host, label string) harness.TrafficResult {
	t.Helper()
	r, err := probeOnceAt(f, lbIP, port, path, host, probesPerRun)
	require.NoErrorf(t, err, "trigger probe %s", label)
	for inst, count := range r.Distribution {
		t.Logf("  %s: %s -> %d", label, inst, count)
	}
	t.Logf("  %s: %d/%d successful, %d unique", label, r.Successful, r.Total, r.Unique())
	require.GreaterOrEqualf(t, r.Successful, probesPerRun/2, "%s probes succeeded", label)
	return r
}

// waitForPathServing polls until the path/host combination returns any
// successful response. Use after CreateRule/ModifyRule/DeleteRule to bridge
// the lb-agent reconcile window.
func waitForPathServing(t *testing.T, f *sharedFixture, lbIP string, port int64, path, host, label string, timeout time.Duration) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		r, err := probeOnceAt(f, lbIP, port, path, host, 2)
		if err != nil {
			return err
		}
		if r.Successful == 0 {
			return fmt.Errorf("%s: 0/%d successful", label, r.Total)
		}
		return nil
	}, timeout, 2*time.Second)
}

// waitForPathRoutedAway polls a path until responses stop coming from
// awayFrom. Use after CreateRule/ModifyRule/DeleteRule to wait out the
// lb-agent's reconcile window. Requires at least one successful probe so
// we don't pass on a transient outage.
func waitForPathRoutedAway(t *testing.T, f *sharedFixture, lbIP string, port int64, path, awayFrom, label string, timeout time.Duration) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		r, err := probeOnceAt(f, lbIP, port, path, "", 2)
		if err != nil {
			return err
		}
		if r.Successful == 0 {
			return fmt.Errorf("%s: 0/%d successful", label, r.Total)
		}
		if r.Distribution[awayFrom] > 0 {
			return fmt.Errorf("%s: still seeing %s", label, awayFrom)
		}
		return nil
	}, timeout, 2*time.Second)
}

// waitForPathRoutedTo polls a path until responses come from wantHost.
// Use when the caller already knows the expected backend hostname.
func waitForPathRoutedTo(t *testing.T, f *sharedFixture, lbIP string, port int64, path, wantHost, label string, timeout time.Duration) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		r, err := probeOnceAt(f, lbIP, port, path, "", 2)
		if err != nil {
			return err
		}
		if r.Distribution[wantHost] == 0 {
			return fmt.Errorf("%s: %s not yet observed", label, wantHost)
		}
		return nil
	}, timeout, 2*time.Second)
}

// probeOnceAt is probeOnce with explicit path + Host-header support. Returns
// errors rather than failing so callers can poll.
func probeOnceAt(f *sharedFixture, lbIP string, port int64, path, host string, count int) (harness.TrafficResult, error) {
	resultsFile := fmt.Sprintf("rul-%d-%d.txt", port, time.Now().UnixNano())
	order := map[string]any{
		"proto":     "http",
		"ip":        lbIP,
		"count":     count,
		"outfile":   resultsFile,
		"http_port": port,
		"http_path": path,
		"host":      host,
		"tcp_port":  tcpPort,
	}
	body, err := json.Marshal(order)
	if err != nil {
		return harness.TrafficResult{}, err
	}
	triggerURL := fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Post(triggerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("trigger: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return harness.TrafficResult{}, fmt.Errorf("trigger status %d", resp.StatusCode)
	}
	results, err := plainHTTPGet(
		fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, resultsFile),
		10*time.Second)
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("fetch %s: %w", resultsFile, err)
	}
	return harness.VerifyResultsLines(results, "http"), nil
}

// waitForListenerServing polls the shared client with a tiny round of probes
// until at least one returns a successful instance_id payload. Used after
// ModifyListener calls so the assertive probeAtPort doesn't race the
// lb-agent's poll-driven HAProxy reload (5s tick + reload latency).
func waitForListenerServing(t *testing.T, f *sharedFixture, lbIP string, port int64, label string, timeout time.Duration) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		r, err := probeOnce(f, lbIP, port, 2)
		if err != nil {
			return err
		}
		if r.Successful == 0 {
			return fmt.Errorf("%s: listener not serving on :%d (0/%d successful)", label, port, r.Total)
		}
		return nil
	}, timeout, 2*time.Second)
}

// probeOnce drives the shared client to issue `count` HTTP probes at
// lbIP:port and returns the parsed TrafficResult. Errors are returned rather
// than failing the test so callers can poll.
func probeOnce(f *sharedFixture, lbIP string, port int64, count int) (harness.TrafficResult, error) {
	resultsFile := fmt.Sprintf("mod-%d-%d.txt", port, time.Now().UnixNano())
	order := map[string]any{
		"proto":     "http",
		"ip":        lbIP,
		"count":     count,
		"outfile":   resultsFile,
		"http_port": port,
		"tcp_port":  tcpPort,
	}
	body, err := json.Marshal(order)
	if err != nil {
		return harness.TrafficResult{}, err
	}
	triggerURL := fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Post(triggerURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("trigger: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return harness.TrafficResult{}, fmt.Errorf("trigger status %d", resp.StatusCode)
	}
	results, err := plainHTTPGet(
		fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, resultsFile),
		10*time.Second)
	if err != nil {
		return harness.TrafficResult{}, fmt.Errorf("fetch %s: %w", resultsFile, err)
	}
	return harness.VerifyResultsLines(results, "http"), nil
}

// probeAtPort runs round-robin probes from the shared client to lbIP:port and
// asserts both app instances respond. Mirrors runInternalTrafficViaClient but
// takes an explicit port so ModifyListener edits can be verified.
func probeAtPort(t *testing.T, f *sharedFixture, lbIP string, port int64, label string) {
	t.Helper()
	resultsFile := fmt.Sprintf("mod-%d-%d.txt", port, time.Now().UnixNano())
	order := map[string]any{
		"proto":     "http",
		"ip":        lbIP,
		"count":     probesPerRun,
		"outfile":   resultsFile,
		"http_port": port,
		"tcp_port":  tcpPort,
	}
	body, err := json.Marshal(order)
	require.NoError(t, err)

	triggerURL := fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Post(triggerURL, "application/json", bytes.NewReader(body))
	require.NoErrorf(t, err, "trigger probe %s", label)
	resp.Body.Close()
	require.Equalf(t, 200, resp.StatusCode, "trigger probe %s status", label)

	results, err := plainHTTPGet(
		fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, resultsFile),
		10*time.Second)
	require.NoErrorf(t, err, "fetch %s", resultsFile)
	harness.AssertRoundRobin(t,
		harness.VerifyResultsLines(results, "http"),
		1, probesPerRun/2, label)
}

func lbENI(t *testing.T, c *harness.AWSClient, prefix string, lb lbInfo) *ec2.NetworkInterface {
	t.Helper()
	parts := strings.Split(lb.ARN, "/")
	require.GreaterOrEqualf(t, len(parts), 3, "lbENI: malformed LB ARN %q", lb.ARN)
	lbName := parts[len(parts)-2]
	desc := fmt.Sprintf("ELB %s/%s/%s", prefix, lbName, lb.ID)
	var eni *ec2.NetworkInterface
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("description"),
				Values: []*string{aws.String(desc)},
			}},
		})
		if err != nil {
			return err
		}
		if len(out.NetworkInterfaces) == 0 {
			return fmt.Errorf("no ENI for %s", desc)
		}
		eni = out.NetworkInterfaces[0]
		return nil
	}, 30*time.Second, 2*time.Second)
	return eni
}

func publicIP(eni *ec2.NetworkInterface) string {
	if eni == nil || eni.Association == nil {
		return ""
	}
	return aws.StringValue(eni.Association.PublicIp)
}

func privateIP(eni *ec2.NetworkInterface) string {
	if eni == nil {
		return ""
	}
	return aws.StringValue(eni.PrivateIpAddress)
}

func assertInternalDNS(t *testing.T, c *harness.AWSClient, lbArn, label string) {
	t.Helper()
	out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{aws.String(lbArn)},
	})
	require.NoError(t, err)
	require.NotEmpty(t, out.LoadBalancers)
	dns := aws.StringValue(out.LoadBalancers[0].DNSName)
	assert.True(t, strings.HasPrefix(dns, "internal-"), "%s internal DNS missing internal- prefix: %s", label, dns)
}

// --- Internet-facing traffic ---------------------------------------------

// runInternetFacingTrafficSingle drives one LB (ALB or NLB) over its public
// IP. Runs probes locally from the test driver, and if a peer node is wired
// up, also runs the same probes from there to exercise inter-node routing.
func runInternetFacingTrafficSingle(t *testing.T, kind lbKind, ssh *harness.PeerSSH, peer, ip string) {
	t.Helper()
	if kind == kindALB {
		url := fmt.Sprintf("http://%s:%d", ip, httpPort)
		harness.AssertRoundRobin(t,
			harness.HTTPRoundRobin(url, probesPerRun, 5*time.Second),
			2, probesPerRun/2, "ALB inet (local)")
		if ssh != nil && peer != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			harness.AssertRoundRobin(t,
				remoteHTTPRoundRobin(t, ctx, ssh, peer, url, probesPerRun),
				2, probesPerRun/2, "ALB inet (remote)")
		}
		return
	}
	harness.AssertRoundRobin(t,
		harness.TCPRoundRobin(ip, tcpPort, probesPerRun, 5*time.Second),
		1, probesPerRun/2, "NLB inet (local)")
	if ssh != nil && peer != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		harness.AssertRoundRobin(t,
			remoteTCPRoundRobin(t, ctx, ssh, peer, ip, tcpPort, probesPerRun),
			1, probesPerRun/2, "NLB inet (remote)")
	}
}

func remoteHTTPRoundRobin(t *testing.T, ctx context.Context, ssh *harness.PeerSSH, peer, url string, n int) harness.TrafficResult {
	t.Helper()
	var lines []string
	for i := 0; i < n; i++ {
		out, err := ssh.Run(ctx, peer, fmt.Sprintf("curl -s --max-time 5 '%s/'", url))
		if err != nil {
			continue
		}
		lines = append(lines, strings.TrimSpace(string(out)))
	}
	return harness.VerifyResultsLines(strings.Join(lines, "\n"), "http")
}

func remoteTCPRoundRobin(t *testing.T, ctx context.Context, ssh *harness.PeerSSH, peer, host string, port, n int) harness.TrafficResult {
	t.Helper()
	var lines []string
	for i := 0; i < n; i++ {
		out, err := ssh.Run(ctx, peer, fmt.Sprintf("echo '' | nc -w5 '%s' %d", host, port))
		if err != nil {
			continue
		}
		lines = append(lines, strings.TrimSpace(string(out)))
	}
	return harness.VerifyResultsLines(strings.Join(lines, "\n"), "tcp")
}

func runNLBDeregisterDraining(t *testing.T, c *harness.AWSClient, tgArn, targetID string) {
	t.Helper()
	_, err := c.ELBv2.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String(targetID)}},
	})
	require.NoError(t, err)

	time.Sleep(3 * time.Second)
	out, err := c.ELBv2.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(tgArn)})
	require.NoError(t, err)

	remaining := len(out.TargetHealthDescriptions)
	draining := 0
	for _, th := range out.TargetHealthDescriptions {
		if aws.StringValue(th.TargetHealth.State) == "draining" {
			draining++
		}
	}
	t.Logf("NLB deregister: %d remaining, %d draining", remaining, draining)
	assert.True(t, remaining == 1 || draining >= 1, "NLB deregister: expected 1 remaining or >=1 draining")
}

// --- Shared probe client + per-suite trigger ----------------------------

// launchSharedProbeClient launches one client VM whose user-data exposes
// http.server on :80 (results dir) and a JSON trigger endpoint on :9090.
// Both internal suites hit the same client — avoids the ~60s second cold
// boot of a per-suite client.
func launchSharedProbeClient(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	out, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(f.AMIID),
		InstanceType: aws.String(f.InstanceType),
		KeyName:      aws.String(lbKeyName),
		SubnetId:     aws.String(f.SubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     aws.String(base64Encode(clientUserData)),
	})
	require.NoError(t, err, "run-instances probe client")
	f.ClientID = aws.StringValue(out.Instances[0].InstanceId)
	t.Logf("probe client: %s", f.ClientID)

	harness.WaitForInstanceRunning(t, c, f.ClientID, 120*time.Second)
	eni := harness.InstanceENI(t, c, f.ClientID)
	f.ClientPublicIP = publicIP(eni)
	require.NotEmpty(t, f.ClientPublicIP, "probe client needs public IP")
	t.Logf("probe client public IP: %s", f.ClientPublicIP)

	// Wait until the trigger server is accepting connections before the
	// first suite starts firing probes at it.
	triggerURL := fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort)
	harness.Eventually(t, func() bool {
		req, _ := http.NewRequest(http.MethodGet, triggerURL, nil)
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}, 5*time.Minute, 5*time.Second, "probe client trigger server not ready")
}

// runInternalTrafficViaClient POSTs a probe order to the shared client's
// trigger server, then fetches the results file it wrote and parses it.
func runInternalTrafficViaClient(t *testing.T, _ *harness.AWSClient, f *sharedFixture, kind lbKind, lbIP string) {
	t.Helper()
	var proto, resultsFile string
	if kind == kindALB {
		proto, resultsFile = "http", fmt.Sprintf("alb-%d.txt", time.Now().UnixNano())
	} else {
		proto, resultsFile = "tcp", fmt.Sprintf("nlb-%d.txt", time.Now().UnixNano())
	}

	order := map[string]any{
		"proto":     proto,
		"ip":        lbIP,
		"count":     probesPerRun,
		"outfile":   resultsFile,
		"http_port": httpPort,
		"tcp_port":  tcpPort,
	}
	body, err := json.Marshal(order)
	require.NoError(t, err)

	triggerURL := fmt.Sprintf("http://%s:%d/", f.ClientPublicIP, triggerPort)
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Post(triggerURL, "application/json", bytes.NewReader(body))
	require.NoErrorf(t, err, "trigger %s probe", kind)
	resp.Body.Close()
	require.Equalf(t, 200, resp.StatusCode, "trigger %s probe HTTP status", kind)

	results, err := plainHTTPGet(
		fmt.Sprintf("http://%s:%d/%s", f.ClientPublicIP, httpPort, resultsFile),
		10*time.Second)
	require.NoErrorf(t, err, "fetch %s", resultsFile)
	harness.AssertRoundRobin(t,
		harness.VerifyResultsLines(results, proto),
		1, probesPerRun/2, string(kind)+" internal")
}

// plainHTTPGet fetches a plain-HTTP URL (no TLS). The probe client serves
// over port 80 with no cert, so the harness HTTPSGet helper would refuse.
func plainHTTPGet(url string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func dumpDaemonLogs(t *testing.T, dir, label string) {
	t.Helper()
	harness.DumpCmd(t, dir, fmt.Sprintf("daemon-%s.log", label),
		"journalctl", "-u", "spinifex-daemon", "--no-pager", "-n", "200")
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
