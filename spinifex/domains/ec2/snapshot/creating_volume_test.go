package snapshot

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A volume still being prepared has no settled contents, so it cannot be
// captured; once available the same volume snapshots normally.
func TestCreateSnapshot_RefusesACreatingVolume(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	ctx := context.Background()
	seedProviderVolume(t, svc, "vol-preparing", 4)
	seedVolumeDocument(t, store, ebsmetadata.Volume{
		VolumeID: "vol-preparing", TenantID: testAccountID, CapacityGiB: 4,
		State: "creating", AvailabilityZone: "us-east-1a",
	})

	_, err := svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-preparing")}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorIncorrectState)
	snaps, err := svc.metadata.ListSnapshotsStrict(ctx, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, snaps)

	meta, err := svc.metadata.GetVolume(ctx, testAccountID, "vol-preparing")
	require.NoError(t, err)
	meta.State = "available"
	require.NoError(t, svc.metadata.PutVolume(ctx, meta))
	_, err = svc.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String("vol-preparing")}, testAccountID)
	require.NoError(t, err)
}
