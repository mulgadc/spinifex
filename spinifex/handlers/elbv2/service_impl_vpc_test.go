package handlers_elbv2

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestServiceWithVPC creates an ELBv2 service wired to a real VPC service
// with a pre-created VPC and subnet for ENI allocation testing.
func setupTestServiceWithVPC(t *testing.T) (*ELBv2ServiceImpl, *handlers_ec2_vpc.VPCServiceImpl) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	// Create VPC service
	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	// Create ELBv2 service with VPC wired in.
	// Use DevNetworking=true so single-subnet tests aren't blocked by multi-AZ validation.
	cfg := &config.Config{Daemon: config.DaemonConfig{DevNetworking: true}}
	elbv2Svc, err := NewELBv2ServiceImplWithNATS(cfg, nc)
	require.NoError(t, err)
	elbv2Svc.VPCService = vpcSvc

	// Create a VPC and subnet for tests
	vpcOut, err := vpcSvc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = vpcSvc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:            vpcOut.Vpc.VpcId,
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)

	return elbv2Svc, vpcSvc
}

// getTestSubnetID creates a fresh subnet and returns its ID.
func getTestSubnetID(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl, vpcID, cidr, az string) string {
	t.Helper()
	out, err := vpcSvc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String(cidr),
		AvailabilityZone: aws.String(az),
	}, testAccountID)
	require.NoError(t, err)
	return *out.Subnet.SubnetId
}

func TestCreateLoadBalancer_CreatesENIs(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	// Find the subnet we created
	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, subnets.Subnets)
	subnetID := *subnets.Subnets[0].SubnetId

	// Create ALB with subnet
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("eni-test-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	lb := out.LoadBalancers[0]

	// Verify VpcId was populated from subnet
	assert.NotEmpty(t, *lb.VpcId)

	// Verify AZ info was populated
	require.Len(t, lb.AvailabilityZones, 1)
	assert.Equal(t, "us-east-1a", *lb.AvailabilityZones[0].ZoneName)
	assert.Equal(t, subnetID, *lb.AvailabilityZones[0].SubnetId)

	// Verify ENI was created
	eniDesc, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, eniDesc.NetworkInterfaces, 1)

	eni := eniDesc.NetworkInterfaces[0]
	assert.Contains(t, *eni.Description, "ELB app/eni-test-alb/")
	assert.True(t, *eni.RequesterManaged)
	assert.Equal(t, subnetID, *eni.SubnetId)
	assert.NotEmpty(t, *eni.PrivateIpAddress)

	// Verify ENI has the managed-by tag
	foundTag := false
	for _, tag := range eni.TagSet {
		if *tag.Key == "spinifex:managed-by" && *tag.Value == "elbv2" {
			foundTag = true
		}
	}
	assert.True(t, foundTag, "ENI should have spinifex:managed-by=elbv2 tag")
}

// TestCreateLoadBalancer_SubnetMappings_CreatesENIs guards the LBC path: the AWS
// Load Balancer Controller supplies ALB subnets via SubnetMappings, not the plain
// Subnets list. Ignoring SubnetMappings skips ENI allocation, so the data-plane VM
// never launches and the ALB is created but cannot serve traffic.
func TestCreateLoadBalancer_SubnetMappings_CreatesENIs(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, subnets.Subnets)
	subnetID := *subnets.Subnets[0].SubnetId

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("subnetmapping-alb"),
		SubnetMappings: []*elbv2.SubnetMapping{
			{SubnetId: aws.String(subnetID)},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	lb := out.LoadBalancers[0]

	assert.NotEmpty(t, *lb.VpcId, "VpcId must be resolved from the SubnetMappings subnet")
	require.Len(t, lb.AvailabilityZones, 1)
	assert.Equal(t, subnetID, *lb.AvailabilityZones[0].SubnetId)

	eniDesc, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, eniDesc.NetworkInterfaces, 1, "an ENI must be created for the SubnetMappings subnet")
	assert.Equal(t, subnetID, *eniDesc.NetworkInterfaces[0].SubnetId)
}

func TestCreateLoadBalancer_MultipleSubnets(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	// Get VPC ID
	vpcs, _ := vpcSvc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	vpcID := *vpcs.Vpcs[0].VpcId

	// Create two subnets in different AZs
	sub1 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.2.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.3.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("multi-subnet-alb"),
		Subnets: []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)

	lb := out.LoadBalancers[0]
	assert.Len(t, lb.AvailabilityZones, 2)

	// Verify 2 ENIs created (+ 1 from setupTestServiceWithVPC's initial subnet)
	eniDesc, _ := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	managedCount := 0
	for _, eni := range eniDesc.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			managedCount++
		}
	}
	assert.Equal(t, 2, managedCount)
}

// TestCreateLoadBalancer_MultiSubnet_AllENIsPassedToLauncher verifies that
// every subnet's ENI is threaded through to SystemInstanceInput, not just
// the first one.
func TestCreateLoadBalancer_MultiSubnet_AllENIsPassedToLauncher(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	vpcs, _ := vpcSvc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	vpcID := *vpcs.Vpcs[0].VpcId

	sub1 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.10.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.11.0/24", "us-east-1b")
	sub3 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.12.0/24", "us-east-1c")

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-multi-alb",
			PrivateIP:  "10.0.10.4",
			PublicIP:   "203.0.113.200",
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("multi-eni-alb"),
		Subnets: []*string{aws.String(sub1), aws.String(sub2), aws.String(sub3)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	require.Len(t, mock.launchCalls, 1)
	launchInput := mock.launchCalls[0]

	// Primary ENI is the first subnet's ENI.
	assert.NotEmpty(t, launchInput.ENIID, "primary ENIID must be populated")
	assert.NotEmpty(t, launchInput.ENIMac, "primary ENIMac must be populated")
	assert.NotEmpty(t, launchInput.ENIIP, "primary ENIIP must be populated")
	assert.Equal(t, sub1, launchInput.SubnetID)

	// Two extra ENIs, one per remaining subnet, each with MAC/IP resolved.
	require.Len(t, launchInput.ExtraENIs, 2, "launcher must receive one ExtraENI per additional subnet")
	extraSubnets := map[string]bool{}
	for _, extra := range launchInput.ExtraENIs {
		assert.NotEmpty(t, extra.ENIID, "extra ENIID must be populated")
		assert.NotEmpty(t, extra.ENIMac, "extra ENIMac must be populated")
		assert.NotEmpty(t, extra.ENIIP, "extra ENIIP must be populated")
		assert.NotEmpty(t, extra.SubnetID, "extra SubnetID must be populated")
		extraSubnets[extra.SubnetID] = true
	}
	assert.True(t, extraSubnets[sub2], "extras should include sub2")
	assert.True(t, extraSubnets[sub3], "extras should include sub3")
}

func TestDeleteLoadBalancer_CleansUpENIs(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, _ := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	subnetID := *subnets.Subnets[0].SubnetId

	// Create and then delete ALB
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("cleanup-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	// Verify ENI exists
	eniDesc, _ := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	assert.Len(t, eniDesc.NetworkInterfaces, 1)

	// Delete ALB
	_, err = svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	// Verify ENI was cleaned up
	eniDesc, _ = vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	assert.Empty(t, eniDesc.NetworkInterfaces)
}

func TestCreateLoadBalancer_InvalidSubnet(t *testing.T) {
	svc, _ := setupTestServiceWithVPC(t)

	_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("bad-subnet-alb"),
		Subnets: []*string{aws.String("subnet-nonexistent")},
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "SubnetNotFound")
}

func TestCreateLoadBalancer_RollbackOnPartialFailure(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, _ := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	validSubnet := *subnets.Subnets[0].SubnetId

	// First subnet valid, second invalid — should rollback the first ENI
	_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("rollback-alb"),
		Subnets: []*string{aws.String(validSubnet), aws.String("subnet-bogus")},
	}, testAccountID)
	assert.Error(t, err)

	// Verify no orphaned ENIs remain
	eniDesc, _ := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	assert.Empty(t, eniDesc.NetworkInterfaces)
}

func TestCreateLoadBalancer_WithoutVPCService(t *testing.T) {
	// When vpcService is nil (e.g. in pure unit tests), ENI creation is skipped
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("no-vpc-alb"),
		Subnets: []*string{aws.String("subnet-xxx")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.LoadBalancers[0].VpcId)
	assert.Empty(t, out.LoadBalancers[0].AvailabilityZones)
}

// --- Scheme networking integration tests ---

func TestCreateLoadBalancer_InternetFacing_AllocatesPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-pub",
			PrivateIP:  "10.0.1.5",
			PublicIP:   "203.0.113.50",
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("inet-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "internet-facing", *out.LoadBalancers[0].Scheme)

	svc.WaitLaunches()

	// Verify launcher was called with internet-facing scheme
	require.Len(t, mock.launchCalls, 1)
	assert.Equal(t, SchemeInternetFacing, mock.launchCalls[0].Scheme)

	// The public IP is attached by the asynchronous launch, so it surfaces on a
	// describe after the boot completes rather than on the create response (which
	// returns while the LB is still provisioning — AWS parity).
	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{out.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	lb := desc.LoadBalancers[0]

	// Internet-facing ALB: AZ includes both the public IP and the ENI's private IP.
	require.Len(t, lb.AvailabilityZones, 1)
	require.Len(t, lb.AvailabilityZones[0].LoadBalancerAddresses, 2)
	var gotPublic, gotPrivate bool
	for _, addr := range lb.AvailabilityZones[0].LoadBalancerAddresses {
		if aws.StringValue(addr.IpAddress) == "203.0.113.50" {
			gotPublic = true
		}
		if aws.StringValue(addr.PrivateIPv4Address) != "" {
			gotPrivate = true
		}
	}
	assert.True(t, gotPublic, "expected public IpAddress in LoadBalancerAddresses")
	assert.True(t, gotPrivate, "expected ENI PrivateIPv4Address in LoadBalancerAddresses")
}

func TestCreateLoadBalancer_Internal_NoPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-priv",
			PrivateIP:  "10.0.1.6",
			// No PublicIP — internal scheme
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("internal-only"),
		Scheme:  aws.String("internal"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	lb := out.LoadBalancers[0]
	assert.Equal(t, "internal", *lb.Scheme)
	// Internal ALB: no public IpAddress, but the ENI's private IP should be
	// exposed in LoadBalancerAddresses[].PrivateIPv4Address.
	require.Len(t, lb.AvailabilityZones, 1)
	require.Len(t, lb.AvailabilityZones[0].LoadBalancerAddresses, 1)
	assert.Nil(t, lb.AvailabilityZones[0].LoadBalancerAddresses[0].IpAddress)
	assert.NotEmpty(t, aws.StringValue(lb.AvailabilityZones[0].LoadBalancerAddresses[0].PrivateIPv4Address))

	// Verify launcher was called with internal scheme
	svc.WaitLaunches()
	require.Len(t, mock.launchCalls, 1)
	assert.Equal(t, SchemeInternal, mock.launchCalls[0].Scheme)
}

func TestCreateLoadBalancer_NLB_Internal_NoPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-nlb-priv",
			PrivateIP:  "10.0.1.10",
			// No PublicIP — internal scheme
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("nlb-internal"),
		Type:    aws.String("network"),
		Scheme:  aws.String("internal"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	lb := out.LoadBalancers[0]
	assert.Equal(t, "internal", *lb.Scheme)
	assert.Equal(t, "network", *lb.Type)
	assert.Contains(t, *lb.DNSName, "internal-")
	assert.Contains(t, *lb.LoadBalancerArn, "loadbalancer/net/nlb-internal")

	// Verify launcher was called with internal scheme
	svc.WaitLaunches()
	require.Len(t, mock.launchCalls, 1)
	assert.Equal(t, SchemeInternal, mock.launchCalls[0].Scheme)

	// Internal NLB: no public IpAddress, but the ENI's private IP is exposed.
	require.Len(t, lb.AvailabilityZones, 1)
	require.Len(t, lb.AvailabilityZones[0].LoadBalancerAddresses, 1)
	assert.Nil(t, lb.AvailabilityZones[0].LoadBalancerAddresses[0].IpAddress)
	assert.NotEmpty(t, aws.StringValue(lb.AvailabilityZones[0].LoadBalancerAddresses[0].PrivateIPv4Address))

	// Verify no security groups (NLBs don't support them)
	assert.Empty(t, lb.SecurityGroups)
}

func TestDeleteLoadBalancer_TerminatesVM_WithPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-del",
			PrivateIP:  "10.0.1.7",
			PublicIP:   "203.0.113.51",
		},
		terminateDone: make(chan struct{}),
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("del-pub-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	// Verify ENI exists before delete
	eniDesc, _ := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	assert.Len(t, eniDesc.NetworkInterfaces, 1)

	// Delete ALB
	_, err = svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	// Wait for async VM termination goroutine to complete
	mock.waitTerminate()
	mock.mu.Lock()
	assert.Len(t, mock.terminateCalls, 1)
	assert.Equal(t, "i-alb-del", mock.terminateCalls[0])
	mock.mu.Unlock()

	// Verify ENIs were cleaned up (detach + delete happens in DeleteLoadBalancer)
	eniDesc, _ = vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	assert.Empty(t, eniDesc.NetworkInterfaces)
}

func TestDeleteLoadBalancer_ReapsFloatingIPNAT(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-nat",
			PrivateIP:  "10.0.1.9",
			PublicIP:   "203.0.113.60",
		},
		terminateDone: make(chan struct{}),
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	// Capture the vpc.delete-nat event the teardown must publish.
	type natEvent struct {
		VpcId      string `json:"vpc_id"`
		ExternalIP string `json:"external_ip"`
		LogicalIP  string `json:"logical_ip"`
	}
	gotNAT := make(chan natEvent, 4)
	sub, err := svc.nc.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
		var evt natEvent
		if json.Unmarshal(msg.Data, &evt) == nil {
			gotNAT <- evt
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("nat-reap-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	_, err = svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	select {
	case evt := <-gotNAT:
		assert.Equal(t, "203.0.113.60", evt.ExternalIP, "floating IP reaped as external_ip")
		assert.Equal(t, "10.0.1.9", evt.LogicalIP, "LB VM VPC IP reaped as logical_ip")
		assert.NotEmpty(t, evt.VpcId)
	case <-time.After(3 * time.Second):
		t.Fatal("DeleteLoadBalancer did not publish vpc.delete-nat for the LB floating IP")
	}
}

func TestDescribeLoadBalancers_InternetFacing_IncludesPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-desc",
			PrivateIP:  "10.0.1.8",
			PublicIP:   "203.0.113.52",
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("desc-pub-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	// Describe and verify public IP is in the response
	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String("desc-pub-alb")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)

	lb := desc.LoadBalancers[0]
	require.Len(t, lb.AvailabilityZones, 1)
	// Internet-facing ALB: both the ENI's private IP and the allocated public
	// IP should appear in LoadBalancerAddresses.
	require.Len(t, lb.AvailabilityZones[0].LoadBalancerAddresses, 2)
	var gotPublic, gotPrivate bool
	for _, addr := range lb.AvailabilityZones[0].LoadBalancerAddresses {
		if aws.StringValue(addr.IpAddress) == "203.0.113.52" {
			gotPublic = true
		}
		if aws.StringValue(addr.PrivateIPv4Address) != "" {
			gotPrivate = true
		}
	}
	assert.True(t, gotPublic, "expected public IpAddress in LoadBalancerAddresses")
	assert.True(t, gotPrivate, "expected ENI PrivateIPv4Address in LoadBalancerAddresses")
}

func TestDescribeLoadBalancers_Internal_NoPublicIP(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-alb-int",
			PrivateIP:  "10.0.1.9",
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("desc-int-alb"),
		Scheme:  aws.String("internal"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()

	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String("desc-int-alb")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)

	lb := desc.LoadBalancers[0]
	require.Len(t, lb.AvailabilityZones, 1)
	// Internal ALB: private IP of the ENI is exposed via PrivateIPv4Address.
	require.Len(t, lb.AvailabilityZones[0].LoadBalancerAddresses, 1)
	assert.Nil(t, lb.AvailabilityZones[0].LoadBalancerAddresses[0].IpAddress)
	assert.NotEmpty(t, aws.StringValue(lb.AvailabilityZones[0].LoadBalancerAddresses[0].PrivateIPv4Address))
	// Verify DNS has internal prefix
	assert.Contains(t, *lb.DNSName, "internal-desc-int-alb")
}

func TestCreateLoadBalancer_LaunchFailure_SetsStateFailed(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchErr: assert.AnError,
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("fail-launch-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	// The create returns provisioning; the launch failure lands on the record
	// once the background boot attempt fails.
	assert.Equal(t, StateProvisioning, *out.LoadBalancers[0].State.Code)

	svc.WaitLaunches()
	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{out.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, StateFailed, *desc.LoadBalancers[0].State.Code)
}

func TestCreateLoadBalancer_MissingCredentials_SetsStateFailed(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-should-not-launch",
			PrivateIP:  "10.0.1.99",
		},
	}
	svc.InstanceLauncher = mock
	// Deliberately NOT setting credentials

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("no-creds-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, StateProvisioning, *out.LoadBalancers[0].State.Code)

	svc.WaitLaunches()
	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{out.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, StateFailed, *desc.LoadBalancers[0].State.Code)
	// Verify launcher was never called (missing creds short-circuit the boot)
	assert.Empty(t, mock.launchCalls)
}

func TestENI_RequesterManagedFlag(t *testing.T) {
	_, vpcSvc := setupTestServiceWithVPC(t)

	subnets, _ := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	subnetID := *subnets.Subnets[0].SubnetId

	// Create a normal ENI (not managed)
	normalENI, err := vpcSvc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String("user ENI"),
	}, testAccountID)
	require.NoError(t, err)

	// Create a managed ENI (like ALB would)
	managedENI, err := vpcSvc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String("ELB app/test/lb123"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("network-interface"),
				Tags: []*ec2.Tag{
					{Key: aws.String("spinifex:managed-by"), Value: aws.String("elbv2")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Describe all ENIs
	desc, _ := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.Len(t, desc.NetworkInterfaces, 2)

	for _, eni := range desc.NetworkInterfaces {
		if *eni.NetworkInterfaceId == *normalENI.NetworkInterface.NetworkInterfaceId {
			assert.False(t, *eni.RequesterManaged, "normal ENI should not be RequesterManaged")
		}
		if *eni.NetworkInterfaceId == *managedENI.NetworkInterface.NetworkInterfaceId {
			assert.True(t, *eni.RequesterManaged, "managed ENI should be RequesterManaged")
		}
	}
}

// TestCreateLoadBalancer_AttachesSpecifiedSecurityGroupsToENI verifies caller-supplied
// SecurityGroups are applied to the ALB ENI; without this the OVN port-group misses
// the user's allow rules and the default-deny ACL drops all inbound traffic.
func TestCreateLoadBalancer_AttachesSpecifiedSecurityGroupsToENI(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, subnets.Subnets)
	subnetID := *subnets.Subnets[0].SubnetId
	vpcID := *subnets.Subnets[0].VpcId

	sgOut, err := vpcSvc.CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("alb-sg"),
		Description: aws.String("ALB ingress"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	sgID := *sgOut.GroupId

	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:           aws.String("sg-attach-alb"),
		Subnets:        []*string{aws.String(subnetID)},
		SecurityGroups: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)

	eniDesc, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)

	var albENI *ec2.NetworkInterface
	for _, eni := range eniDesc.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			albENI = eni
			break
		}
	}
	require.NotNil(t, albENI, "ELB-managed ENI must be created")

	require.Len(t, albENI.Groups, 1, "ALB ENI must join exactly the specified SG, not the default SG")
	assert.Equal(t, sgID, *albENI.Groups[0].GroupId)
}

// TestCreateLoadBalancer_NoSecurityGroupsFallsBackToDefault preserves the
// AWS-semantics fallback: an ALB created without SecurityGroups joins the
// VPC's default SG (so the default-deny ACL has at least one allow source).
func TestCreateLoadBalancer_NoSecurityGroupsFallsBackToDefault(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId
	vpcID := *subnets.Subnets[0].VpcId

	defaultSGID, err := vpcSvc.FindDefaultSGForVPC(testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, defaultSGID, "VPC must have a default SG provisioned")

	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("no-sg-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	eniDesc, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)

	var albENI *ec2.NetworkInterface
	for _, eni := range eniDesc.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			albENI = eni
			break
		}
	}
	require.NotNil(t, albENI)
	require.Len(t, albENI.Groups, 1)
	assert.Equal(t, defaultSGID, *albENI.Groups[0].GroupId)
}

// TestRebuildSystemInstanceInput_HappyPath verifies a recovering ALB VM gets the
// same SystemInstanceInput as the original launch; a mismatch causes writeFwCfgBlobs
// to emit a different netcfg/cacert blob than the persisted QEMU command line.
func TestRebuildSystemInstanceInput_HappyPath(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-recover-1",
			PrivateIP:  "10.0.1.42",
		},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"
	svc.CACert = "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"

	_, err = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("recover-alb"),
		Subnets: []*string{aws.String(subnetID)},
		Scheme:  aws.String("internal"),
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	require.Len(t, mock.launchCalls, 1)
	originalInput := mock.launchCalls[0]

	ctx := RecoveryContext{
		InstanceID:   "i-recover-1",
		InstanceType: originalInput.InstanceType,
		ENIMac:       originalInput.ENIMac,
		MgmtMAC:      "02:a0:00:11:22:33",
		MgmtIP:       "172.31.0.7",
	}

	rebuilt, err := svc.RebuildSystemInstanceInput(ctx)
	require.NoError(t, err)
	require.NotNil(t, rebuilt)

	assert.Equal(t, originalInput.InstanceType, rebuilt.InstanceType)
	assert.Equal(t, originalInput.SubnetID, rebuilt.SubnetID)
	assert.Equal(t, originalInput.ENIID, rebuilt.ENIID)
	assert.Equal(t, originalInput.ENIMac, rebuilt.ENIMac)
	// ENIIP comes from lb.VPCIP (set by the launcher's PrivateIP return value);
	// the test mock returns a synthetic value, so assert against the mock's IP.
	assert.Equal(t, "10.0.1.42", rebuilt.ENIIP)
	assert.Equal(t, originalInput.Scheme, rebuilt.Scheme)
	assert.Equal(t, originalInput.AccountID, rebuilt.AccountID)
	assert.Equal(t, originalInput.LBAgentEnv, rebuilt.LBAgentEnv)
	assert.Equal(t, originalInput.CACert, rebuilt.CACert)

	require.GreaterOrEqual(t, len(rebuilt.NICs), 2)
	assert.True(t, rebuilt.NICs[0].IsDefault)
	assert.Equal(t, "02:a0:00:11:22:33", rebuilt.NICs[1].MAC, "mgmt NIC MAC must come from RecoveryContext")
	assert.Equal(t, "172.31.0.7/24", rebuilt.NICs[1].CIDR, "mgmt NIC CIDR must come from RecoveryContext")
}

// TestRebuildSystemInstanceInput_NoLBRecord verifies the recovery_failed path when
// the LB record is gone from KV but the VM record survives; recovery must not
// silently boot the VM into an unowned state.
func TestRebuildSystemInstanceInput_NoLBRecord(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.RebuildSystemInstanceInput(RecoveryContext{
		InstanceID:   "i-ghost",
		InstanceType: "sys.micro",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no LB record references instance i-ghost")
}

// TestRebuildSystemInstanceInput_MultiENI verifies extras survive the rebuild for
// multi-subnet ALBs; recovery must produce the same ExtraENIInput shape so the
// daemon wires the same taps and NICs the persisted QEMU args reference.
func TestRebuildSystemInstanceInput_MultiENI(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	vpcs, _ := vpcSvc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	vpcID := *vpcs.Vpcs[0].VpcId
	sub1 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.20.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vpcID, "10.0.21.0/24", "us-east-1b")

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-recover-multi", PrivateIP: "10.0.20.4"},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("recover-multi-alb"),
		Subnets: []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	require.Len(t, mock.launchCalls, 1)
	originalInput := mock.launchCalls[0]

	rebuilt, err := svc.RebuildSystemInstanceInput(RecoveryContext{
		InstanceID:   "i-recover-multi",
		InstanceType: originalInput.InstanceType,
		ENIMac:       originalInput.ENIMac,
	})
	require.NoError(t, err)
	require.Len(t, rebuilt.ExtraENIs, len(originalInput.ExtraENIs))

	originalByENI := map[string]ExtraENIInput{}
	for _, e := range originalInput.ExtraENIs {
		originalByENI[e.ENIID] = e
	}
	for _, e := range rebuilt.ExtraENIs {
		orig, ok := originalByENI[e.ENIID]
		require.True(t, ok, "extra ENI %s not in original launch", e.ENIID)
		assert.Equal(t, orig.SubnetID, e.SubnetID)
		assert.Equal(t, orig.ENIMac, e.ENIMac)
		assert.Equal(t, orig.ENIIP, e.ENIIP)
	}
}

func TestCreateClusterNLBSync_CarriesCrossAccountENI(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)

	subnets, err := vpcSvc.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnets.Subnets[0].SubnetId

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-cluster-nlb", PrivateIP: "10.0.1.9"},
	}
	svc.InstanceLauncher = mock
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"

	// The Set A ENI lives in a different (customer) account than the LB's own
	// system-account ENIs; it arrives fully populated as the caller created it.
	extra := ExtraENIInput{
		ENIID:     "eni-seta-001",
		ENIMac:    "02:0a:01:23:45:67",
		ENIIP:     "10.20.0.5",
		SubnetID:  "subnet-seta",
		AccountID: "999988887777",
	}

	_, err = svc.CreateClusterNLBSync(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("eks-alpha"),
		Type:    aws.String("network"),
		Scheme:  aws.String("internal"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID, []ExtraENIInput{extra})
	require.NoError(t, err)

	require.Len(t, mock.launchCalls, 1)
	launch := mock.launchCalls[0]

	// The cross-account ENI must be threaded onto the LB VM with its own account.
	require.Len(t, launch.ExtraENIs, 1)
	assert.Equal(t, "eni-seta-001", launch.ExtraENIs[0].ENIID)
	assert.Equal(t, "999988887777", launch.ExtraENIs[0].AccountID)

	// NIC[0] primary, NIC[1] mgmt, NIC[2] the Set A extra NIC.
	require.Len(t, launch.NICs, 3)
	assert.Equal(t, "02:0a:01:23:45:67", launch.NICs[2].MAC)

	// And it is persisted for host-reboot recovery.
	record, err := svc.store.GetLoadBalancerByName("eks-alpha", testAccountID)
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Len(t, record.CrossAccountENIs, 1)
	assert.Equal(t, "eni-seta-001", record.CrossAccountENIs[0].ENIID)
}
