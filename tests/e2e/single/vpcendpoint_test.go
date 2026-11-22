//go:build e2e

package single

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vpceVPCCIDR       = "10.233.0.0/16"
	vpcePubCIDR       = "10.233.1.0/24"
	vpcePrivCIDR      = "10.233.2.0/24"
	vpceSecondaryCIDR = "10.233.3.0/24"
	vpceFenceVPCCIDR  = "10.234.0.0/16"

	vpceObjectKey = "vpce-e2e-marker"
	vpceCredAllow = "/home/ubuntu/.vpce-allow.curl"
	vpceCredSrcIP = "/home/ubuntu/.vpce-sourceip.curl"
	vpceCredVpce  = "/home/ubuntu/.vpce-sourcevpce.curl"
	vpceCredOther = "/home/ubuntu/.vpce-othervpce.curl"

	// vpceConvergeBudget is generous against the ~10 s the node takes to
	// apply an endpoint change; the multinode suite asserts the bound itself.
	vpceConvergeBudget = 60 * time.Second
	// vpceHoldWindow is how long a "still carried" check keeps probing,
	// twice the time a change takes to reach the node.
	vpceHoldWindow = 20 * time.Second
)

// runVPCGatewayEndpoint proves an S3 gateway endpoint is the only path from a
// private subnet to a gate address. Stages after CreateOpensPath need the open
// path and restore what they change; DeleteClosesPath runs last.
func runVPCGatewayEndpoint(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — S3 gateway VPC endpoint: route tables, security groups and deletion on the datapath")
	c := fix.AWS

	harness.Step(t, "wait for the S3 gateway endpoint service to be offered")
	svc := harness.WaitForS3EndpointService(t, c)
	region := harness.S3EndpointRegion(svc)
	plID, entries := harness.S3PrefixList(t, c, svc)
	gate, port := entries[0], harness.S3GatePort()
	harness.Detail(t, "service", svc, "prefix_list", plID, "entries", strings.Join(entries, ","), "gate_port", port)

	t.Run("DeleteVpcFence", func(t *testing.T) { vpceDeleteVpcFence(t, c, svc) })

	if !fix.PublicPool {
		t.Skip("the datapath stages reach their private guest through a bastion, which needs a public address")
	}

	amiID := needAMI(t, fix)
	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)

	// --- Topology: public subnet (bastion) and two private subnets on one table ---

	vpcID := harness.CreateVPC(t, c, vpceVPCCIDR)
	t.Cleanup(func() { harness.DeleteVPCWhenFree(t, c, vpcID, 2*time.Minute) })
	pubSubnetID := harness.CreateSubnet(t, c, vpcID, vpcePubCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, pubSubnetID) })
	privSubnetID := harness.CreateSubnet(t, c, vpcID, vpcePrivCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, privSubnetID) })
	secSubnetID := harness.CreateSubnet(t, c, vpcID, vpceSecondaryCIDR)
	t.Cleanup(func() { harness.DeleteSubnet(t, c, secSubnetID) })
	_, err := c.EC2.ModifySubnetAttribute(&ec2.ModifySubnetAttributeInput{
		SubnetId:            aws.String(pubSubnetID),
		MapPublicIpOnLaunch: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
	})
	require.NoError(t, err, "modify-subnet-attribute MapPublicIpOnLaunch %s", pubSubnetID)

	igwOut, err := c.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	t.Cleanup(func() {
		_, _ = c.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
		})
		_, _ = c.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String(igwID)})
	})
	_, err = c.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	// The main table carries the bastion's internet route; the private
	// subnets get their own table with no route out of the VPC.
	mainRT := harness.MainRouteTableID(t, c, vpcID)
	_, err = c.EC2.CreateRoute(&ec2.CreateRouteInput{
		RouteTableId: aws.String(mainRT), DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(igwID),
	})
	require.NoError(t, err, "create-route 0.0.0.0/0 -> %s on %s", igwID, mainRT)

	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "create-route-table (private)")
	privRT := aws.StringValue(rtOut.RouteTable.RouteTableId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(privRT)})
	})
	for _, sub := range []string{privSubnetID, secSubnetID} {
		assoc, aerr := c.EC2.AssociateRouteTable(&ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(privRT), SubnetId: aws.String(sub),
		})
		require.NoError(t, aerr, "associate-route-table %s <- %s", privRT, sub)
		assocID := aws.StringValue(assoc.AssociationId)
		t.Cleanup(func() {
			_, _ = c.EC2.DisassociateRouteTable(&ec2.DisassociateRouteTableInput{AssociationId: aws.String(assocID)})
		})
	}

	sgOut, err := c.EC2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
		{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
	}})
	require.NoError(t, err, "describe-security-groups default")
	require.NotEmpty(t, sgOut.SecurityGroups, "no default SG for %s", vpcID)
	bastionSG := aws.StringValue(sgOut.SecurityGroups[0].GroupId)
	harness.AuthorizeSSHIngress(t, c, bastionSG)

	privSGOut, err := c.EC2.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId: aws.String(vpcID), GroupName: aws.String("vpce-e2e-private"),
		Description: aws.String("private guest behind the S3 gateway endpoint"),
	})
	require.NoError(t, err, "create-security-group private")
	privSG := aws.StringValue(privSGOut.GroupId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(privSG)})
	})
	_, err = c.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(privSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String(vpceVPCCIDR)}},
		}},
	})
	require.NoError(t, err, "authorize tcp/22 from the VPC on %s", privSG)
	harness.Detail(t, "vpc", vpcID, "priv_rtb", privRT, "priv_subnet", privSubnetID,
		"secondary_subnet", secSubnetID, "priv_sg", privSG, "igw", igwID)

	// --- Guests: bastion, then the private guest under test ---

	bastionID := launchBaselineInstance(t, fix, amiID, instType, keyName, pubSubnetID, []string{bastionSG})
	bastionPub := instancePublicIP(t, fix, bastionID)
	bastionPriv := instancePrivateIP(t, fix, bastionID)
	privID := launchBaselineInstance(t, fix, amiID, instType, keyName, privSubnetID, []string{privSG})
	privIP := instancePrivateIP(t, fix, privID)
	harness.Detail(t, "bastion", bastionID, "bastion_public_ip", bastionPub, "private_instance", privID, "private_ip", privIP)

	if !sshHealth.TryReady(t, bastionPub, 22, keyPath, harness.SSHReadyBudget) {
		harness.DumpVPCFlowDiagnostics(t, c, bastionID, "VPC endpoint bastion SSH timeout",
			harness.VPCDiagnosticsOpts{ExternalIP: bastionPub, LogicalIP: bastionPriv, ArtifactDir: fix.ArtifactDir(t)})
		t.Fatalf("bastion %s never answered SSH within %dms", bastionPub, harness.SSHReadyBudget.Milliseconds())
	}
	bastion := harness.SSHTarget{User: "ubuntu", Host: bastionPub, Port: 22, KeyPath: keyPath}
	scpKey(t, keyPath, bastionPub)
	_ = harness.RunSSH(t, bastion, "chmod 600 /tmp/key.pem")
	harness.Step(t, "wait for the private guest over the bastion")
	harness.EventuallyErr(t, func() error {
		out, gerr := harness.GuestExecViaBastion(bastion, privIP, "hostname", 30*time.Second)
		if gerr != nil {
			return fmt.Errorf("%w: %s", gerr, strings.TrimSpace(out))
		}
		return nil
	}, 3*time.Minute, 5*time.Second)

	// --- Principals: one allowed on the bucket, one allowed only from node addresses ---

	suffix := time.Now().UnixNano()
	bucket := fmt.Sprintf("vpce-e2e-%d", suffix)
	allowed := harness.CreateS3Principal(t, c, fmt.Sprintf("vpce-allow-%d", suffix), harness.S3BucketStatements(bucket))
	harness.CreateS3BucketWithObject(t, gate, allowed, bucket, vpceObjectKey)
	nodeAddrs := vpceNodeAddresses(fix.Env, entries)
	srcIP := harness.CreateS3Principal(t, c, fmt.Sprintf("vpce-srcip-%d", suffix), []map[string]any{{
		"Effect":    "Allow",
		"Action":    "s3:ListBucket",
		"Resource":  "arn:aws:s3:::" + bucket,
		"Condition": map[string]any{"IpAddress": map[string]any{"aws:SourceIp": nodeAddrs}},
	}})
	runnerErr := harness.WaitS3PrincipalLive(t, gate, srcIP, bucket)
	harness.Detail(t, "bucket", bucket, "source_ip_condition", strings.Join(nodeAddrs, ","),
		"runner_request_allowed", runnerErr == nil)
	for path, p := range map[string]harness.S3Principal{vpceCredAllow: allowed, vpceCredSrcIP: srcIP} {
		if _, werr := harness.GuestExecViaBastion(bastion, privIP, harness.GuestS3CurlConfigCmd(path, p), 30*time.Second); werr != nil {
			t.Fatalf("writing the curl config for %s into the guest: %v", p.UserName, werr)
		}
	}

	probe := func(cfg, addr string) func() (harness.GuestCurl, error) {
		return func() (harness.GuestCurl, error) {
			return harness.ProbeGuestS3(bastion, privIP, cfg, region, addr, port, bucket)
		}
	}
	listed := func(g harness.GuestCurl) bool { return g.Code == "200" && strings.Contains(g.Body, vpceObjectKey) }
	timedOut := func(g harness.GuestCurl) bool { return g.TimedOut() }
	allowedProbe := probe(vpceCredAllow, gate)

	t.Run("NoEndpointNoPath", func(t *testing.T) {
		g, err := allowedProbe()
		require.NoError(t, err, "probe the gate from the private guest")
		require.Truef(t, g.TimedOut(),
			"with no endpoint and no route out of the VPC, a connection to %s:%d must time out; got %s", gate, port, g)
	})

	// --- Create: the endpoint opens the path ---

	harness.Step(t, "create-vpc-endpoint %s on %s", svc, privRT)
	ep := harness.CreateS3GatewayEndpoint(t, c, vpcID, svc, privRT)
	epID := aws.StringValue(ep.VpcEndpointId)
	harness.Detail(t, "vpc_endpoint", epID)

	createOK := t.Run("CreateOpensPath", func(t *testing.T) {
		assert.Equal(t, "available", aws.StringValue(ep.State), "an S3 gateway endpoint is available at once")
		assert.Equal(t, ec2.VpcEndpointTypeGateway, aws.StringValue(ep.VpcEndpointType))
		assert.Equal(t, []string{privRT}, aws.StringValueSlice(ep.RouteTableIds))

		r := harness.EndpointRoute(t, c, privRT, plID)
		require.NotNilf(t, r, "%s shows no route to %s after create", privRT, plID)
		assert.Equal(t, epID, aws.StringValue(r.GatewayId), "the endpoint route's target")
		assert.Equal(t, "active", aws.StringValue(r.State), "the endpoint route's state")

		g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe, listed, "a listing of the bucket")
		harness.Detail(t, "after", "create", "probe", g.String(), "converge_ms", ms)
	})
	if !createOK {
		t.Fatalf("CreateOpensPath failed; every later stage depends on the open path")
	}

	// An allow conditioned on aws:SourceVpce matches only through the endpoint it
	// names: the ordinary path and an allow naming another endpoint are denied.
	t.Run("SourceVpcePolicyMatchesOnlyItsEndpoint", func(t *testing.T) {
		onVpce := func(name, id string) harness.S3Principal {
			return harness.CreateS3Principal(t, c, fmt.Sprintf("%s-%d", name, suffix), []map[string]any{{
				"Effect":    "Allow",
				"Action":    "s3:ListBucket",
				"Resource":  "arn:aws:s3:::" + bucket,
				"Condition": map[string]any{"StringEquals": map[string]any{"aws:SourceVpce": id}},
			}})
		}
		named := onVpce("vpce-named", epID)
		other := onVpce("vpce-other", "vpce-00000000000000000")
		direct := harness.WaitS3PrincipalLive(t, gate, named, bucket)
		require.Truef(t, harness.ErrorCodeIs(direct, "AccessDenied"),
			"an allow on aws:SourceVpce %s must not match a request on the ordinary path; got %v", epID, direct)
		for path, p := range map[string]harness.S3Principal{vpceCredVpce: named, vpceCredOther: other} {
			if _, werr := harness.GuestExecViaBastion(bastion, privIP, harness.GuestS3CurlConfigCmd(path, p), 30*time.Second); werr != nil {
				t.Fatalf("writing the curl config for %s into the guest: %v", p.UserName, werr)
			}
		}

		g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, probe(vpceCredVpce, gate), listed, "a listing allowed by aws:SourceVpce")
		harness.Detail(t, "after", "source_vpce_allow", "probe", g.String(), "converge_ms", ms)
		g, err := probe(vpceCredOther, gate)()
		require.NoError(t, err, "probe as %s", other.UserName)
		require.Equalf(t, "403", g.Code, "an allow naming another endpoint must not match; got %s", g)
		assert.Contains(t, g.Body, "AccessDenied")
	})

	// An allow conditioned on aws:SourceIp being a node address must not match
	// a request through the endpoint, which carries no public source address.
	t.Run("SourceIpPolicyDoesNotMatch", func(t *testing.T) {
		g, err := probe(vpceCredSrcIP, gate)()
		require.NoError(t, err, "probe as %s", srcIP.UserName)
		require.Equalf(t, "403", g.Code,
			"a policy allowing aws:SourceIp %v must not allow a request through the endpoint; got %s", nodeAddrs, g)
		assert.Contains(t, g.Body, "AccessDenied")

		g, err = allowedProbe()
		require.NoError(t, err, "control probe as %s", allowed.UserName)
		require.Truef(t, listed(g), "control: the unconditioned principal must still list the bucket; got %s", g)
	})

	// A known divergence: AWS lets a more specific route win over a prefix
	// list route, but our capture runs before routing, so the endpoint still wins.
	t.Run("HostRouteDoesNotOverride", func(t *testing.T) {
		dst := gate + "/32"
		harness.Step(t, "create-route %s -> %s on %s", dst, igwID, privRT)
		_, err := c.EC2.CreateRoute(&ec2.CreateRouteInput{
			RouteTableId: aws.String(privRT), DestinationCidrBlock: aws.String(dst), GatewayId: aws.String(igwID),
		})
		require.NoError(t, err, "create-route %s", dst)
		deleted := false
		t.Cleanup(func() {
			if !deleted {
				_, _ = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(privRT), DestinationCidrBlock: aws.String(dst)})
			}
		})
		require.NotNil(t, harness.EndpointRoute(t, c, privRT, plID), "the endpoint route vanished beside the /32 route")

		probes := vpceHold(t, vpceHoldWindow, allowedProbe, listed, "a listing of the bucket")
		harness.Detail(t, "host_route", dst, "probes_held", probes)

		_, err = c.EC2.DeleteRoute(&ec2.DeleteRouteInput{RouteTableId: aws.String(privRT), DestinationCidrBlock: aws.String(dst)})
		require.NoError(t, err, "delete-route %s", dst)
		deleted = true
	})

	// A known divergence: only the primary interface is captured, so an
	// interface in a subnet whose table holds the endpoint still has no path.
	t.Run("SecondaryENINotCaptured", func(t *testing.T) {
		vpceSecondaryENI(t, fix, bastion, privID, privIP, secSubnetID, privSG, bastionPriv, gate, port)
		g, err := allowedProbe()
		require.NoError(t, err, "control probe on the primary interface")
		require.Truef(t, listed(g), "control: the primary interface must still list the bucket; got %s", g)
	})

	t.Run("EgressSGResets", func(t *testing.T) {
		allEgress := []*ec2.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}}
		only443 := []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}}
		harness.Step(t, "restrict %s egress to tcp/443", privSG)
		_, err := c.EC2.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(privSG), IpPermissions: allEgress})
		if err != nil && !harness.ErrorCodeIs(err, "InvalidPermission.NotFound") {
			require.NoError(t, err, "revoke all egress on %s", privSG)
		}
		restored := false
		restore := func() error {
			if _, rerr := c.EC2.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
				GroupId: aws.String(privSG), IpPermissions: only443,
			}); rerr != nil && !harness.ErrorCodeIs(rerr, "InvalidPermission.NotFound") {
				return rerr
			}
			_, rerr := c.EC2.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
				GroupId: aws.String(privSG), IpPermissions: allEgress,
			})
			if rerr != nil && !harness.ErrorCodeIs(rerr, "InvalidPermission.Duplicate") {
				return rerr
			}
			return nil
		}
		t.Cleanup(func() {
			if !restored {
				if rerr := restore(); rerr != nil {
					t.Logf("cleanup: restore egress on %s: %v", privSG, rerr)
				}
			}
		})
		_, err = c.EC2.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(privSG), IpPermissions: only443})
		require.NoError(t, err, "authorize tcp/443 egress on %s", privSG)

		g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe,
			func(g harness.GuestCurl) bool { return g.Reset() }, "a reset connection (curl exit 7, 35 or 56)")
		harness.Detail(t, "after", "egress_443_only", "probe", g.String(), "converge_ms", ms)

		require.NoError(t, restore(), "restore all egress on %s", privSG)
		restored = true
		g, ms = harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe, listed, "a listing of the bucket")
		harness.Detail(t, "after", "egress_restored", "probe", g.String(), "converge_ms", ms)
	})

	t.Run("EndpointPolicy", func(t *testing.T) {
		vpceEndpointPolicy(t, c, epID, gate, bucket, allowed, func(keys []string) (harness.GuestCurl, error) {
			return harness.ProbeGuestS3DeleteObjects(bastion, privIP, vpceCredAllow, region, gate, port, bucket, keys)
		}, allowedProbe, listed)
	})

	t.Run("ModifyRemovesAndRestores", func(t *testing.T) {
		harness.Step(t, "modify-vpc-endpoint %s remove %s", epID, privRT)
		_, err := c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{
			VpcEndpointId: aws.String(epID), RemoveRouteTableIds: []*string{aws.String(privRT)},
		})
		require.NoError(t, err, "modify-vpc-endpoint remove")
		harness.WaitVpcEndpointRouteTables(t, c, epID)
		assert.Nil(t, harness.EndpointRoute(t, c, privRT, plID), "%s still shows the endpoint route after removal", privRT)
		g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe, timedOut, "a timed-out connection (curl exit 28)")
		harness.Detail(t, "after", "modify_remove", "probe", g.String(), "converge_ms", ms)

		harness.Step(t, "modify-vpc-endpoint %s add %s", epID, privRT)
		_, err = c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{
			VpcEndpointId: aws.String(epID), AddRouteTableIds: []*string{aws.String(privRT)},
		})
		require.NoError(t, err, "modify-vpc-endpoint add")
		harness.WaitVpcEndpointRouteTables(t, c, epID, privRT)
		require.NotNil(t, harness.EndpointRoute(t, c, privRT, plID), "%s shows no endpoint route after re-adding", privRT)
		g, ms = harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe, listed, "a listing of the bucket")
		harness.Detail(t, "after", "modify_add", "probe", g.String(), "converge_ms", ms)
	})

	t.Run("DeleteClosesPath", func(t *testing.T) {
		harness.Step(t, "delete-vpc-endpoints %s while %s is associated", epID, privRT)
		out, err := c.EC2.DeleteVpcEndpoints(&ec2.DeleteVpcEndpointsInput{VpcEndpointIds: []*string{aws.String(epID)}})
		require.NoError(t, err, "delete-vpc-endpoints")
		require.Empty(t, out.Unsuccessful, "delete-vpc-endpoints reported failures")
		harness.EventuallyErr(t, func() error {
			if _, live, derr := harness.DescribeVpcEndpoint(c, epID); derr != nil || live {
				return fmt.Errorf("vpc endpoint %s still described (err=%w)", epID, derr)
			}
			if harness.EndpointRoute(t, c, privRT, plID) != nil {
				return fmt.Errorf("%s still shows the route to %s", privRT, plID)
			}
			return nil
		}, 30*time.Second, 2*time.Second)
		g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, allowedProbe, timedOut, "a timed-out connection (curl exit 28)")
		harness.Detail(t, "after", "delete", "probe", g.String(), "converge_ms", ms)
	})
}

// vpceEndpointPolicy proves a custom endpoint policy is a second check on
// requests through the endpoint only: it denies a listing the identity policy
// allows, leaves the direct path alone, and decides each key of a batch delete.
func vpceEndpointPolicy(t *testing.T, c *harness.AWSClient, epID, gate, bucket string, p harness.S3Principal,
	deleteObjects func(keys []string) (harness.GuestCurl, error), list func() (harness.GuestCurl, error), listed func(harness.GuestCurl) bool,
) {
	const dropKey, keepKey = "vpce-policy-drop", "vpce-policy-keep"
	direct, err := harness.PredastoreS3WithCreds(gate, p.AccessKeyID, p.SecretAccessKey)
	require.NoError(t, err, "s3 client for %s", p.UserName)
	for _, k := range []string{dropKey, keepKey} {
		_, err := direct.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader("endpoint policy e2e\n")})
		require.NoError(t, err, "put-object %s/%s", bucket, k)
		t.Cleanup(func() { _, _ = direct.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(k)}) })
	}

	doc, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []map[string]any{
		{"Effect": "Allow", "Principal": "*", "Action": "s3:*", "Resource": "*"},
		{"Effect": "Deny", "Principal": "*", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::" + bucket},
		{"Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::" + bucket + "/" + keepKey},
	}})
	require.NoError(t, err, "marshal endpoint policy")
	harness.Step(t, "modify-vpc-endpoint %s with a policy denying ListBucket on %s and DeleteObject on %s", epID, bucket, keepKey)
	_, err = c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(epID), PolicyDocument: aws.String(string(doc))})
	require.NoError(t, err, "modify-vpc-endpoint policy")
	reset := false
	resetPolicy := func() error {
		_, rerr := c.EC2.ModifyVpcEndpoint(&ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(epID), ResetPolicy: aws.Bool(true)})
		return rerr
	}
	t.Cleanup(func() {
		if !reset {
			if rerr := resetPolicy(); rerr != nil {
				t.Logf("cleanup: reset the policy on %s: %v", epID, rerr)
			}
		}
	})
	ep, live, err := harness.DescribeVpcEndpoint(c, epID)
	require.NoError(t, err, "describe-vpc-endpoints %s", epID)
	require.True(t, live, "%s is not described", epID)
	assert.JSONEq(t, string(doc), aws.StringValue(ep.PolicyDocument), "the stored policy")

	denied := func(g harness.GuestCurl) bool { return g.Code == "403" && strings.Contains(g.Body, "AccessDenied") }
	g, ms := harness.WaitGuestS3(t, vpceConvergeBudget, list, denied, "a listing denied by the endpoint policy")
	harness.Detail(t, "after", "endpoint_policy_deny", "probe", g.String(), "converge_ms", ms)
	_, err = direct.ListObjectsV2(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err, "the endpoint policy must not hold a request on the direct path")

	g, err = deleteObjects([]string{dropKey, keepKey})
	require.NoError(t, err, "guest delete-objects")
	require.Equalf(t, "200", g.Code, "a mixed batch answers 200 with per-key results; got %s: %s", g, g.Body)
	assert.Contains(t, g.Body, "<Deleted><Key>"+dropKey+"</Key>", "the allowed key is reported deleted")
	assert.Regexp(t, "<Error><Key>"+keepKey+"</Key><Code>AccessDenied</Code>", g.Body, "the denied key is reported in Errors")
	_, err = direct.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(keepKey)})
	require.NoError(t, err, "the denied key must still exist")
	_, err = direct.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(dropKey)})
	require.Error(t, err, "the allowed key must be gone")

	harness.Step(t, "modify-vpc-endpoint %s reset-policy", epID)
	require.NoError(t, resetPolicy(), "modify-vpc-endpoint reset-policy")
	reset = true
	g, ms = harness.WaitGuestS3(t, vpceConvergeBudget, list, listed, "a listing of the bucket")
	harness.Detail(t, "after", "endpoint_policy_reset", "probe", g.String(), "converge_ms", ms)
}

// vpceDeleteVpcFence proves DeleteVpc is refused while an endpoint is the
// VPC's only dependency, and that deleting a table the endpoint lists succeeds
// and drops it from the endpoint. The VPC deletes once the endpoint is gone.
func vpceDeleteVpcFence(t *testing.T, c *harness.AWSClient, svc string) {
	vpcID := harness.CreateVPC(t, c, vpceFenceVPCCIDR)
	t.Cleanup(func() { harness.DeleteVPCWhenFree(t, c, vpcID, 2*time.Minute) })
	mainRT := harness.MainRouteTableID(t, c, vpcID)
	rtOut, err := c.EC2.CreateRouteTable(&ec2.CreateRouteTableInput{VpcId: aws.String(vpcID)})
	require.NoError(t, err, "create-route-table")
	spareRT := aws.StringValue(rtOut.RouteTable.RouteTableId)
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(spareRT)})
	})

	ep := harness.CreateS3GatewayEndpoint(t, c, vpcID, svc, mainRT, spareRT)
	epID := aws.StringValue(ep.VpcEndpointId)
	harness.Detail(t, "vpc", vpcID, "main_rtb", mainRT, "spare_rtb", spareRT, "vpc_endpoint", epID)

	harness.Step(t, "delete-route-table %s while %s lists it", spareRT, epID)
	_, err = c.EC2.DeleteRouteTable(&ec2.DeleteRouteTableInput{RouteTableId: aws.String(spareRT)})
	require.NoError(t, err, "delete-route-table of a table the endpoint lists")
	harness.WaitVpcEndpointRouteTables(t, c, epID, mainRT)

	harness.Step(t, "delete-vpc %s with the endpoint as its only dependency", vpcID)
	_, err = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	harness.AssertAWSError(t, err, "DependencyViolation")

	require.NoError(t, harness.DeleteVpcEndpointWait(c, epID, 2*time.Minute), "delete the fence endpoint")
	harness.EventuallyErr(t, func() error {
		if _, derr := c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}); derr != nil {
			return fmt.Errorf("delete-vpc %s after the endpoint went: %w", vpcID, derr)
		}
		return nil
	}, 60*time.Second, 3*time.Second)
}

// vpceSecondaryENI attaches an interface in secSubnetID to the guest, routes
// its own address out of it, and requires it to reach the bastion but not the
// gate. Detaches and deletes the interface before returning.
func vpceSecondaryENI(t *testing.T, fix *Fixture, bastion harness.SSHTarget, instanceID, privIP, secSubnetID, sgID, bastionPriv, gate string, port int) {
	t.Helper()
	c := fix.AWS
	sub, err := c.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{SubnetIds: []*string{aws.String(secSubnetID)}})
	require.NoError(t, err, "describe-subnets %s", secSubnetID)
	require.Len(t, sub.Subnets, 1)
	cidr, err := netip.ParsePrefix(aws.StringValue(sub.Subnets[0].CidrBlock))
	require.NoError(t, err, "subnet CIDR")
	gw := cidr.Masked().Addr().Next()

	eniOut, err := c.EC2.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(secSubnetID), Groups: []*string{aws.String(sgID)},
		Description: aws.String("vpce-e2e-secondary"),
	})
	require.NoError(t, err, "create-network-interface")
	eniID := aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId)
	eniMAC := strings.ToLower(aws.StringValue(eniOut.NetworkInterface.MacAddress))
	eniIP := aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress)
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_, _ = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniID)})
		}
	})
	attOut, err := c.EC2.AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID), InstanceId: aws.String(instanceID), DeviceIndex: aws.Int64(1),
	})
	require.NoError(t, err, "attach-network-interface %s", eniID)
	attachID := aws.StringValue(attOut.AttachmentId)
	detached := false
	t.Cleanup(func() {
		if !detached {
			_, _ = c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{AttachmentId: aws.String(attachID)})
			_ = waitForENIStatusSoft(c, eniID, "available", 2*time.Minute)
		}
	})
	waitForENIAttached(t, c, eniID, instanceID)
	harness.Detail(t, "eni", eniID, "eni_ip", eniIP, "eni_mac", eniMAC, "gateway", gw.String())

	// Policy routing sends traffic from the secondary address out of its own
	// interface; the guest image configures only the primary at boot.
	cfg := fmt.Sprintf(`IF=; for d in /sys/class/net/*; do [ "$(cat "$d/address" 2>/dev/null)" = %[1]s ] && IF=${d##*/}; done; `+
		`[ -n "$IF" ] || { echo "eni-config missing"; exit 0; }; `+
		`sudo ip link set "$IF" up && sudo ip addr replace %[2]s/%[3]d dev "$IF" && `+
		`sudo ip route replace default via %[4]s dev "$IF" table 4242 && `+
		`{ sudo ip rule del from %[2]s lookup 4242 2>/dev/null; sudo ip rule add from %[2]s lookup 4242 priority 4242; } && `+
		`echo "eni-config ok $IF"`, harness.ShellQuote(eniMAC), eniIP, cidr.Bits(), gw)
	harness.Step(t, "configure the secondary interface in the guest")
	harness.EventuallyErr(t, func() error {
		out, gerr := harness.GuestExecViaBastion(bastion, privIP, cfg, 30*time.Second)
		if gerr != nil {
			return fmt.Errorf("%w: %s", gerr, strings.TrimSpace(out))
		}
		if !strings.Contains(out, "eni-config ok") {
			return fmt.Errorf("secondary interface not configured yet: %s", strings.TrimSpace(out))
		}
		return nil
	}, 90*time.Second, 3*time.Second)

	harness.Step(t, "control: tcp %s -> %s:22 over the secondary interface", eniIP, bastionPriv)
	harness.EventuallyErr(t, func() error {
		got, perr := vpceTCPProbe(bastion, privIP, eniIP, bastionPriv, 22)
		if perr != nil {
			return perr
		}
		if got != "connected" {
			return fmt.Errorf("secondary interface cannot reach the bastion: %s", got)
		}
		return nil
	}, 60*time.Second, 3*time.Second)

	harness.Step(t, "tcp %s -> %s:%d over the secondary interface", eniIP, gate, port)
	got, err := vpceTCPProbe(bastion, privIP, eniIP, gate, port)
	require.NoError(t, err, "probe the gate from the secondary interface")
	require.Equalf(t, "timeout", got,
		"a secondary interface must not be carried by the endpoint, so its connection to %s:%d times out", gate, port)

	_, err = c.EC2.DetachNetworkInterface(&ec2.DetachNetworkInterfaceInput{AttachmentId: aws.String(attachID)})
	require.NoError(t, err, "detach-network-interface %s", eniID)
	waitForENIStatus(t, c, eniID, "available")
	detached = true
	_, err = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniID)})
	require.NoError(t, err, "delete-network-interface %s", eniID)
	deleted = true
}

// vpceTCPProbeProgram opens one TCP connection from a bound source address and
// prints how it ended, so the outcome does not depend on what the server speaks.
const vpceTCPProbeProgram = `import socket,sys
s=socket.socket()
s.settimeout(float(sys.argv[3]))
s.bind((sys.argv[4],0))
try:
    s.connect((sys.argv[1],int(sys.argv[2])))
    print("tcp-probe connected")
except socket.timeout:
    print("tcp-probe timeout")
except ConnectionRefusedError:
    print("tcp-probe refused")
except OSError as e:
    print("tcp-probe error-%d" % e.errno)
`

// vpceTCPProbe connects from src to dst:port inside the guest and returns
// "connected", "timeout", "refused" or "error-<errno>".
func vpceTCPProbe(bastion harness.SSHTarget, privIP, src, dst string, port int) (string, error) {
	cmd := fmt.Sprintf("python3 -c %s %s %d 5 %s", harness.ShellQuote(vpceTCPProbeProgram), dst, port, src)
	out, err := harness.GuestExecViaBastion(bastion, privIP, cmd, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	_, rest, ok := strings.Cut(out, "tcp-probe ")
	if !ok {
		return "", fmt.Errorf("no tcp probe result in %q", out)
	}
	return strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]), nil
}

// vpceHold probes for window and fails on the first outcome ok rejects,
// returning how many probes held.
func vpceHold(t *testing.T, window time.Duration, probe func() (harness.GuestCurl, error), ok func(harness.GuestCurl) bool, what string) int {
	t.Helper()
	deadline := time.Now().Add(window)
	n := 0
	for time.Now().Before(deadline) {
		g, err := probe()
		require.NoError(t, err, "probe %d", n+1)
		require.Truef(t, ok(g), "probe %d after %dms: got %s, want %s", n+1, window.Milliseconds()-time.Until(deadline).Milliseconds(), g, what)
		n++
		time.Sleep(2 * time.Second)
	}
	return n
}

// vpceNodeAddresses is every address the formation's nodes are known by, as
// /32s, for a policy that allows only requests from a node.
func vpceNodeAddresses(env *harness.Env, entries []string) []string {
	var out []string
	add := func(ip string) {
		a, err := netip.ParseAddr(ip)
		if err != nil || !a.Is4() || a.IsLoopback() {
			return
		}
		if c := a.String() + "/32"; !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	for _, list := range [][]string{entries, env.NodeIPs, env.ServiceIPs, {env.WANHost}} {
		for _, ip := range list {
			add(ip)
		}
	}
	return out
}
