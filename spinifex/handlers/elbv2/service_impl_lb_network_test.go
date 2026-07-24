package handlers_elbv2

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createLBArn(t *testing.T, svc *ELBv2ServiceImpl, name string) string {
	t.Helper()
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String(name),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	return *out.LoadBalancers[0].LoadBalancerArn
}

func describeLB(t *testing.T, svc *ELBv2ServiceImpl, arn string) *elbv2.LoadBalancer {
	t.Helper()
	out, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{aws.String(arn)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	return out.LoadBalancers[0]
}

func TestSetIpAddressType_IPv4Idempotent(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-lb")

	out, err := svc.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
		IpAddressType:   aws.String("ipv4"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ipv4", *out.IpAddressType)
	assert.Equal(t, "ipv4", *describeLB(t, svc, arn).IpAddressType)
}

func TestSetIpAddressType_DualstackRejected(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-ds")

	_, err := svc.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
		IpAddressType:   aws.String("dualstack"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestSetIpAddressType_MissingParams(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-mp")

	_, err := svc.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	_, err = svc.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
		IpAddressType: aws.String("ipv4"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestSetIpAddressType_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef"),
		IpAddressType:   aws.String("ipv4"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestSetSecurityGroups_UpdatesRecord(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-lb")

	out, err := svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa", "sg-bbb"}),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"sg-aaa", "sg-bbb"}, aws.StringValueSlice(out.SecurityGroupIds))
	assert.Equal(t, []string{"sg-aaa", "sg-bbb"}, aws.StringValueSlice(describeLB(t, svc, arn).SecurityGroups))
}

func TestSetSecurityGroups_EmptyRejected(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-empty")

	_, err := svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestSetSecurityGroups_NLBRejected(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("sg-nlb"),
		Type: aws.String("network"),
	}, testAccountID)
	require.NoError(t, err)
	arn := *out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa"}),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestSetSecurityGroups_NLBWithSGs_Replaces(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:           aws.String("sg-nlb-repl"),
		Type:           aws.String("network"),
		SecurityGroups: aws.StringSlice([]string{"sg-orig"}),
	}, testAccountID)
	require.NoError(t, err)
	arn := *out.LoadBalancers[0].LoadBalancerArn

	res, err := svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-new1", "sg-new2"}),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"sg-new1", "sg-new2"}, aws.StringValueSlice(res.SecurityGroupIds))

	rec, err := svc.store.GetLoadBalancerByArn(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, []string{"sg-new1", "sg-new2"}, rec.SecurityGroups)
}

func TestSetSecurityGroups_TooManyRejected(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-too-many")

	_, err := svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-1", "sg-2", "sg-3", "sg-4", "sg-5", "sg-6"}),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestSetSecurityGroups_CrossAccount(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-xacct")

	_, err := svc.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa"}),
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

// frontendPublicIP returns the first non-empty public IpAddress across the LB's
// AZ addresses, or "" when none is set yet.
func frontendPublicIP(lb *elbv2.LoadBalancer) string {
	for _, az := range lb.AvailabilityZones {
		for _, addr := range az.LoadBalancerAddresses {
			if ip := aws.StringValue(addr.IpAddress); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func TestCreateLoadBalancerSync_ReturnsFrontendIP(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	mock.launchResult.PublicIP = "203.0.113.9"
	vid := vpcID(t, vpcSvc)
	sub := getTestSubnetID(t, vpcSvc, vid, "10.0.30.0/24", "us-east-1a")

	out, err := svc.CreateLoadBalancerSync(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("sync-lb"),
		Type:    aws.String("network"),
		Scheme:  aws.String(elbv2.LoadBalancerSchemeEnumInternetFacing),
		Subnets: []*string{aws.String(sub)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	// Sync create drove the launch inline — no WaitLaunches needed — so the
	// returned LB already carries its front-end IP.
	require.Len(t, mock.launchCalls, 1)
	assert.Equal(t, "203.0.113.9", frontendPublicIP(out.LoadBalancers[0]))
}

func TestCreateLoadBalancer_AsyncDefersFrontendIP(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	mock.launchResult.PublicIP = "203.0.113.9"
	vid := vpcID(t, vpcSvc)
	sub := getTestSubnetID(t, vpcSvc, vid, "10.0.31.0/24", "us-east-1a")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("async-lb"),
		Type:    aws.String("network"),
		Scheme:  aws.String(elbv2.LoadBalancerSchemeEnumInternetFacing),
		Subnets: []*string{aws.String(sub)},
	}, testAccountID)
	require.NoError(t, err)
	// Async create returns provisioning before the launch goroutine runs — the
	// front-end IP is not yet known (this is the race EKS must not read).
	assert.Equal(t, StateProvisioning, aws.StringValue(out.LoadBalancers[0].State.Code))
	assert.Empty(t, frontendPublicIP(out.LoadBalancers[0]))
	svc.WaitLaunches()
	// Once the launch settles, the IP is on the persisted record.
	assert.Equal(t, "203.0.113.9", frontendPublicIP(describeLB(t, svc, *out.LoadBalancers[0].LoadBalancerArn)))
}

// setupSubnetTestService wires a VPC-backed ELBv2 service with a launcher mock
// so SetSubnets can exercise ENI create/delete plus the LB-VM relaunch.
func setupSubnetTestService(t *testing.T) (*ELBv2ServiceImpl, *handlers_ec2_vpc.VPCServiceImpl, *mockSystemInstanceLauncher) {
	t.Helper()
	svc, vpcSvc := setupTestServiceWithVPC(t)
	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-lb", PrivateIP: "10.0.1.5"},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"
	return svc, vpcSvc, mock
}

func countManagedENIs(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) int {
	t.Helper()
	out, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	n := 0
	for _, eni := range out.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			n++
		}
	}
	return n
}

func vpcID(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) string {
	t.Helper()
	vpcs, err := vpcSvc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, vpcs.Vpcs)
	return *vpcs.Vpcs[0].VpcId
}

func TestSetSubnets_AddSubnet(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.20.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.21.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("add-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn
	require.Equal(t, 1, countManagedENIs(t, vpcSvc))
	require.Len(t, mock.launchCalls, 1)

	res, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, res.AvailabilityZones, 2)

	// New ENI created in sub2; LB VM relaunched on the new set.
	assert.Equal(t, 2, countManagedENIs(t, vpcSvc))
	assert.Len(t, mock.terminateCalls, 1)
	require.Len(t, mock.launchCalls, 2)
	assert.Len(t, mock.launchCalls[1].ExtraENIs, 1, "relaunch must carry the added subnet as an ExtraENI")

	lb := describeLB(t, svc, arn)
	assert.ElementsMatch(t, []string{sub1, sub2}, subnetIDsOfLB(lb))
}

func TestSetSubnets_RemoveSubnet(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.22.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.23.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("rm-lb"),
		Subnets: []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn
	require.Equal(t, 2, countManagedENIs(t, vpcSvc))

	res, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, res.AvailabilityZones, 1)

	// sub2's ENI is deleted; only sub1's remains.
	assert.Equal(t, 1, countManagedENIs(t, vpcSvc))
	assert.Len(t, mock.terminateCalls, 1)
	lb := describeLB(t, svc, arn)
	assert.Equal(t, []string{sub1}, subnetIDsOfLB(lb))
}

func TestSetSubnets_Replace(t *testing.T) {
	svc, vpcSvc, _ := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.24.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.25.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("swap-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)

	// One ENI swapped for another — still a single managed ENI, now in sub2.
	assert.Equal(t, 1, countManagedENIs(t, vpcSvc))
	lb := describeLB(t, svc, arn)
	assert.Equal(t, []string{sub2}, subnetIDsOfLB(lb))
}

func TestSetSubnets_TerminateFailureRollsBackNewENIs(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.26.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.27.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("rollback-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn
	require.Equal(t, 1, countManagedENIs(t, vpcSvc))

	// Terminate fails mid-relaunch (e.g. owning daemon unreachable). The ENI
	// created for the added subnet must be rolled back, not leaked.
	mock.terminateErr = errors.New("no responders available")

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.Error(t, err)

	// No leak: still the single original ENI; LB record unchanged; no relaunch.
	assert.Equal(t, 1, countManagedENIs(t, vpcSvc))
	assert.Len(t, mock.launchCalls, 1)
	lb := describeLB(t, svc, arn)
	assert.Equal(t, []string{sub1}, subnetIDsOfLB(lb))
}

func TestSetSubnets_Idempotent(t *testing.T) {
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.26.0/24", "us-east-1a")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("idem-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)

	// No change: no relaunch, no terminate, ENI count unchanged.
	assert.Equal(t, 1, countManagedENIs(t, vpcSvc))
	assert.Empty(t, mock.terminateCalls)
	assert.Len(t, mock.launchCalls, 1)
}

func TestSetSubnets_SubnetMappings(t *testing.T) {
	svc, vpcSvc, _ := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.27.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.28.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("mapping-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := *out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		SubnetMappings: []*elbv2.SubnetMapping{
			{SubnetId: aws.String(sub1)},
			{SubnetId: aws.String(sub2)},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, 2, countManagedENIs(t, vpcSvc))
}

func TestSetSubnets_WithoutVPCService(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "novpc-subnets")

	res, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String("subnet-aaa"), aws.String("subnet-bbb")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, res.AvailabilityZones, 2)
	assert.ElementsMatch(t, []string{"subnet-aaa", "subnet-bbb"}, subnetIDsOfLB(describeLB(t, svc, arn)))
}

func TestSetSubnets_MissingParams(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "subnets-mp")

	_, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		Subnets: []*string{aws.String("subnet-aaa")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestSetSubnets_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef"),
		Subnets:         []*string{aws.String("subnet-aaa")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestSetSubnets_CrossAccount(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "subnets-xacct")

	_, err := svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String("subnet-aaa")},
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func subnetIDsOfLB(lb *elbv2.LoadBalancer) []string {
	out := make([]string, 0, len(lb.AvailabilityZones))
	for _, az := range lb.AvailabilityZones {
		out = append(out, aws.StringValue(az.SubnetId))
	}
	return out
}
