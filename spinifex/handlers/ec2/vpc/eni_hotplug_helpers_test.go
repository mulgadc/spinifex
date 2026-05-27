package handlers_ec2_vpc

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetENIRecord(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	rec, err := svc.GetENIRecord(testAccountID, eniId)
	require.NoError(t, err)
	assert.Equal(t, eniId, rec.NetworkInterfaceId)
	assert.Equal(t, subnetId, rec.SubnetId)
	assert.Equal(t, "available", rec.Status)
	assert.NotEmpty(t, rec.MacAddress)
}

func TestGetENIRecord_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.GetENIRecord(testAccountID, "eni-nonexistent")
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestUpdateENI_PatchesFields(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	err := svc.UpdateENI(testAccountID, eniId, func(r *ENIRecord) {
		r.AttachmentStatus = "attaching"
		r.HotPlugSlot = 3
		r.LastAttachError = "prior failure"
	})
	require.NoError(t, err)

	rec, err := svc.GetENIRecord(testAccountID, eniId)
	require.NoError(t, err)
	assert.Equal(t, "attaching", rec.AttachmentStatus)
	assert.Equal(t, 3, rec.HotPlugSlot)
	assert.Equal(t, "prior failure", rec.LastAttachError)
}

func TestUpdateENI_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	err := svc.UpdateENI(testAccountID, "eni-nonexistent", func(r *ENIRecord) {
		r.AttachmentStatus = "attached"
	})
	assert.ErrorContains(t, err, "InvalidNetworkInterfaceID.NotFound")
}

func TestUpdateENI_NoopPatchPersists(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	err := svc.UpdateENI(testAccountID, eniId, func(*ENIRecord) {})
	require.NoError(t, err)

	rec, err := svc.GetENIRecord(testAccountID, eniId)
	require.NoError(t, err)
	assert.Equal(t, eniId, rec.NetworkInterfaceId)
}

func TestFindENIByAttachment(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	attachId, err := svc.AttachENI(testAccountID, eniId, "i-find-test", 2)
	require.NoError(t, err)

	rec, err := svc.FindENIByAttachment(testAccountID, attachId)
	require.NoError(t, err)
	assert.Equal(t, eniId, rec.NetworkInterfaceId)
	assert.Equal(t, "i-find-test", rec.InstanceId)
	assert.Equal(t, int64(2), rec.DeviceIndex)
}

func TestFindENIByAttachment_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.FindENIByAttachment(testAccountID, "eni-attach-missing")
	assert.ErrorContains(t, err, "InvalidAttachmentID.NotFound")
}

func TestFindENIByAttachment_EmptyBucket(t *testing.T) {
	svc := setupTestVPCService(t)
	_, err := svc.FindENIByAttachment(testAccountID, "eni-attach-anything")
	assert.ErrorContains(t, err, "InvalidAttachmentID.NotFound")
}

func TestFindENIByAttachment_AccountScoped(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	attachId, err := svc.AttachENI(testAccountID, eniId, "i-scoped", 0)
	require.NoError(t, err)

	_, err = svc.FindENIByAttachment("999988887777", attachId)
	assert.ErrorContains(t, err, "InvalidAttachmentID.NotFound")
}

// TestUpdateENI_RoundTripsAllNewFields exercises every Sprint 3b field on
// ENIRecord so a struct-field rename would surface as a test failure rather
// than a silent JSON-tag drift.
func TestUpdateENI_RoundTripsAllNewFields(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	eniId := createTestENI(t, svc, subnetId)

	require.NoError(t, svc.UpdateENI(testAccountID, eniId, func(r *ENIRecord) {
		r.AttachmentStatus = "detaching"
		r.HotPlugSlot = 7
		r.LastAttachError = "boom"
		r.DetachInFlight = true
		r.DetachForce = true
	}))

	rec, err := svc.GetENIRecord(testAccountID, eniId)
	require.NoError(t, err)
	assert.Equal(t, "detaching", rec.AttachmentStatus)
	assert.Equal(t, 7, rec.HotPlugSlot)
	assert.Equal(t, "boom", rec.LastAttachError)
	assert.True(t, rec.DetachInFlight)
	assert.True(t, rec.DetachForce)
}

// TestUpdateENI_PreservesUnrelatedFields ensures a partial patch does not
// clobber pre-existing record state.
func TestUpdateENI_PreservesUnrelatedFields(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcId := createTestVPC(t, svc, "10.0.0.0/16")
	subnetId := createTestSubnet(t, svc, vpcId, "10.0.1.0/24")
	out, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetId),
		Description: aws.String("preserved-desc"),
	}, testAccountID)
	require.NoError(t, err)
	eniId := *out.NetworkInterface.NetworkInterfaceId

	require.NoError(t, svc.UpdateENI(testAccountID, eniId, func(r *ENIRecord) {
		r.AttachmentStatus = "attaching"
	}))

	rec, err := svc.GetENIRecord(testAccountID, eniId)
	require.NoError(t, err)
	assert.Equal(t, "preserved-desc", rec.Description)
	assert.Equal(t, subnetId, rec.SubnetId)
	assert.NotEmpty(t, rec.MacAddress)
}
