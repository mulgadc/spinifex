package handlers_ec2_volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testVolAccountID = "123456789012"

// seedVolume writes a volume config.json into the store so listAllVolumeIDs /
// GetVolumeConfig see it. CreateVolume cannot be used in unit tests — it needs a
// live viperblock S3 backend.
func seedVolume(t *testing.T, svc *VolumeServiceImpl, volID, state, attachedInstance string) {
	t.Helper()
	cfg := &viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{
			VolumeID:         volID,
			TenantID:         testVolAccountID,
			SizeGiB:          8,
			State:            state,
			AttachedInstance: attachedInstance,
			AvailabilityZone: "ap-southeast-2a",
		},
	}
	require.NoError(t, svc.putVolumeConfig(context.Background(), volID, cfg))
}

// TestRLC1_VolumeDeleteNotFoundOnAbsent enforces the Common Resource Lifecycle
// Contract rule #1 (AWS-faithful delete, per-service): the EC2 DeleteVolume API
// returns InvalidVolume.NotFound for an absent volume, not success. Idempotent
// convergence belongs to destroy orchestration, which tolerates NotFound via
// awserrors.IsNotFound; the public API stays AWS compatible. The attached /
// has-snapshots live-reference guards are unaffected.
func TestRLC1_VolumeDeleteNotFoundOnAbsent(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-absent00000000"),
	}, "123456789012")

	require.Errorf(t, err, "DeleteVolume on an absent volume must return %s, not success (RLC rule #1 AWS-faithful delete): destroy orchestration tolerates NotFound, the API must not", awserrors.ErrorInvalidVolumeNotFound)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidVolumeNotFound, "DeleteVolume on an absent volume must return the canonical InvalidVolume.NotFound (RLC rule #1)")
}

// TestRLC3_VolumeDeleteRequiresDetach enforces the Common Resource Lifecycle
// Contract rule #3 (live-reference guard): an attached volume is a live
// reference and must not be deleted out from under its instance — DeleteVolume
// returns VolumeInUse until it is detached.
func TestRLC3_VolumeDeleteRequiresDetach(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-attached00000", "in-use", "i-live0000000000")

	_, err := svc.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String("vol-attached00000")}, testVolAccountID)
	assert.ErrorContainsf(t, err, awserrors.ErrorVolumeInUse,
		"ADR-0005 §2: deleting an attached volume must return VolumeInUse (detach-before-delete, rule #3)")
}

// TestRLC5_VolumeGCMarksLeakedVolumeNeverDeletes enforces ADR-0005 §3 — the one
// principled exception to the GC backstop's reap-actual−desired default. A
// volume left attached to a definitively-gone instance is data: the GC must
// MARK it orphaned + alarm and NEVER delete it. A future maintainer must not be
// able to "fix" this into destroying volume data.
func TestRLC5_VolumeGCMarksLeakedVolumeNeverDeletes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-leaked0000000", "in-use", "i-gone0000000000")
	seedVolume(t, svc, "vol-live000000000", "in-use", "i-running00000000")

	reaper := svc.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return map[string]bool{"i-gone0000000000": true}, nil
	})

	marked, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, marked, "ADR-0005 §3: the GC must surface exactly the leaked volume")

	leaked, err := svc.GetVolumeConfig("vol-leaked0000000")
	require.NoErrorf(t, err, "ADR-0005 §3: the GC must NOT delete the leaked volume — it carries data; it may only mark + alarm")
	assert.NotEmpty(t, leaked.VolumeMetadata.Tags[orphanTagKey],
		"ADR-0005 §3: a leaked volume must be marked orphaned")

	// A volume attached to an instance NOT in the leaked set must be untouched —
	// node-local detection never false-marks another node's live-instance volume.
	live, err := svc.GetVolumeConfig("vol-live000000000")
	require.NoError(t, err)
	assert.Empty(t, live.VolumeMetadata.Tags[orphanTagKey],
		"a volume on a live instance must never be marked orphaned")

	// Idempotent: a second sweep re-marks nothing and still deletes nothing.
	marked, err = reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, marked, "an already-marked orphan must not be re-marked")
	_, err = svc.GetVolumeConfig("vol-leaked0000000")
	require.NoError(t, err, "ADR-0005 §3: the volume must still exist after repeated sweeps")
}
