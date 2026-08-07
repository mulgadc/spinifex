package vblegacy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/require"
)

// fullVBState populates every field of viperblock.VBState with a distinct,
// non-zero value so a marshalled comparison actually exercises every tag.
func fullVBState() viperblock.VBState {
	return viperblock.VBState{
		VolumeName:          "vol-full",
		VolumeSize:          10 * 1024 * 1024 * 1024,
		BlockSize:           4096,
		ObjBlockSize:        4 * 1024 * 1024,
		SeqNum:              7,
		ObjectNum:           8,
		WALNum:              9,
		BlockToObjectWALNum: 10,
		Version:             1,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:            "vol-full",
				VolumeName:          "my-volume",
				TenantID:            "111122223333",
				SizeGiB:             10,
				State:               "available",
				CreatedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				AttachedAt:          time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC),
				AvailabilityZone:    "ap-southeast-2a",
				AttachedInstance:    "i-abc123",
				DeviceName:          "/dev/nbd1",
				VolumeType:          "gp3",
				IOPS:                3000,
				Throughput:          125,
				Tags:                map[string]string{"Name": "test"},
				SnapshotID:          "snap-src",
				DeleteOnTermination: true,
			},
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         "ami-full",
				Name:            "debian-12-cloud",
				Description:     "a description",
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				CreationDate:    time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC),
				RootDeviceType:  "ebs",
				Virtualization:  "hvm",
				ImageOwnerAlias: "spinifex",
				VolumeSizeGiB:   8,
				SnapshotID:      "snap-ami",
				BootMode:        "uefi",
				Distro:          "debian",
				DistroFamily:    "debian",
				Tags:            map[string]string{"env": "test"},
				State:           "available",
			},
			Modification: &viperblock.VolumeModification{
				VolumeID:           "vol-full",
				ModificationState:  "modifying",
				Progress:           50,
				StatusMessage:      "in progress",
				OriginalSize:       10,
				OriginalIops:       3000,
				OriginalVolumeType: "gp3",
				TargetSize:         20,
				TargetIops:         4000,
				TargetVolumeType:   "io1",
				StartTime:          time.Date(2026, 1, 5, 3, 4, 5, 0, time.UTC),
				EndTime:            time.Date(2026, 1, 5, 4, 4, 5, 0, time.UTC),
			},
		},
		ShardedWAL:        true,
		SnapshotID:        "snap-vb",
		SourceVolumeName:  "vol-source",
		EncryptionEnabled: true,
		VolumeUUID:        [4]byte{1, 2, 3, 4},
		SeqNumHighWater:   1 << 20,
		StateSeqNum:       3,
		KeyFingerprint:    "deadbeef",
	}
}

// fullMirrorState is fullVBState's field-for-field twin in the mirror types.
// Every value must match fullVBState exactly.
func fullMirrorState() VBState {
	return VBState{
		VolumeName:          "vol-full",
		VolumeSize:          10 * 1024 * 1024 * 1024,
		BlockSize:           4096,
		ObjBlockSize:        4 * 1024 * 1024,
		SeqNum:              7,
		ObjectNum:           8,
		WALNum:              9,
		BlockToObjectWALNum: 10,
		Version:             1,
		VolumeConfig: VolumeConfig{
			VolumeMetadata: VolumeMetadata{
				VolumeID:            "vol-full",
				VolumeName:          "my-volume",
				TenantID:            "111122223333",
				SizeGiB:             10,
				State:               "available",
				CreatedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				AttachedAt:          time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC),
				AvailabilityZone:    "ap-southeast-2a",
				AttachedInstance:    "i-abc123",
				DeviceName:          "/dev/nbd1",
				VolumeType:          "gp3",
				IOPS:                3000,
				Throughput:          125,
				Tags:                map[string]string{"Name": "test"},
				SnapshotID:          "snap-src",
				DeleteOnTermination: true,
			},
			AMIMetadata: AMIMetadata{
				ImageID:         "ami-full",
				Name:            "debian-12-cloud",
				Description:     "a description",
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				CreationDate:    time.Date(2026, 1, 4, 3, 4, 5, 0, time.UTC),
				RootDeviceType:  "ebs",
				Virtualization:  "hvm",
				ImageOwnerAlias: "spinifex",
				VolumeSizeGiB:   8,
				SnapshotID:      "snap-ami",
				BootMode:        "uefi",
				Distro:          "debian",
				DistroFamily:    "debian",
				Tags:            map[string]string{"env": "test"},
				State:           "available",
			},
			Modification: &VolumeModification{
				VolumeID:           "vol-full",
				ModificationState:  "modifying",
				Progress:           50,
				StatusMessage:      "in progress",
				OriginalSize:       10,
				OriginalIops:       3000,
				OriginalVolumeType: "gp3",
				TargetSize:         20,
				TargetIops:         4000,
				TargetVolumeType:   "io1",
				StartTime:          time.Date(2026, 1, 5, 3, 4, 5, 0, time.UTC),
				EndTime:            time.Date(2026, 1, 5, 4, 4, 5, 0, time.UTC),
			},
		},
		ShardedWAL:        true,
		SnapshotID:        "snap-vb",
		SourceVolumeName:  "vol-source",
		EncryptionEnabled: true,
		VolumeUUID:        [4]byte{1, 2, 3, 4},
		SeqNumHighWater:   1 << 20,
		StateSeqNum:       3,
		KeyFingerprint:    "deadbeef",
	}
}

// TestVBStateMirror_ByteForByteEquivalence is the drift guard the mirror
// types depend on: every field of viperblock.VBState (down through
// VolumeConfig, VolumeMetadata, AMIMetadata, VolumeModification) is set to a
// distinct value and marshalled from both the real type and the mirror. A
// missing field, wrong JSON tag, or wrong omitempty on either side changes
// the bytes and fails this test — a fixture built from the mirror would
// otherwise seed a document production code reads differently than intended.
func TestVBStateMirror_ByteForByteEquivalence(t *testing.T) {
	realBytes, err := json.Marshal(fullVBState())
	require.NoError(t, err)

	mirror, err := json.Marshal(fullMirrorState())
	require.NoError(t, err)

	require.Equal(t, string(realBytes), string(mirror))
}

// TestVBStateMirror_ZeroValueEquivalence covers the common fixture case where
// only a few fields are set and the rest of VBState (and VolumeConfig's
// unused half, e.g. AMIMetadata on a volume fixture) stays at its zero value
// — omitempty and nil-vs-empty-map behavior must agree on the zero value too.
func TestVBStateMirror_ZeroValueEquivalence(t *testing.T) {
	realBytes, err := json.Marshal(viperblock.VBState{})
	require.NoError(t, err)

	mirror, err := json.Marshal(VBState{})
	require.NoError(t, err)

	require.Equal(t, string(realBytes), string(mirror))
}

// TestConfigWrapperMirror_ByteForByteEquivalence guards ConfigWrapper against
// the same drift as VBState, for the shape written before a volume has a
// full VBState (e.g. CreateVolume pre-mount).
func TestConfigWrapperMirror_ByteForByteEquivalence(t *testing.T) {
	type realWrapper struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}

	realBytes, err := json.Marshal(realWrapper{VolumeConfig: fullVBState().VolumeConfig})
	require.NoError(t, err)

	mirror, err := json.Marshal(ConfigWrapper{VolumeConfig: fullMirrorState().VolumeConfig})
	require.NoError(t, err)

	require.Equal(t, string(realBytes), string(mirror))
}
