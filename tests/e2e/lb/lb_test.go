//go:build e2e

package lb

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"text/template"
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
	artifacts := harness.ArtifactDir(t, env)

	client := harness.NewAWSClient(t, env)
	fixture := setupSharedFixture(t, client, artifacts)

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
		runLBSuite(t, client, fixture, kindNLB, "internal", nil, "")
	})
}

// --- Fixture: shared VPC, subnet, IGW, SG, app instances ----------------

type sharedFixture struct {
	VPCID          string
	SubnetID       string
	IGWID          string
	SecurityGroup  string
	AMIID          string
	InstanceType   string
	AppInstanceIDs []string
}

func setupSharedFixture(t *testing.T, c *harness.AWSClient, artifacts string) *sharedFixture {
	t.Helper()
	f := &sharedFixture{}
	f.InstanceType = discoverNanoInstanceType(t, c)
	f.AMIID = discoverAMI(t, c)
	ensureKeyPair(t, c)
	t.Cleanup(func() { deleteKeyPair(t, c) })

	createVPC(t, c, f)
	t.Cleanup(func() { deleteVPC(t, c, f) })

	createIGW(t, c, f)
	t.Cleanup(func() { deleteIGW(t, c, f) })

	createSubnet(t, c, f)
	t.Cleanup(func() { deleteSubnet(t, c, f) })

	configureDefaultSG(t, c, f)
	launchAppInstances(t, c, f)
	t.Cleanup(func() { terminateInstances(t, c, f.AppInstanceIDs) })

	harness.OnFailure(t, func() {
		dumpDaemonLogs(t, artifacts, "setup")
	})
	return f
}

//go:embed testdata/client-userdata.sh.tmpl
var clientUserDataTmpl string

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

	for _, port := range []int64{httpPort, tcpPort} {
		_, err := c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:    aws.String(f.SecurityGroup),
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(port),
			ToPort:     aws.Int64(port),
			CidrIp:     aws.String("0.0.0.0/0"),
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

	eni := lbENI(t, c, scheme, eniDescPrefix, lb.ID)

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
	if _, err := c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.ARN),
	}); err != nil {
		t.Logf("delete LB %s: %v", lb.ARN, err)
		return
	}
	prefix, lbName := "app", "alb"
	if lb.Type == "network" {
		prefix, lbName = "net", "nlb"
	}
	suffix := "int"
	if lb.Scheme == "internet-facing" {
		suffix = "inet"
	}
	filter := fmt.Sprintf("ELB %s/lb-e2e-%s-%s/%s", prefix, lbName, suffix, lb.ID)
	harness.WaitForENICleanup(t, c, filter, lb.ARN, 30*time.Second)
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

func lbENI(t *testing.T, c *harness.AWSClient, scheme, prefix, lbID string) *ec2.NetworkInterface {
	t.Helper()
	suffix := "int"
	if scheme == "internet-facing" {
		suffix = "inet"
	}
	lbName := "alb"
	if prefix == "net" {
		lbName = "nlb"
	}
	desc := fmt.Sprintf("ELB %s/lb-e2e-%s-%s/%s", prefix, lbName, suffix, lbID)
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

// --- Internal traffic via per-suite client VM -----------------------------

// runInternalTrafficViaClient launches a fresh client VM whose user-data
// probes the supplied LB private IP from inside the VPC, then fetches the
// results file from the client's public-port-80 server. One client per
// suite (ALB OR NLB) keeps peak instance count at 4 (2 app + 1 LB +
// 1 client) on capacity-constrained dev nodes.
func runInternalTrafficViaClient(t *testing.T, c *harness.AWSClient, f *sharedFixture, kind lbKind, lbIP string) {
	t.Helper()
	var albIP, nlbIP, resultsFile, proto string
	if kind == kindALB {
		albIP, resultsFile, proto = lbIP, "alb_results.txt", "http"
	} else {
		nlbIP, resultsFile, proto = lbIP, "nlb_results.txt", "tcp"
	}

	userData, err := renderClientUserData(albIP, nlbIP, probesPerRun)
	require.NoError(t, err)

	out, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(f.AMIID),
		InstanceType: aws.String(f.InstanceType),
		KeyName:      aws.String(lbKeyName),
		SubnetId:     aws.String(f.SubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     aws.String(base64Encode(userData)),
	})
	require.NoErrorf(t, err, "run-instances client (%s)", kind)
	clientID := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() { terminateInstances(t, c, []string{clientID}) })
	t.Logf("%s client: %s", kind, clientID)

	harness.WaitForInstanceRunning(t, c, clientID, 120*time.Second)
	eni := harness.InstanceENI(t, c, clientID)
	clientIP := publicIP(eni)
	require.NotEmpty(t, clientIP, "client VM needs public IP")

	statusURL := fmt.Sprintf("http://%s:%d/status.txt", clientIP, httpPort)
	harness.Eventually(t, func() bool {
		body, err := plainHTTPGet(statusURL, 5*time.Second)
		return err == nil && strings.TrimSpace(body) == "done"
	}, 5*time.Minute, 5*time.Second, "client probe did not complete")

	results, err := plainHTTPGet(fmt.Sprintf("http://%s:%d/%s", clientIP, httpPort, resultsFile), 10*time.Second)
	require.NoErrorf(t, err, "fetch %s", resultsFile)
	harness.AssertRoundRobin(t, harness.VerifyResultsLines(results, proto), 1, probesPerRun/2, string(kind)+" internal")
}

func renderClientUserData(albIP, nlbIP string, n int) (string, error) {
	tmpl, err := template.New("client").Parse(clientUserDataTmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"ALBPrivateIP": albIP,
		"NLBPrivateIP": nlbIP,
		"NumRequests":  n,
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
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
