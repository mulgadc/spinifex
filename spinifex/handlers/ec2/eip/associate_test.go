//test:in-package — these tests drive the package's own setupTestEIP fixture and
//read svc.natsConn to assert the NAT messages AssociateAddress publishes.

package handlers_ec2_eip

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAssocENI builds a VPC, a subnet and an ENI in it, returning the ENI as
// the VPC service describes it.
func createAssocENI(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) *ec2.NetworkInterface {
	t.Helper()
	vpc, err := vpcSvc.CreateVpc(t.Context(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnet, err := vpcSvc.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId:     vpc.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)

	eni, err := vpcSvc.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: subnet.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)

	return eni.NetworkInterface
}

func allocateEIP(t *testing.T, svc *EIPServiceImpl) (allocID, publicIP string) {
	t.Helper()
	out, err := svc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	return *out.AllocationId, *out.PublicIp
}

func describeEIP(t *testing.T, svc *EIPServiceImpl, allocID string) *ec2.Address {
	t.Helper()
	desc, err := svc.DescribeAddresses(t.Context(), &ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Addresses, 1)
	return desc.Addresses[0]
}

// Associating by ENI id must copy the interface's own addressing onto the EIP
// record. A field lost here leaves the record pointing at the right ENI with
// the wrong private IP, and the NAT rule sends the traffic elsewhere.
func TestEIP_AssociateByNetworkInterfaceID(t *testing.T) {
	svc, _, vpcSvc := setupTestEIP(t)
	eni := createAssocENI(t, vpcSvc)
	allocID, publicIP := allocateEIP(t, svc)

	sub, err := svc.natsConn.SubscribeSync("vpc.add-nat")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId:       aws.String(allocID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.AssociationId)
	assert.Contains(t, *out.AssociationId, "eipassoc-")

	addr := describeEIP(t, svc, allocID)
	assert.Equal(t, *out.AssociationId, *addr.AssociationId)
	assert.Equal(t, *eni.NetworkInterfaceId, *addr.NetworkInterfaceId)
	assert.Equal(t, *eni.PrivateIpAddress, *addr.PrivateIpAddress)

	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var got natEvent
	require.NoError(t, json.Unmarshal(msg.Data, &got))
	assert.Equal(t, publicIP, got.ExternalIP)
	assert.Equal(t, *eni.PrivateIpAddress, got.LogicalIP)
	assert.Equal(t, *eni.MacAddress, got.MAC,
		"the MAC must come from the looked-up ENI, or the NAT rule ARPs for the wrong host")
}

// An explicit PrivateIpAddress overrides the ENI's primary address; the rest of
// the ENI's identity still has to come from the lookup.
func TestEIP_AssociateByNetworkInterfaceID_PrivateIPOverride(t *testing.T) {
	svc, _, vpcSvc := setupTestEIP(t)
	eni := createAssocENI(t, vpcSvc)
	allocID, _ := allocateEIP(t, svc)

	_, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId:       aws.String(allocID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
		PrivateIpAddress:   aws.String("10.0.1.99"),
	}, testAccountID)
	require.NoError(t, err)

	addr := describeEIP(t, svc, allocID)
	assert.Equal(t, "10.0.1.99", *addr.PrivateIpAddress)
	assert.Equal(t, *eni.NetworkInterfaceId, *addr.NetworkInterfaceId)
}

func TestEIP_AssociateByNetworkInterfaceID_NotFound(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	allocID, _ := allocateEIP(t, svc)

	_, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId:       aws.String(allocID),
		NetworkInterfaceId: aws.String("eni-doesnotexist"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidNetworkInterfaceIDNotFound, err.Error())

	// The allocation must survive a failed association.
	addr := describeEIP(t, svc, allocID)
	assert.Nil(t, addr.AssociationId)
}

// Associating by instance id resolves the instance's primary ENI, which is the
// path RunInstances-then-AssociateAddress takes.
func TestEIP_AssociateByInstanceID(t *testing.T) {
	svc, _, vpcSvc := setupTestEIP(t)
	eni := createAssocENI(t, vpcSvc)
	const instanceID = "i-0123456789abcdef0"
	_, err := vpcSvc.AttachENI(testAccountID, *eni.NetworkInterfaceId, instanceID, 0)
	require.NoError(t, err)

	allocID, publicIP := allocateEIP(t, svc)

	out, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId: aws.String(allocID),
		InstanceId:   aws.String(instanceID),
	}, testAccountID)
	require.NoError(t, err)

	addr := describeEIP(t, svc, allocID)
	assert.Equal(t, *out.AssociationId, *addr.AssociationId)
	assert.Equal(t, *eni.NetworkInterfaceId, *addr.NetworkInterfaceId,
		"the instance's primary ENI must be resolved from the attachment")
	assert.Equal(t, instanceID, *addr.InstanceId)
	assert.Equal(t, *eni.PrivateIpAddress, *addr.PrivateIpAddress)

	// The daemon re-announces dnat_and_snat off this lookup on relaunch.
	ip, ok := svc.AssociatedPublicIPForInstance(context.Background(), testAccountID, instanceID)
	assert.True(t, ok)
	assert.Equal(t, publicIP, ip)
}

func TestEIP_AssociateByInstanceID_NoENI(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	allocID, _ := allocateEIP(t, svc)

	_, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId: aws.String(allocID),
		InstanceId:   aws.String("i-nointerface"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// An allocated-but-unassociated EIP is not a public IP for any instance, and
// the empty instance id never matches a record.
func TestEIP_AssociatedPublicIPForInstance_Misses(t *testing.T) {
	svc, _, _ := setupTestEIP(t)
	allocateEIP(t, svc)

	ip, ok := svc.AssociatedPublicIPForInstance(context.Background(), testAccountID, "i-unknown")
	assert.False(t, ok)
	assert.Empty(t, ip)

	ip, ok = svc.AssociatedPublicIPForInstance(context.Background(), testAccountID, "")
	assert.False(t, ok)
	assert.Empty(t, ip)
}

// Disassociation withdraws the NAT rule with the MAC lookupENI resolves, then
// returns the record to "allocated" without giving the address back.
func TestEIP_DisassociateAfterAssociate(t *testing.T) {
	svc, _, vpcSvc := setupTestEIP(t)
	eni := createAssocENI(t, vpcSvc)
	allocID, publicIP := allocateEIP(t, svc)

	assoc, err := svc.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
		AllocationId:       aws.String(allocID),
		NetworkInterfaceId: eni.NetworkInterfaceId,
	}, testAccountID)
	require.NoError(t, err)

	sub, err := svc.natsConn.SubscribeSync("vpc.delete-nat")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{
		AssociationId: assoc.AssociationId,
	}, testAccountID)
	require.NoError(t, err)

	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var got natEvent
	require.NoError(t, json.Unmarshal(msg.Data, &got))
	assert.Equal(t, publicIP, got.ExternalIP)
	assert.Equal(t, *eni.MacAddress, got.MAC)

	addr := describeEIP(t, svc, allocID)
	assert.Nil(t, addr.AssociationId)
	assert.Equal(t, publicIP, *addr.PublicIp)
}
