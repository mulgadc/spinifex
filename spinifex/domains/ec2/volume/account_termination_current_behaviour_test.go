package volume_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin what terminate does
// to a DeleteOnTermination volume today; a failure means it changed.

// A snapshotted DeleteOnTermination volume loses its attachment before the
// delete refuses, so it survives as an unattached volume that the leak reaper,
// which keys on the attachment, never marks.
func TestCurrentBehaviour_AccountTermination_SnapshottedDeleteOnTerminationVolumeIsDetachedAndUnmarked(t *testing.T) {
	const (
		accountID  = "123456789012"
		bucket     = "ct-bucket"
		volumeID   = "vol-dot-snapshotted"
		instanceID = "i-0123456789abcdef0"
	)
	ctx := context.Background()
	_, nc, js := testutil.StartTestJetStream(t)
	refs, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: ec2snapshot.KVBucketVolumeSnapshots})
	require.NoError(t, err)

	store := objectstore.NewMemoryObjectStore()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	cfg := &config.Config{WalDir: t.TempDir(), Predastore: config.PredastoreConfig{Bucket: bucket}}
	volumes := ec2volume.NewVolumeServiceImplWithStore(cfg, store, nc, refs)
	volumes.SetEBSProvider(provider)
	snapshots := ec2snapshot.NewSnapshotServiceImplWithStore(cfg, store, nc, refs)
	snapshots.SetEBSProvider(provider)
	metadata := ebsmetadata.NewStore(store, bucket)

	created, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	require.NoError(t, metadata.PutVolume(ctx, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: accountID, CapacityGiB: 1,
		State: "available", ProviderHandle: created.Handle, DeleteOnTermination: true,
	}))
	_, err = snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{VolumeId: aws.String(volumeID)}, accountID)
	require.NoError(t, err)
	require.NoError(t, volumes.UpdateVolumeState(accountID, volumeID, "in-use", instanceID, "/dev/sda1"))

	err = volumes.DeleteVolumeOnTerminate(ctx, volumeID, accountID)
	assert.EqualError(t, err, awserrors.ErrorVolumeInUse, "the snapshot index refuses the terminate delete")

	survivor, err := metadata.GetVolume(ctx, accountID, volumeID)
	require.NoError(t, err, "the volume survives its instance")
	assert.Empty(t, survivor.AttachedInstance, "the attachment was cleared before the refusal")
	assert.Equal(t, "available", survivor.State)

	reaper := volumes.NewVolumeLeakReaper(func() (map[string]bool, error) {
		return map[string]bool{instanceID: true}, nil
	})
	marked, err := reaper.Sweep(ctx)
	require.NoError(t, err)
	assert.Zero(t, marked, "with no attachment left, the leak reaper cannot attribute the volume to its instance")

	_, err = volumes.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}, accountID)
	assert.EqualError(t, err, awserrors.ErrorVolumeInUse, "an ordinary delete is refused for the same reason")
}
