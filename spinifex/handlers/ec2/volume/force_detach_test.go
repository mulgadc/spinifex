package handlers_ec2_volume

//test:in-package — seeding a volume's metadata document is how the attachment
// under test is created, and the metadata store is unexported.

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newForceDetachService(t *testing.T) *VolumeServiceImpl {
	t.Helper()
	svc := newTestVolumeService("ap-southeast-2a")
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	return svc
}

func seedAttachedVolume(t *testing.T, svc *VolumeServiceImpl, volumeID, accountID, instanceID string) {
	t.Helper()
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: accountID, CapacityGiB: 1,
		State: "available", AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
	}))
	require.NoError(t, svc.UpdateVolumeState(accountID, volumeID, "in-use", instanceID, "/dev/sdf"))
}

// This is the escape from the deadlock teardown exists to break: a volume
// attached to an instance that will not terminate leaves both undeletable, so
// the attachment is cleared without the guest's cooperation.
func TestForceDetachVolumeClearsTheAttachment(t *testing.T) {
	svc := newForceDetachService(t)
	seedAttachedVolume(t, svc, "vol-forcedetach1", "acct-tenant", "i-stuck0000000000")

	attachment, err := svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-forcedetach1"), Force: aws.Bool(true),
	}, "acct-tenant")
	require.NoError(t, err)

	// The previous instance is reported back: it is the only record of what
	// the volume was torn away from.
	assert.Equal(t, "i-stuck0000000000", aws.StringValue(attachment.InstanceId))
	assert.Equal(t, "detached", aws.StringValue(attachment.State))

	meta, err := svc.GetVolumeMetadata("acct-tenant", "vol-forcedetach1")
	require.NoError(t, err)
	assert.Equal(t, "available", meta.State)
	assert.Empty(t, meta.AttachedInstance)
	assert.Empty(t, meta.DeviceName)
}

// The force path is still account-scoped. Teardown may escalate past a state
// guard; it may never reach a volume belonging to another tenant.
func TestForceDetachVolumeRefusesAnotherAccountsVolume(t *testing.T) {
	svc := newForceDetachService(t)
	seedAttachedVolume(t, svc, "vol-forcedetach2", "acct-owner", "i-stuck0000000000")

	_, err := svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-forcedetach2"),
	}, "acct-intruder")

	require.Error(t, err)
	// The same answer an absent volume gives, so this cannot be used to prove
	// a volume id exists in someone else's account.
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	meta, err := svc.GetVolumeMetadata("acct-owner", "vol-forcedetach2")
	require.NoError(t, err)
	assert.Equal(t, "i-stuck0000000000", meta.AttachedInstance)
}

func TestForceDetachVolumeRejectsBadInput(t *testing.T) {
	svc := newForceDetachService(t)

	for _, input := range []*ec2.DetachVolumeInput{nil, {}, {VolumeId: aws.String("")}} {
		_, err := svc.ForceDetachVolume(context.Background(), input, "acct-tenant")
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

func TestForceDetachVolumeReportsAMissingVolume(t *testing.T) {
	svc := newForceDetachService(t)

	_, err := svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-doesnotexist"),
	}, "acct-tenant")

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// An unattached volume is already in the state the caller wants, so force
// detach is a no-op rather than an error — teardown re-runs after a crash.
func TestForceDetachVolumeOnAnUnattachedVolume(t *testing.T) {
	svc := newForceDetachService(t)
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: "vol-forcedetach3", TenantID: "acct-tenant", CapacityGiB: 1,
		State: "available", AvailabilityZone: "ap-southeast-2a", VolumeType: "gp3",
	}))

	attachment, err := svc.ForceDetachVolume(context.Background(), &ec2.DetachVolumeInput{
		VolumeId: aws.String("vol-forcedetach3"),
	}, "acct-tenant")
	require.NoError(t, err)

	assert.Empty(t, aws.StringValue(attachment.InstanceId))
}
