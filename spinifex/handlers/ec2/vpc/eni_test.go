package handlers_ec2_vpc

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestENI(t *testing.T, svc *VPCServiceImpl, subnetId string) string {
	t.Helper()
	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	return *out.NetworkInterface.NetworkInterfaceId
}

func TestCreateNetworkInterface(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.NetworkInterface)

	eni := out.NetworkInterface
	assert.Equal(t, "eni-", (*eni.NetworkInterfaceId)[:4])
	assert.Equal(t, subnetId, *eni.SubnetId)
	assert.Equal(t, vpcId, *eni.VpcId)
	assert.Equal(t, "available", *eni.Status)
	assert.Equal(t, "10.0.1.4", *eni.PrivateIpAddress)
	assert.NotEmpty(t, *eni.MacAddress)
}

func TestCreateNetworkInterface_SequentialIPs(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	out1, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.4", *out1.NetworkInterface.PrivateIpAddress)

	out2, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "10.0.1.5", *out2.NetworkInterface.PrivateIpAddress)
}

func TestCreateNetworkInterface_MissingSubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateNetworkInterface_InvalidSubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String("subnet-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidSubnetID.NotFound")
}

func TestCreateNetworkInterface_WithTags(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetId),
		Description: aws.String("test eni"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("network-interface"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-eni")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "test eni", *out.NetworkInterface.Description)
	require.Len(t, out.NetworkInterface.TagSet, 1)
	assert.Equal(t, "Name", *out.NetworkInterface.TagSet[0].Key)
	assert.Equal(t, "my-eni", *out.NetworkInterface.TagSet[0].Value)
}

func TestDeleteNetworkInterface(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	_, err := svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniId),
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	_, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestDeleteNetworkInterface_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String("eni-nonexistent"),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestDeleteNetworkInterface_InUse(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	// Attach the ENI
	_, err := svc.AttachENI(testAccountID, eniId, "i-test123", 0)
	require.NoError(t, err)

	// Try to delete — should fail
	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniId),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterface.InUse")
}

func TestDeleteNetworkInterface_ReleasesIP(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	// Create and delete an ENI
	out1, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	ip1 := *out1.NetworkInterface.PrivateIpAddress

	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: out1.NetworkInterface.NetworkInterfaceId,
	}, testAccountID)
	require.NoError(t, err)

	// Create another ENI — should reuse the released IP
	out2, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, ip1, *out2.NetworkInterface.PrivateIpAddress)
}

func TestDescribeNetworkInterfaces_All(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 2)
}

func TestDescribeNetworkInterfaces_ByID(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eniId := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId) // second ENI

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eniId, *out.NetworkInterfaces[0].NetworkInterfaceId)
}

func TestDescribeNetworkInterfaces_FilterBySubnet(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetA := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	subnetB := createTestSubnet(t, svc, vpcId, "10.0.2.0/24")

	createTestENI(t, svc, subnetA)
	createTestENI(t, svc, subnetB)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("subnet-id"), Values: []*string{aws.String(subnetA)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, subnetA, *out.NetworkInterfaces[0].SubnetId)
}

func TestDescribeNetworkInterfaces_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String("eni-nonexistent")},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestAttachENI(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	attachId, err := svc.AttachENI(testAccountID, eniId, "i-test123", 0)
	require.NoError(t, err)
	assert.Contains(t, attachId, "eni-attach-")

	// Verify status changed
	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	require.NoError(t, err)
	eni := out.NetworkInterfaces[0]
	assert.Equal(t, "in-use", *eni.Status)
	assert.NotNil(t, eni.Attachment)
	assert.Equal(t, "i-test123", *eni.Attachment.InstanceId)
	assert.Equal(t, int64(0), *eni.Attachment.DeviceIndex)
}

func TestAttachENI_AlreadyAttached(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	_, err := svc.AttachENI(testAccountID, eniId, "i-test123", 0)
	require.NoError(t, err)

	// Second attach should fail
	_, err = svc.AttachENI(testAccountID, eniId, "i-test456", 1)
	assert.ErrorContains(t, err, "InvalidNetworkInterface.InUse")
}

func TestDetachENI(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	_, err := svc.AttachENI(testAccountID, eniId, "i-test123", 0)
	require.NoError(t, err)

	err = svc.DetachENI(testAccountID, eniId)
	require.NoError(t, err)

	// Verify status changed back
	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "available", *out.NetworkInterfaces[0].Status)
	assert.Nil(t, out.NetworkInterfaces[0].Attachment)
}

func TestGenerateENIMac(t *testing.T) {
	mac := generateENIMac("eni-test123")
	hw, err := net.ParseMAC(mac)
	require.NoError(t, err)
	assert.Equal(t, byte(0x02), hw[0]&0x03)

	// Same input produces same MAC
	assert.Equal(t, mac, generateENIMac("eni-test123"))

	// Different input produces different MAC
	assert.NotEqual(t, mac, generateENIMac("eni-test456"))
}

// --- Filter tests ---

func TestDescribeNetworkInterfaces_FilterByVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "10.1.0.0/16")
	subnet1 := createTestSubnet(t, svc, vpc1, "10.0.1.0/24")
	subnet2 := createTestSubnet(t, svc, vpc2, "10.1.1.0/24")

	createTestENI(t, svc, subnet1)
	createTestENI(t, svc, subnet2)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, vpc1, *out.NetworkInterfaces[0].VpcId)
}

func TestDescribeNetworkInterfaces_FilterByAttachmentInstanceId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eni1 := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId) // second ENI, not attached

	// Attach first ENI to an instance
	_, err := svc.AttachENI(testAccountID, eni1, "i-attached", 0)
	require.NoError(t, err)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []*string{aws.String("i-attached")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eni1, *out.NetworkInterfaces[0].NetworkInterfaceId)
}

func TestDescribeNetworkInterfaces_FilterByDescription(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	// Create two ENIs with different descriptions
	out1, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetId),
		Description: aws.String("ELB app/my-alb/lb-123"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetId),
		Description: aws.String("regular ENI"),
	}, testAccountID)
	require.NoError(t, err)

	// Filter by description should return only the ALB ENI
	desc, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("description"), Values: []*string{aws.String("ELB app/my-alb/lb-123")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.NetworkInterfaces, 1)
	assert.Equal(t, *out1.NetworkInterface.NetworkInterfaceId, *desc.NetworkInterfaces[0].NetworkInterfaceId)
}

func TestCreateNetworkInterface_IPExhaustion(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	// /28 subnet: 16 IPs total, 4 reserved at start + 1 broadcast = 11 usable
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.0.0/28")

	// Allocate all 11 available IPs
	for i := range 11 {
		_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
			SubnetId: aws.String(subnetId),
		}, testAccountID)
		require.NoError(t, err, "ENI %d should succeed", i)
	}

	// 12th allocation should fail — subnet exhausted
	_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	assert.Error(t, err)
}

// --- NATS event tests ---

func TestCreateNetworkInterface_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.create-port", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	eniId := *out.NetworkInterface.NetworkInterfaceId

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), eniId)
		assert.Contains(t, string(msg.Data), subnetId)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create-port event")
	}
}

func TestDeleteNetworkInterface_PublishesEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.delete-port", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniId),
	}, testAccountID)
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), eniId)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.delete-port event")
	}
}

func TestModifyNetworkInterfaceAttribute_SecurityGroups(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)
	sg1 := createTestSG(t, svc, vpcId, "sg-one")
	sg2 := createTestSG(t, svc, vpcId, "sg-two")

	_, err := svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
		Groups:             []*string{aws.String(sg1), aws.String(sg2)},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.NetworkInterfaces, 1)
	require.Len(t, desc.NetworkInterfaces[0].Groups, 2)
	assert.Equal(t, sg1, *desc.NetworkInterfaces[0].Groups[0].GroupId)
	assert.Equal(t, sg2, *desc.NetworkInterfaces[0].Groups[1].GroupId)
}

func TestModifyNetworkInterfaceAttribute_PublishesUpdatePortSGs(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)
	sg1 := createTestSG(t, svc, vpcId, "sg-mod-1")
	sg2 := createTestSG(t, svc, vpcId, "sg-mod-2")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.update-port-sgs", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
		Groups:             []*string{aws.String(sg1), aws.String(sg2)},
	}, testAccountID)
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), eniId)
		assert.Contains(t, string(msg.Data), sg1)
		assert.Contains(t, string(msg.Data), sg2)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.update-port-sgs event")
	}
}

func TestModifyNetworkInterfaceAttribute_DescriptionOnly_NoSGEvent(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.update-port-sgs", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
		Description:        &ec2.AttributeValue{Value: aws.String("only desc")},
	}, testAccountID)
	require.NoError(t, err)

	select {
	case <-eventCh:
		t.Fatal("unexpected vpc.update-port-sgs event for description-only modify")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestCreateNetworkInterface_PublishesEventCarriesSGs(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	sgA := createTestSG(t, svc, vpcId, "sg-evt-A")

	eventCh := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("vpc.create-port", func(msg *nats.Msg) {
		eventCh <- msg
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
		Groups:   []*string{aws.String(sgA)},
	}, testAccountID)
	require.NoError(t, err)

	select {
	case msg := <-eventCh:
		assert.Contains(t, string(msg.Data), sgA)
		assert.Contains(t, string(msg.Data), `"security_group_ids"`)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for vpc.create-port event")
	}
}

func TestModifyNetworkInterfaceAttribute_Description(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	_, err := svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
		Description:        &ec2.AttributeValue{Value: aws.String("updated description")},
	}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniId)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.NetworkInterfaces, 1)
	assert.Equal(t, "updated description", *desc.NetworkInterfaces[0].Description)
}

func TestModifyNetworkInterfaceAttribute_NoAttributes(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	_, err := svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidParameterValue")
}

func TestModifyNetworkInterfaceAttribute_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String("eni-nonexistent"),
		Groups:             []*string{aws.String("sg-111")},
	}, testAccountID)
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestModifyNetworkInterfaceAttribute_MissingID(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		Groups: []*string{aws.String("sg-111")},
	}, testAccountID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestCreateNetworkInterface_WithSecurityGroups(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	sgA := createTestSG(t, svc, vpcId, "sg-aaa")
	sgB := createTestSG(t, svc, vpcId, "sg-bbb")

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
		Groups:   []*string{aws.String(sgA), aws.String(sgB)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterface.Groups, 2)
	assert.Equal(t, sgA, *out.NetworkInterface.Groups[0].GroupId)
	assert.Equal(t, sgB, *out.NetworkInterface.Groups[1].GroupId)
}

func TestCreateNetworkInterface_FallsBackToDefaultSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	defaultSG := findDefaultSGInVPC(t, svc, vpcId)

	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterface.Groups, 1)
	assert.Equal(t, defaultSG, *out.NetworkInterface.Groups[0].GroupId)
}

func TestDescribeNetworkInterfaces_FilterByNetworkInterfaceId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eniId := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("network-interface-id"), Values: []*string{aws.String(eniId)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eniId, *out.NetworkInterfaces[0].NetworkInterfaceId)

	// Wildcard
	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("network-interface-id"), Values: []*string{aws.String("eni-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 2)
}

func TestDescribeNetworkInterfaces_FilterByStatus(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eni1 := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId) // stays available

	_, err := svc.AttachENI(testAccountID, eni1, "i-test", 0)
	require.NoError(t, err)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("in-use")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eni1, *out.NetworkInterfaces[0].NetworkInterfaceId)

	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)
}

func TestDescribeNetworkInterfaces_FilterByPrivateIpAddress(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	createTestENI(t, svc, subnetId) // 10.0.1.4
	createTestENI(t, svc, subnetId) // 10.0.1.5

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("private-ip-address"), Values: []*string{aws.String("10.0.1.4")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, "10.0.1.4", *out.NetworkInterfaces[0].PrivateIpAddress)

	// Wildcard
	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("private-ip-address"), Values: []*string{aws.String("10.0.1.*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 2)

	// Non-match
	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("private-ip-address"), Values: []*string{aws.String("192.168.0.1")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NetworkInterfaces)
}

func TestDescribeNetworkInterfaces_FilterByAvailabilityZone(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	// Create subnet with explicit AZ
	subnetOut, err := svc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcId),
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("ap-southeast-2a"),
	}, testAccountID)
	require.NoError(t, err)
	subnetId := *subnetOut.Subnet.SubnetId

	createTestENI(t, svc, subnetId)

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)

	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("us-east-1a")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NetworkInterfaces)
}

func TestDescribeNetworkInterfaces_FilterByGroupId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	sgA := createTestSG(t, svc, vpcId, "sg-aaa")
	sgB := createTestSG(t, svc, vpcId, "sg-bbb")

	// Create ENI with security groups
	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
		Groups:   []*string{aws.String(sgA), aws.String(sgB)},
	}, testAccountID)
	require.NoError(t, err)
	eniId := *out.NetworkInterface.NetworkInterfaceId

	createTestENI(t, svc, subnetId) // no SGs

	// Match one of the SGs
	desc, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{aws.String(sgB)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.NetworkInterfaces, 1)
	assert.Equal(t, eniId, *desc.NetworkInterfaces[0].NetworkInterfaceId)

	// Non-match
	desc, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{aws.String("sg-zzz")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.NetworkInterfaces)
}

func TestDescribeNetworkInterfaces_FilterByMacAddress(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	outCreate, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetId),
	}, testAccountID)
	require.NoError(t, err)
	mac := *outCreate.NetworkInterface.MacAddress

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("mac-address"), Values: []*string{aws.String(mac)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)

	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("mac-address"), Values: []*string{aws.String("ff:ff:ff:ff:ff:ff")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.NetworkInterfaces)
}

func TestDescribeNetworkInterfaces_FilterByAttachmentId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eniId := createTestENI(t, svc, subnetId)
	attachId, err := svc.AttachENI(testAccountID, eniId, "i-test", 0)
	require.NoError(t, err)

	createTestENI(t, svc, subnetId) // not attached

	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.attachment-id"), Values: []*string{aws.String(attachId)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eniId, *out.NetworkInterfaces[0].NetworkInterfaceId)
}

func TestDescribeNetworkInterfaces_FilterByAttachmentStatus(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")

	eni1 := createTestENI(t, svc, subnetId)
	createTestENI(t, svc, subnetId) // stays detached

	_, err := svc.AttachENI(testAccountID, eni1, "i-test", 0)
	require.NoError(t, err)

	// Filter attached
	out, err := svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.status"), Values: []*string{aws.String("attached")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	assert.Equal(t, eni1, *out.NetworkInterfaces[0].NetworkInterfaceId)

	// Filter detached
	out, err = svc.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.status"), Values: []*string{aws.String("detached")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.NetworkInterfaces, 1)
	assert.NotEqual(t, eni1, *out.NetworkInterfaces[0].NetworkInterfaceId)
}

// --- isEIPOwned tests ---

// setupEIPTestService creates a VPC service with an EIP KV bucket wired in.
func setupEIPTestService(t *testing.T) (*VPCServiceImpl, nats.KeyValue) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	svc, err := NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "eip-test"})
	require.NoError(t, err)
	svc.eipKV = kv

	return svc, kv
}

func putEIPRecord(t *testing.T, kv nats.KeyValue, key, eniID string) {
	t.Helper()
	data, err := json.Marshal(struct {
		ENIId string `json:"eni_id"`
	}{ENIId: eniID})
	require.NoError(t, err)
	_, err = kv.Put(key, data)
	require.NoError(t, err)
}

func TestIsEIPOwned_NilKV(t *testing.T) {
	svc := &VPCServiceImpl{eipKV: nil}

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.False(t, owned)
}

func TestIsEIPOwned_NoKeys(t *testing.T) {
	svc, _ := setupEIPTestService(t)

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.False(t, owned)
}

func TestIsEIPOwned_MatchingENI(t *testing.T) {
	svc, kv := setupEIPTestService(t)
	putEIPRecord(t, kv, testAccountID+".eipalloc-001", "eni-111")

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.True(t, owned)
}

func TestIsEIPOwned_NonMatchingENI(t *testing.T) {
	svc, kv := setupEIPTestService(t)
	putEIPRecord(t, kv, testAccountID+".eipalloc-001", "eni-222")

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.False(t, owned)
}

func TestIsEIPOwned_WrongAccountPrefix(t *testing.T) {
	svc, kv := setupEIPTestService(t)
	putEIPRecord(t, kv, "999999999999.eipalloc-001", "eni-111")

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.False(t, owned)
}

func TestIsEIPOwned_MalformedJSON(t *testing.T) {
	svc, kv := setupEIPTestService(t)
	_, err := kv.Put(testAccountID+".eipalloc-001", []byte("not json"))
	require.NoError(t, err)

	owned, err := svc.isEIPOwned("eni-111", testAccountID)
	assert.NoError(t, err)
	assert.False(t, owned)
}

func TestIsEIPOwned_MultipleRecords_OneMatches(t *testing.T) {
	svc, kv := setupEIPTestService(t)
	putEIPRecord(t, kv, testAccountID+".eipalloc-001", "eni-aaa")
	putEIPRecord(t, kv, testAccountID+".eipalloc-002", "eni-target")
	putEIPRecord(t, kv, testAccountID+".eipalloc-003", "eni-bbb")

	owned, err := svc.isEIPOwned("eni-target", testAccountID)
	assert.NoError(t, err)
	assert.True(t, owned)
}

// --- validateSGAttachment (Phase 2.2) ---

func TestValidateSGAttachment_OK(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sg := createTestSG(t, svc, vpcID, "ok")

	err := svc.validateSGAttachment(testAccountID, []string{sg}, vpcID)
	assert.NoError(t, err)
}

func TestValidateSGAttachment_EmptyList(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	err := svc.validateSGAttachment(testAccountID, nil, vpcID)
	assert.ErrorContains(t, err, "MissingParameter")
}

func TestValidateSGAttachment_TooMany(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgs := make([]string, 6)
	for i := range sgs {
		sgs[i] = createTestSG(t, svc, vpcID, fmt.Sprintf("sg-%d", i))
	}

	err := svc.validateSGAttachment(testAccountID, sgs, vpcID)
	assert.ErrorContains(t, err, "SecurityGroupsPerInterfaceLimitExceeded")
}

func TestValidateSGAttachment_UnknownVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	err := svc.validateSGAttachment(testAccountID, []string{"sg-aaa"}, "vpc-missing")
	assert.ErrorContains(t, err, "InvalidVpcID.NotFound")
}

func TestValidateSGAttachment_SGNotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	err := svc.validateSGAttachment(testAccountID, []string{"sg-doesntexist"}, vpcID)
	assert.ErrorContains(t, err, "InvalidGroup.NotFound")
}

func TestValidateSGAttachment_CrossVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcA := createTestVPC(t, svc, "10.0.0.0/16")
	vpcB := createTestVPC(t, svc, "10.1.0.0/16")
	sgInA := createTestSG(t, svc, vpcA, "in-a")

	err := svc.validateSGAttachment(testAccountID, []string{sgInA}, vpcB)
	assert.ErrorContains(t, err, "InvalidParameterValue")
}

// Phase 7: ModifyNetworkInterfaceAttribute must propagate vpcd errors so the
// caller knows OVN port-group reconciliation didn't happen. The KV record
// already changed by this point — that's intentional (reconciler converges).
func TestModifyNetworkInterfaceAttribute_VpcdError_Propagated(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)
	sg1 := createTestSG(t, svc, vpcId, "sg-mod-fail-1")

	// Layer a failing responder on top of the default success stub so the
	// vpc.update-port-sgs request returns an error.
	resp := []byte(`{"success":false,"error":"forced-update-port-sgs-error"}`)
	failSub, err := nc.Subscribe("vpc.update-port-sgs", func(m *nats.Msg) {
		if m.Reply != "" {
			_ = m.Respond(resp)
		}
	})
	require.NoError(t, err)
	defer func() { _ = failSub.Unsubscribe() }()

	_, err = svc.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniId),
		Groups:             []*string{aws.String(sg1)},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-update-port-sgs-error")
}
