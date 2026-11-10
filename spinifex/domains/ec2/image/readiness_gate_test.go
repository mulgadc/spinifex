package image

import (
	"context"
	"testing"

	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterImage_RefusesASnapshotThatIsNotCompleted(t *testing.T) {
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
			svc, store := setupTestImageService(t)
			snap := tc.snapshot
			snap.SnapshotID, snap.VolumeID, snap.VolumeSize, snap.OwnerID = "snap-gate000001", "vol-gate000001", 8, testAccountID
			putTestSnapshotDoc(t, store, snap)

			_, err := svc.RegisterImage(context.Background(), validRegisterImageServiceInput(snap.SnapshotID), testAccountID)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "unregistered error: %v", err)
			assert.Equal(t, tc.wantCode, code)
			amis, err := svc.MetadataStore().ListAMIs(context.Background())
			require.NoError(t, err)
			assert.Empty(t, amis, "a refused registration writes no AMI")
		})
	}
}

// An AMI whose snapshot is gone reads as gone, as an AMI with no snapshot document does.
func TestCopyImage_RefusesASourceThatIsNotAvailable(t *testing.T) {
	tests := []struct {
		name     string
		amiState string
		hideSnap bool
		wantCode string
	}{
		{name: "pending", amiState: "pending", wantCode: awserrors.ErrorInvalidAMIIDUnavailable},
		{name: "failed", amiState: "failed", wantCode: awserrors.ErrorInvalidAMIIDUnavailable},
		{name: "snapshot deleted", amiState: "available", hideSnap: true, wantCode: awserrors.ErrorInvalidAMIIDNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, store := setupTestImageService(t)
			seedCopyableAMI(t, store, "ami-gate000001", "gated", testAccountID, "snap-gate000001", "vol-gate000001", 8)
			ctx := context.Background()
			ami, err := svc.MetadataStore().GetAMI(ctx, "ami-gate000001")
			require.NoError(t, err)
			ami.State = tc.amiState
			require.NoError(t, svc.MetadataStore().PutAMI(ctx, ami))
			if tc.hideSnap {
				snap := readTestSnapshotDoc(t, store, testAccountID, "snap-gate000001")
				snap.DeleteRequested = true
				putTestSnapshotDoc(t, store, snap)
			}

			_, err = svc.CopyImage(ctx, validCopyImageServiceInput("ami-gate000001", "copy"), testAccountID)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "unregistered error: %v", err)
			assert.Equal(t, tc.wantCode, code)
			amis, err := svc.MetadataStore().ListAMIs(ctx)
			require.NoError(t, err)
			assert.Len(t, amis, 1, "a refused copy writes no AMI")
		})
	}
}
