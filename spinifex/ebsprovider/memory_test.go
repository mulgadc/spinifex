package ebsprovider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryProviderLifecycleAndIdempotency(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{CopyOnWriteClone: true})
	createdAt := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	provider.now = func() time.Time { return createdAt }
	ctx := context.Background()

	create := CreateVolumeRequest{
		Versioned:        NewVersioned(),
		VolumeID:         "vol-1",
		CapacityRange:    CapacityRange{RequiredBytes: 8 << 30},
		AvailabilityZone: "ap-southeast-2a",
	}
	volume, err := provider.CreateVolume(ctx, create)
	require.NoError(t, err)
	assert.Equal(t, "memory://volume/vol-1", volume.Handle)
	assert.Equal(t, VolumeStateAvailable, volume.State)

	repeated, err := provider.CreateVolume(ctx, create)
	require.NoError(t, err)
	assert.Equal(t, volume, repeated, "same-name/same-parameters create must be idempotent")

	changed := create
	changed.CapacityRange.RequiredBytes++
	_, err = provider.CreateVolume(ctx, changed)
	require.ErrorIs(t, err, ErrAlreadyExists)

	published, err := provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "nbd:unix:/memory/vol-1.sock", published.NBDURI)

	repeatedPublish, err := provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	})
	require.NoError(t, err)
	assert.Equal(t, published, repeatedPublish)

	_, err = provider.PublishVolume(ctx, PublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-2",
	})
	require.ErrorIs(t, err, ErrVolumeInUse)
	require.ErrorIs(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}), ErrVolumeInUse)

	require.NoError(t, provider.UnpublishVolume(ctx, UnpublishVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle, NodeID: "node-1",
	}))

	expanded, err := provider.ExpandVolume(ctx, ExpandVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
		CapacityRange: CapacityRange{RequiredBytes: 16 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(16<<30), expanded.CapacityBytes)

	snapshot, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: volume.ID, VolumeHandle: volume.Handle,
	})
	require.NoError(t, err)
	assert.Equal(t, createdAt, snapshot.CreatedAt)
	assert.Equal(t, SnapshotStateCompleted, snapshot.State)
	assert.Equal(t, expanded.CapacityBytes, snapshot.SizeBytes)

	repeatedSnapshot, err := provider.CreateSnapshot(ctx, CreateSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: "snap-1", VolumeID: volume.ID, VolumeHandle: volume.Handle,
	})
	require.NoError(t, err)
	assert.Equal(t, snapshot, repeatedSnapshot)

	require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}))
	require.NoError(t, provider.DeleteVolume(ctx, DeleteVolumeRequest{
		Versioned: NewVersioned(), VolumeID: volume.ID, Handle: volume.Handle,
	}), "delete of an absent volume must be idempotent")

	require.NoError(t, provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: snapshot.ID, Handle: snapshot.Handle,
	}))
	require.NoError(t, provider.DeleteSnapshot(ctx, DeleteSnapshotRequest{
		Versioned: NewVersioned(), SnapshotID: snapshot.ID, Handle: snapshot.Handle,
	}), "delete of an absent snapshot must be idempotent")
}

func TestMemoryProviderRequiresExplicitSchemaVersion(t *testing.T) {
	provider := NewMemoryProvider(Capabilities{})
	_, err := provider.CreateVolume(context.Background(), CreateVolumeRequest{
		VolumeID: "vol-unversioned", CapacityRange: CapacityRange{RequiredBytes: 1},
	})
	require.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestProviderErrorUnwrap(t *testing.T) {
	err := &ProviderError{Code: ErrorCodeNotFound, Message: "volume disappeared"}
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, "volume disappeared", err.Error())
}
