package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestServiceWithInstance creates an ELBv2 + VPC service with a VPC, subnet,
// and a simulated instance ENI (attached to an instance ID).
func setupTestServiceWithInstance(t *testing.T, instanceID, instanceIP string) (*ELBv2ServiceImpl, *handlers_ec2_vpc.VPCServiceImpl, string) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	elbv2Svc, err := NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	elbv2Svc.VPCService = vpcSvc

	// Create VPC + subnet
	vpcOut, err := vpcSvc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnetOut, err := vpcSvc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:            vpcOut.Vpc.VpcId,
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)

	// Create an ENI for the "instance" with specific IP
	eniOut, err := vpcSvc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:         subnetOut.Subnet.SubnetId,
		PrivateIpAddress: aws.String(instanceIP),
		Description:      aws.String("Primary ENI for " + instanceID),
	}, testAccountID)
	require.NoError(t, err)

	// Attach ENI to instance (simulates RunInstances)
	_, err = vpcSvc.AttachENI(testAccountID, *eniOut.NetworkInterface.NetworkInterfaceId, instanceID, 0)
	require.NoError(t, err)

	return elbv2Svc, vpcSvc, *subnetOut.Subnet.SubnetId
}

func TestRegisterTargets_ResolvesPrivateIP(t *testing.T) {
	svc, _, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	// Create target group
	tgOut, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("ip-resolve-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	// Register the instance
	_, err = svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-web001")}},
	}, testAccountID)
	require.NoError(t, err)

	// Verify private IP was resolved
	health, err := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 1)

	// The target should have been registered with the resolved IP in the store
	tg, err := svc.store.GetTargetGroupByArn(*tgArn)
	require.NoError(t, err)
	require.Len(t, tg.Targets, 1)
	assert.Equal(t, "10.0.1.50", tg.Targets[0].PrivateIP)
}

func TestRegisterTargets_UnresolvableIP(t *testing.T) {
	svc, _, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	tgOut, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("unresolvable-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	// Register an instance that doesn't exist — should still succeed with empty IP
	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-nonexistent")}},
	}, testAccountID)
	require.NoError(t, err)

	tg, _ := svc.store.GetTargetGroupByArn(*tgArn)
	require.Len(t, tg.Targets, 1)
	assert.Empty(t, tg.Targets[0].PrivateIP)
}

func TestRegisterTargets_WithoutVPCService(t *testing.T) {
	// When VPC service is nil, IP resolution is skipped gracefully
	svc := setupTestService(t)

	tgOut, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("no-vpc-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-any")}},
	}, testAccountID)
	require.NoError(t, err)

	tg, _ := svc.store.GetTargetGroupByArn(*tgArn)
	assert.Empty(t, tg.Targets[0].PrivateIP)
}

func TestRegisterTargets_MultipleInstances(t *testing.T) {
	svc, vpcSvc, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	// Create a second instance ENI
	subnets, _ := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	subnetID := subnets.Subnets[0].SubnetId

	eni2, _ := vpcSvc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:         subnetID,
		PrivateIpAddress: aws.String("10.0.1.51"),
	}, testAccountID)
	vpcSvc.AttachENI(testAccountID, *eni2.NetworkInterface.NetworkInterfaceId, "i-web002", 0)

	tgOut, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("multi-inst-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("i-web001")},
			{Id: aws.String("i-web002")},
		},
	}, testAccountID)
	require.NoError(t, err)

	tg, _ := svc.store.GetTargetGroupByArn(*tgArn)
	require.Len(t, tg.Targets, 2)
	assert.Equal(t, "10.0.1.50", tg.Targets[0].PrivateIP)
	assert.Equal(t, "10.0.1.51", tg.Targets[1].PrivateIP)
}

// TestResetTargetHealthOnStartup_TransitionsAllNonDrainingToInitial verifies that
// startup resets non-draining targets to "initial", preventing stale "healthy"
// claims before the lb-agent has posted a fresh report after a daemon restart.
func TestResetTargetHealthOnStartup_TransitionsAllNonDrainingToInitial(t *testing.T) {
	store := setupTestNATS(t)
	svc := &ELBv2ServiceImpl{store: store}

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:000:targetgroup/test/tg-reset",
		TargetGroupID:  "tg-reset",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-h", HealthState: TargetHealthHealthy, HealthDesc: "Target is healthy", PrivateIP: "10.0.0.1"},
			{Id: "i-u", HealthState: TargetHealthUnhealthy, HealthDesc: "Health check failed", PrivateIP: "10.0.0.2"},
			{Id: "i-d", HealthState: TargetHealthDraining, HealthDesc: "Draining", PrivateIP: "10.0.0.3"},
			{Id: "i-i", HealthState: TargetHealthInitial, PrivateIP: "10.0.0.4"},
		},
	}
	require.NoError(t, store.PutTargetGroup(tg))

	require.NoError(t, svc.ResetTargetHealthOnStartup(t.Context()))

	got, err := store.GetTargetGroup("tg-reset")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Targets, 4)

	byID := map[string]Target{}
	for _, t := range got.Targets {
		byID[t.Id] = t
	}
	assert.Equal(t, TargetHealthInitial, byID["i-h"].HealthState, "previously healthy must reset")
	assert.Equal(t, TargetHealthInitial, byID["i-u"].HealthState, "previously unhealthy must reset")
	assert.Equal(t, TargetHealthDraining, byID["i-d"].HealthState, "draining must be preserved")
	assert.Equal(t, TargetHealthInitial, byID["i-i"].HealthState, "initial stays initial")
}

func TestResetTargetHealthOnStartup_NilSafe(t *testing.T) {
	var svc *ELBv2ServiceImpl
	assert.NoError(t, svc.ResetTargetHealthOnStartup(t.Context()))

	svc2 := &ELBv2ServiceImpl{}
	assert.NoError(t, svc2.ResetTargetHealthOnStartup(t.Context()))
}

// A target group not forwarded to by any listener serves no traffic, so its
// targets report "unused" (Target.NotInUse) instead of stalling in "initial".
func TestDescribeTargetHealth_UnusedWhenNoListener(t *testing.T) {
	svc, _, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	tgOut, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("detached-tg"), Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	_, err = svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-web001")}},
	}, testAccountID)
	require.NoError(t, err)

	health, err := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{TargetGroupArn: tgArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 1)
	th := health.TargetHealthDescriptions[0].TargetHealth
	assert.Equal(t, TargetHealthUnused, aws.StringValue(th.State))
	assert.Equal(t, "Target.NotInUse", aws.StringValue(th.Reason))
}

// Once a listener forwards to the target group it is in use, so DescribeTargetHealth
// reports the live health state ("initial" until a health report lands), not "unused".
func TestDescribeTargetHealth_InUseWhenListenerForwards(t *testing.T) {
	svc, _, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	tgOut, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("attached-tg"), Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("attached-lb")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"), Port: aws.Int64(80),
		DefaultActions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-web001")}},
	}, testAccountID)
	require.NoError(t, err)

	health, err := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{TargetGroupArn: tgArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 1)
	assert.Equal(t, TargetHealthInitial, aws.StringValue(health.TargetHealthDescriptions[0].TargetHealth.State))
}
