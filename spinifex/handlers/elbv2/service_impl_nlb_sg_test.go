package handlers_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstSubnet returns the ID + VPC ID of the subnet setupTestServiceWithVPC
// pre-creates.
func firstSubnet(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) (subnetID, vpcID string) {
	t.Helper()
	subnets, err := vpcSvc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotEmpty(t, subnets.Subnets)
	return *subnets.Subnets[0].SubnetId, *subnets.Subnets[0].VpcId
}

// describeSG returns the security group record by ID.
func describeSG(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl, sgID string) *ec2.SecurityGroup {
	t.Helper()
	out, err := vpcSvc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: aws.StringSlice([]string{sgID}),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroups, 1)
	return out.SecurityGroups[0]
}

// sgHasRule reports whether the SG has an ingress rule for the proto/port/cidr.
func sgHasRule(sg *ec2.SecurityGroup, proto string, port int64, cidr string) bool {
	for _, p := range sg.IpPermissions {
		if aws.StringValue(p.IpProtocol) != proto {
			continue
		}
		if aws.Int64Value(p.FromPort) != port || aws.Int64Value(p.ToPort) != port {
			continue
		}
		for _, r := range p.IpRanges {
			if aws.StringValue(r.CidrIp) == cidr {
				return true
			}
		}
	}
	return false
}

func managedENI(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) *ec2.NetworkInterface {
	t.Helper()
	out, err := vpcSvc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	for _, eni := range out.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			return eni
		}
	}
	return nil
}

func createNLB(t *testing.T, svc *ELBv2ServiceImpl, name, scheme, subnetID string) *elbv2.LoadBalancer {
	t.Helper()
	in := &elbv2.CreateLoadBalancerInput{
		Name:    aws.String(name),
		Type:    aws.String("network"),
		Subnets: []*string{aws.String(subnetID)},
	}
	if scheme != "" {
		in.Scheme = aws.String(scheme)
	}
	out, err := svc.CreateLoadBalancer(in, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	return out.LoadBalancers[0]
}

func createTCPListener(t *testing.T, svc *ELBv2ServiceImpl, lbArn *string, port int64) {
	t.Helper()
	tg, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-nlb-sg"),
		Protocol: aws.String(elbv2.ProtocolEnumTcp),
		Port:     aws.Int64(port),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbArn,
		Protocol:        aws.String(elbv2.ProtocolEnumTcp),
		Port:            aws.Int64(port),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)
}

// TestCreateNLB_MintsManagedSGAttachedToENI verifies a network LB gets a
// dedicated internal SG (not the VPC default) attached to its ENI, recorded in
// NLBManagedSGID, while DescribeLoadBalancers still reports no customer SGs.
func TestCreateNLB_MintsManagedSGAttachedToENI(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, vpcID := firstSubnet(t, vpcSvc)

	defaultSGID, err := vpcSvc.FindDefaultSGForVPC(testAccountID, vpcID)
	require.NoError(t, err)

	lb := createNLB(t, svc, "nlb-sg", "internal", subnetID)
	assert.Empty(t, lb.SecurityGroups, "NLB must not surface the managed SG as a customer SG")

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	require.NotEmpty(t, rec.NLBManagedSGID, "NLB record must carry the managed SG id")
	assert.NotEqual(t, defaultSGID, rec.NLBManagedSGID, "managed SG must be distinct from the VPC default SG")

	eni := managedENI(t, vpcSvc)
	require.NotNil(t, eni)
	require.Len(t, eni.Groups, 1, "NLB ENI must join exactly the managed SG, not the default SG")
	assert.Equal(t, rec.NLBManagedSGID, *eni.Groups[0].GroupId)
}

// TestLBENIGroups_Precedence checks the ENI-group selector: an NLB prefers its
// customer SGs over the managed SG, falls back to the managed SG without them, and
// an ALB always uses its customer SGs.
func TestLBENIGroups_Precedence(t *testing.T) {
	assert.Equal(t, []string{"sg-a"}, lbENIGroups(&LoadBalancerRecord{
		Type: LoadBalancerTypeNetwork, SecurityGroups: []string{"sg-a"}, NLBManagedSGID: "sg-mgd",
	}))
	assert.Equal(t, []string{"sg-mgd"}, lbENIGroups(&LoadBalancerRecord{
		Type: LoadBalancerTypeNetwork, NLBManagedSGID: "sg-mgd",
	}))
	assert.Equal(t, []string{"sg-x"}, lbENIGroups(&LoadBalancerRecord{
		Type: LoadBalancerTypeApplication, SecurityGroups: []string{"sg-x"},
	}))
}

// TestCreateNLB_WithCustomerSGs_AttachesThemNoManagedSG verifies an NLB created
// with customer SGs joins those SGs on its ENI and skips the managed SG entirely;
// the caller then owns the listener-port ingress rules.
func TestCreateNLB_WithCustomerSGs_AttachesThemNoManagedSG(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, vpcID := firstSubnet(t, vpcSvc)

	sgOut, err := vpcSvc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("nlb-customer-sg"),
		Description: aws.String("NLB ingress"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	sgID := *sgOut.GroupId

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("nlb-customer-sg"),
		Type:           aws.String("network"),
		Subnets:        []*string{aws.String(subnetID)},
		SecurityGroups: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.store.GetLoadBalancerByArn(*out.LoadBalancers[0].LoadBalancerArn)
	require.NoError(t, err)
	assert.Equal(t, []string{sgID}, rec.SecurityGroups)
	assert.Empty(t, rec.NLBManagedSGID, "NLB with customer SGs must not mint a managed SG")

	eni := managedENI(t, vpcSvc)
	require.NotNil(t, eni)
	require.Len(t, eni.Groups, 1, "NLB ENI must join exactly the customer SG")
	assert.Equal(t, sgID, *eni.Groups[0].GroupId)
}

// TestCreateALB_NoManagedSG verifies the managed-SG behavior is NLB-only: an ALB
// keeps the existing customer-SG / default-SG semantics.
func TestCreateALB_NoManagedSG(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("alb-no-sg"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.store.GetLoadBalancerByArn(*out.LoadBalancers[0].LoadBalancerArn)
	require.NoError(t, err)
	assert.Empty(t, rec.NLBManagedSGID, "ALB must not mint a managed NLB SG")
}

// TestCreateListener_Internal_OpensPortFromVPCCIDR verifies an internal NLB's
// listener port is authorized on the managed SG from the VPC CIDR.
func TestCreateListener_Internal_OpensPortFromVPCCIDR(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-int", "internal", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sg := describeSG(t, vpcSvc, rec.NLBManagedSGID)
	assert.True(t, sgHasRule(sg, "tcp", 443, "10.0.0.0/16"),
		"internal NLB listener :443 must be open from the VPC CIDR")
	assert.False(t, sgHasRule(sg, "tcp", 443, "0.0.0.0/0"))
}

// TestCreateListener_InternetFacing_OpensPortFromAnywhere verifies an
// internet-facing NLB's listener port is authorized from 0.0.0.0/0.
func TestCreateListener_InternetFacing_OpensPortFromAnywhere(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-pub", "internet-facing", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sg := describeSG(t, vpcSvc, rec.NLBManagedSGID)
	assert.True(t, sgHasRule(sg, "tcp", 443, "0.0.0.0/0"),
		"internet-facing NLB listener :443 must be open from 0.0.0.0/0")
}

// TestCreateListener_TCPUDP_OpensBothProtocols verifies a TCP_UDP listener opens
// both tcp and udp on the listener port.
func TestCreateListener_TCPUDP_OpensBothProtocols(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-tcpudp", "internet-facing", subnetID)
	tg, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-tcpudp"),
		Protocol: aws.String(elbv2.ProtocolEnumTcpUdp),
		Port:     aws.Int64(53),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        aws.String(elbv2.ProtocolEnumTcpUdp),
		Port:            aws.Int64(53),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sg := describeSG(t, vpcSvc, rec.NLBManagedSGID)
	assert.True(t, sgHasRule(sg, "tcp", 53, "0.0.0.0/0"))
	assert.True(t, sgHasRule(sg, "udp", 53, "0.0.0.0/0"))
}

// TestSetLoadBalancerIngressCIDRs_RewritesListenerRules verifies the override API
// swaps the scheme-default rule for the supplied CIDRs across existing listeners
// and is idempotent on re-invocation.
func TestSetLoadBalancerIngressCIDRs_RewritesListenerRules(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-cidr", "internet-facing", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sgID := rec.NLBManagedSGID

	// Default opened 0.0.0.0/0.
	assert.True(t, sgHasRule(describeSG(t, vpcSvc, sgID), "tcp", 443, "0.0.0.0/0"))

	require.NoError(t, svc.SetLoadBalancerIngressCIDRs(*lb.LoadBalancerArn, []string{"203.0.113.0/24"}, testAccountID))

	sg := describeSG(t, vpcSvc, sgID)
	assert.True(t, sgHasRule(sg, "tcp", 443, "203.0.113.0/24"), "new CIDR must be authorized")
	assert.False(t, sgHasRule(sg, "tcp", 443, "0.0.0.0/0"), "old default rule must be revoked")

	// Idempotent re-apply.
	require.NoError(t, svc.SetLoadBalancerIngressCIDRs(*lb.LoadBalancerArn, []string{"203.0.113.0/24"}, testAccountID))
	sg = describeSG(t, vpcSvc, sgID)
	assert.True(t, sgHasRule(sg, "tcp", 443, "203.0.113.0/24"))

	// Persisted override drives the resolved CIDRs.
	rec, err = svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	assert.Equal(t, []string{"203.0.113.0/24"}, rec.NLBIngressCIDRs)
}

func TestSetLoadBalancerIngressCIDRs_RejectsBadInput(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-badcidr", "internet-facing", subnetID)
	err := svc.SetLoadBalancerIngressCIDRs(*lb.LoadBalancerArn, []string{"not-a-cidr"}, testAccountID)
	require.Error(t, err)

	// ALB is rejected — managed SG is NLB-only.
	albOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("alb-reject"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	err = svc.SetLoadBalancerIngressCIDRs(*albOut.LoadBalancers[0].LoadBalancerArn, []string{"10.0.0.0/8"}, testAccountID)
	require.Error(t, err)
}

// TestDeleteListener_RevokesPort verifies removing a listener closes its port on
// the managed SG.
func TestDeleteListener_RevokesPort(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-del-lst", "internet-facing", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	lst, err := svc.DescribeListeners(&elbv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, lst.Listeners, 1)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	require.True(t, sgHasRule(describeSG(t, vpcSvc, rec.NLBManagedSGID), "tcp", 443, "0.0.0.0/0"))

	_, err = svc.DeleteListener(&elbv2.DeleteListenerInput{ListenerArn: lst.Listeners[0].ListenerArn}, testAccountID)
	require.NoError(t, err)

	assert.False(t, sgHasRule(describeSG(t, vpcSvc, rec.NLBManagedSGID), "tcp", 443, "0.0.0.0/0"),
		"deleting the listener must close its port")
}

// TestDeleteNLB_DeletesManagedSG verifies the managed SG is removed on LB
// teardown.
func TestDeleteNLB_DeletesManagedSG(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-del", "internal", subnetID)
	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sgID := rec.NLBManagedSGID
	require.NotEmpty(t, sgID)

	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)

	out, err := vpcSvc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: aws.StringSlice([]string{sgID}),
	}, testAccountID)
	// Deleted SG: either an empty result or a not-found error is acceptable.
	if err == nil {
		assert.Empty(t, out.SecurityGroups, "managed SG must be deleted with the NLB")
	}
}
