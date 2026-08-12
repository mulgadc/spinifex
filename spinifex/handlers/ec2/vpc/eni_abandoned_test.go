package handlers_ec2_vpc

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createAutoENI creates an ENI the way the launch path does, then backdates it
// so the age guard treats it as settled rather than a launch still in flight.
func createAutoENI(t *testing.T, svc *VPCServiceImpl, subnetID, instanceID string, sgIDs []string, age time.Duration) string {
	t.Helper()
	groups := make([]*string, 0, len(sgIDs))
	for _, id := range sgIDs {
		groups = append(groups, aws.String(id))
	}
	out, err := svc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(AutoENIDescriptionPrefix + instanceID),
		Groups:      groups,
	}, testAccountID)
	require.NoError(t, err)

	eniID := *out.NetworkInterface.NetworkInterfaceId
	require.NoError(t, svc.UpdateENI(testAccountID, eniID, func(rec *ENIRecord) {
		rec.CreatedAt = time.Now().Add(-age)
	}))
	return eniID
}

func TestListAbandonedInstanceENIs(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	abandoned := createAutoENI(t, svc, subnetID, "i-abandoned", nil, time.Hour)
	createAutoENI(t, svc, subnetID, "i-inflight", nil, time.Minute)

	// A standalone ENI is indistinguishable by age or attachment; only the
	// description separates one a caller asked for from launch residue.
	standalone := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.UpdateENI(testAccountID, standalone, func(rec *ENIRecord) {
		rec.CreatedAt = time.Now().Add(-time.Hour)
	}))

	attached := createAutoENI(t, svc, subnetID, "i-running", nil, time.Hour)
	_, err := svc.AttachENI(testAccountID, attached, "i-running", 0)
	require.NoError(t, err)

	got, err := svc.ListAbandonedInstanceENIs(context.Background(), 15*time.Minute)
	require.NoError(t, err)

	ids := make([]string, 0, len(got))
	for _, o := range got {
		assert.Equal(t, testAccountID, o.AccountID)
		ids = append(ids, o.Record.NetworkInterfaceId)
	}
	assert.Equal(t, []string{abandoned}, ids,
		"only the aged, unattached, launch-created ENI is residue; the others are live or deliberate")
}

// TestAbandonedENIBlocksSecurityGroupDelete pins the consequence that makes this
// worth sweeping: the SG dependency check counts an abandoned ENI, so the group
// stays undeletable for as long as the record survives.
func TestAbandonedENIBlocksSecurityGroupDelete(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	sgID := createTestSG(t, svc, vpcID, "runner-sg")

	eniID := createAutoENI(t, svc, subnetID, "i-abandoned", []string{sgID}, time.Hour)

	_, err := svc.DeleteSecurityGroup(context.Background(), &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDependencyViolation, err.Error())

	orphans, err := svc.ListAbandonedInstanceENIs(context.Background(), 15*time.Minute)
	require.NoError(t, err)
	require.Len(t, orphans, 1)
	require.Equal(t, eniID, orphans[0].Record.NetworkInterfaceId)

	_, err = svc.DeleteNetworkInterface(context.Background(), &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSecurityGroup(context.Background(), &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.NoError(t, err, "with the residue gone the group must delete")
}
