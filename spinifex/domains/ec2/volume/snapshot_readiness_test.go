package volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateVolume_FromSnapshot_RefusesASnapshotThatIsNotCompleted(t *testing.T) {
	tests := []struct {
		name     string
		snapshot ebsmetadata.Snapshot
		wantCode string
	}{
		{name: "pending", snapshot: ebsmetadata.Snapshot{State: "pending"}, wantCode: awserrors.ErrorIncorrectState},
		{name: "error", snapshot: ebsmetadata.Snapshot{State: "error"}, wantCode: awserrors.ErrorIncorrectState},
		{name: "deleted while pending", snapshot: ebsmetadata.Snapshot{State: "pending", DeleteRequested: true}, wantCode: awserrors.ErrorInvalidSnapshotNotFound},
		{name: "deleted after completing", snapshot: ebsmetadata.Snapshot{State: "completed", DeleteRequested: true, AmbiguousCreate: true}, wantCode: awserrors.ErrorInvalidSnapshotNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := objectstore.NewMemoryObjectStore()
			svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
			metadata := ebsmetadata.NewStore(store, "test-bucket")
			snap := tc.snapshot
			snap.SnapshotID, snap.VolumeID, snap.VolumeSize, snap.OwnerID = "snap-gate000001", "vol-source", 8, testVolAccountID
			require.NoError(t, metadata.PutSnapshot(context.Background(), snap))

			_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
				SnapshotId:       aws.String(snap.SnapshotID),
			}, testVolAccountID)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "unregistered error: %v", err)
			assert.Equal(t, tc.wantCode, code)
			volumes, err := metadata.ListAllVolumes(context.Background())
			require.NoError(t, err)
			assert.Empty(t, volumes, "a refused CreateVolume writes no volume")
		})
	}
}
