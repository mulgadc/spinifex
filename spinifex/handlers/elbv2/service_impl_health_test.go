package handlers_elbv2

import (
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
	vpcOut, err := vpcSvc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnetOut, err := vpcSvc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:            vpcOut.Vpc.VpcId,
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)

	// Create an ENI for the "instance" with specific IP
	eniOut, err := vpcSvc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
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
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("ip-resolve-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	// Register the instance
	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-web001")}},
	}, testAccountID)
	require.NoError(t, err)

	// Verify private IP was resolved
	health, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
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

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("unresolvable-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	// Register an instance that doesn't exist — should still succeed with empty IP
	_, err := svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-nonexistent")}},
	}, testAccountID)
	require.NoError(t, err)

	tg, _ := svc.store.GetTargetGroupByArn(*tgArn)
	require.Len(t, tg.Targets, 1)
	assert.Equal(t, "", tg.Targets[0].PrivateIP)
}

func TestRegisterTargets_WithoutVPCService(t *testing.T) {
	// When VPC service is nil, IP resolution is skipped gracefully
	svc := setupTestService(t)

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("no-vpc-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	_, err := svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-any")}},
	}, testAccountID)
	require.NoError(t, err)

	tg, _ := svc.store.GetTargetGroupByArn(*tgArn)
	assert.Equal(t, "", tg.Targets[0].PrivateIP)
}

func TestRegisterTargets_MultipleInstances(t *testing.T) {
	svc, vpcSvc, _ := setupTestServiceWithInstance(t, "i-web001", "10.0.1.50")

	// Create a second instance ENI
	subnets, _ := vpcSvc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	subnetID := subnets.Subnets[0].SubnetId

	eni2, _ := vpcSvc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:         subnetID,
		PrivateIpAddress: aws.String("10.0.1.51"),
	}, testAccountID)
	vpcSvc.AttachENI(testAccountID, *eni2.NetworkInterface.NetworkInterfaceId, "i-web002", 0)

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("multi-inst-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	_, err := svc.RegisterTargets(&elbv2.RegisterTargetsInput{
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
