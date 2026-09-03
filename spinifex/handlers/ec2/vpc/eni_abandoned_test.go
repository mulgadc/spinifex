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
	t.Parallel()
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

// TestListAttachedInstanceENIs pins the complement of the abandoned listing: the
// records that do name an instance, which is where a zombie hides. The two
// listings must not overlap, or a sweep would act on the same record twice.
func TestListAttachedInstanceENIs(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	attached := createAutoENI(t, svc, subnetID, "i-gone", nil, time.Hour)
	_, err := svc.AttachENI(testAccountID, attached, "i-gone", 0)
	require.NoError(t, err)

	// Attached but still inside the age guard: a launch this young may still be
	// wiring the instance up, so its ENI is not a candidate for anything.
	fresh := createAutoENI(t, svc, subnetID, "i-launching", nil, time.Minute)
	_, err = svc.AttachENI(testAccountID, fresh, "i-launching", 0)
	require.NoError(t, err)

	// Aged but never attached — the abandoned-launch listing owns this one.
	createAutoENI(t, svc, subnetID, "i-abandoned", nil, time.Hour)

	got, err := svc.ListAttachedInstanceENIs(context.Background(), 15*time.Minute)
	require.NoError(t, err)

	ids := make([]string, 0, len(got))
	for _, o := range got {
		assert.Equal(t, testAccountID, o.AccountID)
		ids = append(ids, o.Record.NetworkInterfaceId)
	}
	assert.Equal(t, []string{attached}, ids,
		"only the aged ENI that names an instance is a candidate for the staleness check")
}

// TestListAttachedInstanceENIsIgnoresZeroCreatedAt pins the age guard's fallback.
// A record written before CreatedAt existed carries no age, and age is the only
// thing standing between this listing and an ENI a launch is still using.
func TestListAttachedInstanceENIsIgnoresZeroCreatedAt(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	eniID := createAutoENI(t, svc, subnetID, "i-undated", nil, time.Hour)
	_, err := svc.AttachENI(testAccountID, eniID, "i-undated", 0)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateENI(testAccountID, eniID, func(rec *ENIRecord) {
		rec.CreatedAt = time.Time{}
	}))

	got, err := svc.ListAttachedInstanceENIs(context.Background(), 15*time.Minute)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestAbandonedENIBlocksSecurityGroupDelete pins the consequence that makes this
// worth sweeping: the SG dependency check counts an abandoned ENI, so the group
// stays undeletable for as long as the record survives.
func TestAbandonedENIBlocksSecurityGroupDelete(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	sgID := createTestSG(t, svc, vpcID, "runner-sg")

	eniID := createAutoENI(t, svc, subnetID, "i-abandoned", []string{sgID}, time.Hour)

	_, err := svc.DeleteSecurityGroup(context.Background(), &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.ErrorContains(t, err, awserrors.ErrorDependencyViolation)
	assert.ErrorContains(t, err, eniID, "the refusal must name the ENI that blocked it")

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
