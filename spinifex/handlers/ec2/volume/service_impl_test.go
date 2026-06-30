package handlers_ec2_volume

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestVolumeService(az string) *VolumeServiceImpl {
	cfg := &config.Config{
		AZ: az,
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			Region:    "ap-southeast-2",
			Host:      "localhost:9000",
			AccessKey: "testkey",
			SecretKey: "testsecret",
		},
		WalDir: "/tmp/test-wal",
	}
	return NewVolumeServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nil)
}

func TestCreateVolume_Validation(t *testing.T) {
	tests := []struct {
		name    string
		az      string
		input   *ec2.CreateVolumeInput
		wantErr string
	}{
		{
			name:    "NilInput",
			az:      "ap-southeast-2a",
			input:   nil,
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_Zero",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(0),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_Negative",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(-5),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_TooLarge",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(16385),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "InvalidSize_NoSize",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "UnsupportedVolumeType_IO1",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("io1"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "UnsupportedVolumeType_GP2",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("gp2"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "UnsupportedVolumeType_ST1",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				VolumeType:       aws.String("st1"),
			},
			wantErr: awserrors.ErrorUnknownVolumeType,
		},
		{
			name: "Iops_BelowBaseline",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(2999),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Iops_AboveCeiling",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(16001),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "Iops_AboveRatioForSmallVolume",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(10),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(6000),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "MismatchedAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("us-east-1a"),
			},
			wantErr: awserrors.ErrorInvalidAvailabilityZone,
		},
		{
			name: "EmptyAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String(""),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "NilAZ",
			az:   "ap-southeast-2a",
			input: &ec2.CreateVolumeInput{
				Size: aws.Int64(80),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService(tt.az)
			_, err := svc.CreateVolume(tt.input, "")
			assert.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

// TestCreateVolume_PassesValidation verifies that valid inputs pass validation
// and only fail at the viperblock/S3 layer (no S3 backend in unit tests).
func TestCreateVolume_PassesValidation(t *testing.T) {
	tests := []struct {
		name  string
		input *ec2.CreateVolumeInput
	}{
		{
			name: "MinSize",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(1),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "MaxSize",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(16384),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "DefaultsToGP3",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
			},
		},
		{
			name: "ExplicitIopsInRange",
			input: &ec2.CreateVolumeInput{
				Size:             aws.Int64(80),
				AvailabilityZone: aws.String("ap-southeast-2a"),
				Iops:             aws.Int64(8000),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			_, err := svc.CreateVolume(tt.input, "")
			if err != nil {
				assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, err.Error())
				assert.NotEqual(t, awserrors.ErrorInvalidAvailabilityZone, err.Error())
			}
		})
	}
}

func TestDeleteVolume_Validation(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteVolumeInput
		wantErr string
	}{
		{
			name:    "NilInput",
			input:   nil,
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name:    "EmptyInput",
			input:   &ec2.DeleteVolumeInput{},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "NilVolumeId",
			input: &ec2.DeleteVolumeInput{
				VolumeId: nil,
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "EmptyVolumeId",
			input: &ec2.DeleteVolumeInput{
				VolumeId: aws.String(""),
			},
			wantErr: awserrors.ErrorInvalidParameterValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestVolumeService("ap-southeast-2a")
			_, err := svc.DeleteVolume(tt.input, "")
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestDescribeVolumeStatus_NilInputDefaults(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	// nil input is defaulted to empty, then hits the slow path which
	// calls listAllVolumeIDs. With an empty MemoryObjectStore, no
	// volumes are found and an empty result is returned.
	output, err := svc.DescribeVolumeStatus(nil, "")
	require.NoError(t, err)
	assert.Empty(t, output.VolumeStatuses)
}

func TestDescribeVolumeStatus_WithVolumeIDs(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	// When volume IDs are provided, the fast path is taken. With an
	// empty MemoryObjectStore, the volume config is not found and an
	// InvalidVolume.NotFound error is returned.
	_, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-abc123")},
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// newTestVolumeServiceWithStore creates a volume service with a specific memory store
func newTestVolumeServiceWithStore(az string, store *objectstore.MemoryObjectStore) *VolumeServiceImpl {
	cfg := &config.Config{
		AZ: az,
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			Region:    "ap-southeast-2",
			Host:      "localhost:9000",
			AccessKey: "testkey",
			SecretKey: "testsecret",
		},
		WalDir: "/tmp/test-wal",
	}
	return NewVolumeServiceImplWithStore(cfg, store, nil)
}

func TestCreateVolume_FromSnapshot_PassesValidation(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-test123"

	// Create snapshot metadata in store (matches spinifex snapshot service format)
	snapMeta := snapshotMetadata{
		VolumeID:   "vol-source",
		VolumeSize: 50,
	}
	snapData, err := json.Marshal(snapMeta)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(snapData)),
	})
	require.NoError(t, err)

	// CreateVolume from snapshot without explicit size passes validation
	// (fails later at viperblock backend init because no S3 server in tests)
	_, err = svc.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, "")
	if err != nil {
		// Should not be a snapshot or validation error - those are the paths we're testing
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}

func TestCreateVolume_FromSnapshot_WithExplicitSize(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-test456"

	snapMeta := snapshotMetadata{
		VolumeID:   "vol-source",
		VolumeSize: 50,
	}
	snapData, err := json.Marshal(snapMeta)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(snapData)),
	})
	require.NoError(t, err)

	// CreateVolume from snapshot with explicit larger size passes validation
	_, err = svc.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(100),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, "")
	if err != nil {
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	}
}

func TestCreateVolume_FromSnapshot_NotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String("snap-nonexistent"),
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
}

func TestCreateVolume_FromSnapshot_SizeSmallerThanSnapshot(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-sizecheck"

	snapMeta := snapshotMetadata{
		VolumeID:   "vol-source",
		VolumeSize: 50,
	}
	snapData, err := json.Marshal(snapMeta)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(snapData)),
	})
	require.NoError(t, err)

	// Size 10 < snapshot size 50 -- must be rejected
	_, err = svc.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(10),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

func TestCreateVolume_FromSnapshot_SizeEqualToSnapshot(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-sizeequal"

	snapMeta := snapshotMetadata{
		VolumeID:   "vol-source",
		VolumeSize: 50,
	}
	snapData, err := json.Marshal(snapMeta)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader(string(snapData)),
	})
	require.NoError(t, err)

	// Size == snapshot size should pass validation (may fail at backend init)
	_, err = svc.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(50),
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, "")
	if err != nil {
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		assert.NotContains(t, err.Error(), awserrors.ErrorInvalidSnapshotNotFound)
	}
}

func TestCreateVolume_FromSnapshot_CorruptMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	snapshotID := "snap-corrupt"

	// Put invalid JSON as snapshot metadata
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(snapshotID + "/metadata.json"),
		Body:   strings.NewReader("not valid json{{{"),
	})
	require.NoError(t, err)

	_, err = svc.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("ap-southeast-2a"),
		SnapshotId:       aws.String(snapshotID),
	}, "")
	require.Error(t, err)
}

// setupTestVolumeKV creates a NATS JetStream test server and returns a KV bucket.
func setupTestVolumeKV(t *testing.T) nats.KeyValue {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	js, err := nc.JetStream()
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: "spinifex-volume-snapshots",
	})
	require.NoError(t, err)
	return kv
}

func createVolumeInStore(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string) {
	t.Helper()
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID: volumeID,
				SizeGiB:  10,
				State:    "available",
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

func TestDeleteVolume_BlockedByKV(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-kvblocked"
	createVolumeInStore(t, store, volumeID)

	// Put a snapshot ref in KV
	data, err := json.Marshal([]string{"snap-001"})
	require.NoError(t, err)
	_, err = kv.Put(volumeID, data)
	require.NoError(t, err)

	// DeleteVolume should be blocked
	_, err = svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorVolumeInUse)
}

func TestDeleteVolume_AllowedByKV(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	volumeID := "vol-kvallowed"
	createVolumeInStore(t, store, volumeID)

	// No KV entry → delete allowed
	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.NoError(t, err)
}

func TestDeleteVolume_ErrorWhenKVNil(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	// snapshotKV is nil by default

	volumeID := "vol-nokvtest"
	createVolumeInStore(t, store, volumeID)

	// Should fail because snapshotKV is nil
	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorServerInternal)
}

// createVolumeInStoreWithMeta seeds a volume config.json with custom metadata.
func createVolumeInStoreWithMeta(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, meta viperblock.VolumeMetadata) {
	t.Helper()
	wrapper := volumeConfigWrapper{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: meta,
		},
	}
	data, err := json.Marshal(wrapper)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// createVolumeInStoreWithVBState seeds a volume config.json as a full VBState
// (with BlockSize > 0) so that mergeVolumeConfig preserves VBState fields.
func createVolumeInStoreWithVBState(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, meta viperblock.VolumeMetadata, blockSize uint32, seqNum uint64) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName: volumeID,
		VolumeSize: meta.SizeGiB * 1024 * 1024 * 1024,
		BlockSize:  blockSize,
		SeqNum:     seqNum,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: meta,
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
}

// --- Group 1: getVolumeByID tests ---

func TestGetVolumeByID_FullMetadata(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	now := time.Now()
	meta := viperblock.VolumeMetadata{
		VolumeID:            "vol-full",
		SizeGiB:             20,
		State:               "in-use",
		CreatedAt:           now,
		AvailabilityZone:    "ap-southeast-2a",
		VolumeType:          "gp3",
		IOPS:                5000,
		SnapshotID:          "snap-abc",
		AttachedInstance:    "i-12345",
		DeviceName:          "/dev/nbd0",
		DeleteOnTermination: true,
		AttachedAt:          now,
		Tags:                map[string]string{"Name": "test-vol", "env": "dev"},
	}
	// Seed as a full VBState with EncryptionEnabled=true so getVolumeByID
	// reports Encrypted via the authoritative VBState.EncryptionEnabled path.
	state := viperblock.VBState{
		VolumeName:        "vol-full",
		VolumeSize:        meta.SizeGiB * 1024 * 1024 * 1024,
		BlockSize:         4096,
		EncryptionEnabled: true,
		VolumeConfig:      viperblock.VolumeConfig{VolumeMetadata: meta},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-full/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	result, err := svc.getVolumeByID("vol-full")
	require.NoError(t, err)
	vol := result.volume

	assert.Equal(t, "vol-full", *vol.VolumeId)
	assert.Equal(t, int64(20), *vol.Size)
	assert.Equal(t, "in-use", *vol.State)
	assert.Equal(t, "gp3", *vol.VolumeType)
	assert.Equal(t, int64(5000), *vol.Iops)
	assert.Equal(t, "snap-abc", *vol.SnapshotId)
	assert.True(t, *vol.Encrypted)
	assert.Equal(t, "ap-southeast-2a", *vol.AvailabilityZone)

	// Attachment
	require.Len(t, vol.Attachments, 1)
	att := vol.Attachments[0]
	assert.Equal(t, "i-12345", *att.InstanceId)
	assert.Equal(t, "/dev/nbd0", *att.Device)
	assert.Equal(t, "attached", *att.State)
	assert.True(t, *att.DeleteOnTermination)

	// Tags
	assert.Len(t, vol.Tags, 2)
}

func TestGetVolumeByID_AttachmentDetached(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := viperblock.VolumeMetadata{
		VolumeID:         "vol-detach",
		SizeGiB:          10,
		State:            "available",
		AttachedInstance: "i-99999",
		DeviceName:       "/dev/nbd1",
	}
	createVolumeInStoreWithMeta(t, store, "vol-detach", meta)

	result, err := svc.getVolumeByID("vol-detach")
	require.NoError(t, err)

	require.Len(t, result.volume.Attachments, 1)
	assert.Equal(t, "detached", *result.volume.Attachments[0].State)
}

func TestGetVolumeByID_DefaultStateAndType(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := viperblock.VolumeMetadata{
		VolumeID: "vol-defaults",
		SizeGiB:  5,
		State:    "",
	}
	createVolumeInStoreWithMeta(t, store, "vol-defaults", meta)

	result, err := svc.getVolumeByID("vol-defaults")
	require.NoError(t, err)

	assert.Equal(t, "available", *result.volume.State)
	assert.Equal(t, "gp3", *result.volume.VolumeType)
}

func TestGetVolumeByID_EmptyVolumeID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := viperblock.VolumeMetadata{
		VolumeID: "",
		SizeGiB:  10,
	}
	createVolumeInStoreWithMeta(t, store, "vol-emptyid", meta)

	_, err := svc.getVolumeByID("vol-emptyid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "volume ID is empty")
}

func TestGetVolumeByID_ZeroSize(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := viperblock.VolumeMetadata{
		VolumeID: "vol-zerosize",
		SizeGiB:  0,
	}
	createVolumeInStoreWithMeta(t, store, "vol-zerosize", meta)

	_, err := svc.getVolumeByID("vol-zerosize")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zero size")
}

func TestGetVolumeByID_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.getVolumeByID("vol-nonexistent")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// --- Group 2: DescribeVolumes tests ---

func TestDescribeVolumes_NilInput(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Seed one volume so slow path has something to find
	createVolumeInStoreWithMeta(t, store, "vol-nil1", viperblock.VolumeMetadata{
		VolumeID: "vol-nil1", SizeGiB: 10, State: "available",
	})

	output, err := svc.DescribeVolumes(nil, "")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
}

func TestDescribeVolumes_EmptyStore(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "")
	require.NoError(t, err)
	assert.Empty(t, output.Volumes)
}

func TestDescribeVolumes_SlowPath_MultipleVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-a", "vol-b", "vol-c"} {
		createVolumeInStoreWithMeta(t, store, id, viperblock.VolumeMetadata{
			VolumeID: id, SizeGiB: 10, State: "available",
		})
	}

	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 3)
}

func TestDescribeVolumes_FastPath_SpecificIDs(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-x", "vol-y", "vol-z"} {
		createVolumeInStoreWithMeta(t, store, id, viperblock.VolumeMetadata{
			VolumeID: id, SizeGiB: 10, State: "available",
		})
	}

	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-x"), aws.String("vol-z")},
	}, "")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 2)

	ids := map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-x"])
	assert.True(t, ids["vol-z"])
}

func TestDescribeVolumes_FastPath_MixedExistingAndMissing(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-exists", viperblock.VolumeMetadata{
		VolumeID: "vol-exists", SizeGiB: 10, State: "available",
	})

	// AWS returns InvalidVolume.NotFound when any requested ID is missing
	_, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-exists"), aws.String("vol-ghost")},
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

func TestDescribeVolumes_FastPath_NilVolumeID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-ok", viperblock.VolumeMetadata{
		VolumeID: "vol-ok", SizeGiB: 10, State: "available",
	})

	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{nil, aws.String("vol-ok")},
	}, "")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
}

// --- Group 2b: Account scoping tests ---

func TestDescribeVolumes_AccountScoping_SlowPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Create volumes for two different accounts
	createVolumeInStoreWithMeta(t, store, "vol-acctA", viperblock.VolumeMetadata{
		VolumeID: "vol-acctA", SizeGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-acctB", viperblock.VolumeMetadata{
		VolumeID: "vol-acctB", SizeGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Account A sees only its own volume
	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "111111111111")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-acctA"], "Account A should see its own volume")
	assert.False(t, ids["vol-acctB"], "Account A should NOT see Account B's volume")

	// Account B sees only its own volume
	output, err = svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "222222222222")
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, v := range output.Volumes {
		ids[*v.VolumeId] = true
	}
	assert.True(t, ids["vol-acctB"], "Account B should see its own volume")
	assert.False(t, ids["vol-acctA"], "Account B should NOT see Account A's volume")
}

func TestDescribeVolumes_AccountScoping_FastPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-mine", viperblock.VolumeMetadata{
		VolumeID: "vol-mine", SizeGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-other", viperblock.VolumeMetadata{
		VolumeID: "vol-other", SizeGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Requesting another account's volume by ID returns NotFound
	_, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-other")},
	}, "111111111111")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Requesting own volume by ID succeeds
	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-mine")},
	}, "111111111111")
	require.NoError(t, err)
	assert.Len(t, output.Volumes, 1)
	assert.Equal(t, "vol-mine", *output.Volumes[0].VolumeId)
}

func TestDeleteVolume_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = setupTestVolumeKV(t)

	createVolumeInStoreWithMeta(t, store, "vol-owned", viperblock.VolumeMetadata{
		VolumeID: "vol-owned", SizeGiB: 10, State: "available", TenantID: "111111111111",
	})

	// Another account cannot delete
	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-owned"),
	}, "222222222222")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Owner can delete
	_, err = svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-owned"),
	}, "111111111111")
	require.NoError(t, err)
}

func TestModifyVolume_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-modify", viperblock.VolumeMetadata{
		VolumeID: "vol-modify", SizeGiB: 10, State: "available", TenantID: "111111111111",
	})

	// Another account cannot modify
	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modify"),
		Size:     aws.Int64(20),
	}, "222222222222")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())

	// Owner can modify
	output, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modify"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	assert.Equal(t, int64(20), *output.VolumeModification.TargetSize)
}

func TestDescribeVolumeStatus_AccountScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-statusA", viperblock.VolumeMetadata{
		VolumeID: "vol-statusA", SizeGiB: 10, State: "available", TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-statusB", viperblock.VolumeMetadata{
		VolumeID: "vol-statusB", SizeGiB: 10, State: "available", TenantID: "222222222222",
	})

	// Slow path: Account A only sees its own volume status
	output, err := svc.DescribeVolumeStatus(nil, "111111111111")
	require.NoError(t, err)
	assert.Len(t, output.VolumeStatuses, 1)
	assert.Equal(t, "vol-statusA", *output.VolumeStatuses[0].VolumeId)

	// Fast path: Account A cannot query Account B's volume status
	_, err = svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-statusB")},
	}, "111111111111")
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

func TestCreateVolume_StampsAccountID(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Test via the validation path — CreateVolume should not fail because of accountID.
	_, err := svc.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(1),
		AvailabilityZone: aws.String("ap-southeast-2a"),
	}, "111111111111")
	// Will error at viperblock layer, not at account validation
	if err != nil {
		assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// --- Group 3: ModifyVolume tests ---

func TestModifyVolume_NilVolumeID(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: nil,
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeIDMalformed, err.Error())
}

func TestModifyVolume_EmptyVolumeID(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String(""),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeIDMalformed, err.Error())
}

func TestModifyVolume_VolumeNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-nonexistent"),
		Size:     aws.Int64(20),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

func TestModifyVolume_ShrinkRejected(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-shrink", viperblock.VolumeMetadata{
		VolumeID: "vol-shrink", SizeGiB: 10, State: "available",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-shrink"),
		Size:     aws.Int64(5),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestModifyVolume_SameSizeRejected(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-same", viperblock.VolumeMetadata{
		VolumeID: "vol-same", SizeGiB: 10, State: "available",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-same"),
		Size:     aws.Int64(10),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestModifyVolume_AttachedInUse(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-inuse", viperblock.VolumeMetadata{
		VolumeID:         "vol-inuse",
		SizeGiB:          10,
		State:            "in-use",
		AttachedInstance: "i-12345",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-inuse"),
		Size:     aws.Int64(20),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIncorrectState, err.Error())
}

func TestModifyVolume_SuccessfulGrow(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-grow", viperblock.VolumeMetadata{
		VolumeID:   "vol-grow",
		SizeGiB:    10,
		State:      "available",
		VolumeType: "gp3",
		IOPS:       3000,
	})

	output, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-grow"),
		Size:     aws.Int64(20),
	}, "")
	require.NoError(t, err)

	mod := output.VolumeModification
	assert.Equal(t, "vol-grow", *mod.VolumeId)
	assert.Equal(t, int64(10), *mod.OriginalSize)
	assert.Equal(t, int64(20), *mod.TargetSize)
	assert.Equal(t, "completed", *mod.ModificationState)
	assert.Equal(t, int64(100), *mod.Progress)

	// Verify persisted config
	cfg, err := svc.GetVolumeConfig("vol-grow")
	require.NoError(t, err)
	assert.Equal(t, uint64(20), cfg.VolumeMetadata.SizeGiB)
}

func TestModifyVolume_ModifyTypeAndIOPS(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-typemod", viperblock.VolumeMetadata{
		VolumeID:   "vol-typemod",
		SizeGiB:    10,
		State:      "available",
		VolumeType: "gp3",
		IOPS:       3000,
	})

	output, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId:   aws.String("vol-typemod"),
		Size:       aws.Int64(20),
		VolumeType: aws.String("io1"),
		Iops:       aws.Int64(10000),
	}, "")
	require.NoError(t, err)

	mod := output.VolumeModification
	assert.Equal(t, "gp3", *mod.OriginalVolumeType)
	assert.Equal(t, "io1", *mod.TargetVolumeType)
	assert.Equal(t, int64(3000), *mod.OriginalIops)
	assert.Equal(t, int64(10000), *mod.TargetIops)
}

func TestModifyVolume_AvailableWithAttachment(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Volume attached but state is "available" (stopped instance) -- allowed
	createVolumeInStoreWithMeta(t, store, "vol-stopinst", viperblock.VolumeMetadata{
		VolumeID:         "vol-stopinst",
		SizeGiB:          10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	output, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-stopinst"),
		Size:     aws.Int64(20),
	}, "")
	require.NoError(t, err)
	assert.Equal(t, int64(20), *output.VolumeModification.TargetSize)
}

// --- Group 4: UpdateVolumeState tests ---

func TestUpdateVolumeState_AttachVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-attach", viperblock.VolumeMetadata{
		VolumeID: "vol-attach", SizeGiB: 10, State: "available",
	})

	err := svc.UpdateVolumeState("vol-attach", "in-use", "i-abc123", "/dev/nbd0")
	require.NoError(t, err)

	cfg, err := svc.GetVolumeConfig("vol-attach")
	require.NoError(t, err)
	assert.Equal(t, "in-use", cfg.VolumeMetadata.State)
	assert.Equal(t, "i-abc123", cfg.VolumeMetadata.AttachedInstance)
	assert.Equal(t, "/dev/nbd0", cfg.VolumeMetadata.DeviceName)
	assert.False(t, cfg.VolumeMetadata.AttachedAt.IsZero())
}

func TestUpdateVolumeState_DetachVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-detach2", viperblock.VolumeMetadata{
		VolumeID:         "vol-detach2",
		SizeGiB:          10,
		State:            "in-use",
		AttachedInstance: "i-xyz789",
		DeviceName:       "/dev/nbd1",
	})

	err := svc.UpdateVolumeState("vol-detach2", "available", "", "")
	require.NoError(t, err)

	cfg, err := svc.GetVolumeConfig("vol-detach2")
	require.NoError(t, err)
	assert.Equal(t, "available", cfg.VolumeMetadata.State)
	assert.Empty(t, cfg.VolumeMetadata.AttachedInstance)
	assert.Empty(t, cfg.VolumeMetadata.DeviceName)
}

func TestUpdateVolumeState_VolumeNotFound(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	err := svc.UpdateVolumeState("vol-missing", "available", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get volume config")
}

func TestUpdateVolumeState_PreservesVBState(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	meta := viperblock.VolumeMetadata{
		VolumeID: "vol-vbstate", SizeGiB: 10, State: "available",
	}
	createVolumeInStoreWithVBState(t, store, "vol-vbstate", meta, 4096, 5)

	err := svc.UpdateVolumeState("vol-vbstate", "in-use", "i-preserve", "/dev/nbd0")
	require.NoError(t, err)

	// Re-read the raw JSON to verify VBState fields survived
	getResult, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-vbstate/config.json"),
	})
	require.NoError(t, err)

	body, err := io.ReadAll(getResult.Body)
	require.NoError(t, err)

	var state viperblock.VBState
	require.NoError(t, json.Unmarshal(body, &state))

	assert.Equal(t, uint32(4096), state.BlockSize)
	assert.Equal(t, uint64(5), state.SeqNum)
	assert.Equal(t, "in-use", state.VolumeConfig.VolumeMetadata.State)
	assert.Equal(t, "i-preserve", state.VolumeConfig.VolumeMetadata.AttachedInstance)
}

// --- Group 6: listAllVolumeIDs tests ---

func TestListAllVolumeIDs_FiltersCorrectly(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Seed objects with various prefixes
	for _, key := range []string{
		"vol-abc/config.json",
		"vol-def/config.json",
		"vol-abc-efi/config.json",
		"vol-abc-cloudinit/config.json",
		"ami-123/metadata.json",
		"snap-456/metadata.json",
	} {
		_, err := store.PutObject(&s3.PutObjectInput{
			Bucket: aws.String("test-bucket"),
			Key:    aws.String(key),
			Body:   strings.NewReader("{}"),
		})
		require.NoError(t, err)
	}

	ids, err := svc.listAllVolumeIDs()
	require.NoError(t, err)

	// Should only contain vol-abc and vol-def (not efi/cloudinit/ami/snap)
	assert.Len(t, ids, 2)
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}
	assert.True(t, idSet["vol-abc"])
	assert.True(t, idSet["vol-def"])
}

func TestListAllVolumeIDs_EmptyBucket(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	ids, err := svc.listAllVolumeIDs()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestListAllVolumeIDs_NilPrefix(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Seed a single volume to ensure the loop runs
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("vol-only/config.json"),
		Body:   strings.NewReader("{}"),
	})
	require.NoError(t, err)

	ids, err := svc.listAllVolumeIDs()
	require.NoError(t, err)
	assert.Len(t, ids, 1)
	assert.Equal(t, "vol-only", ids[0])
}

// --- Group 7: DeleteVolume remaining tests ---

func TestDeleteVolume_VolumeInUse(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	createVolumeInStoreWithMeta(t, store, "vol-busy", viperblock.VolumeMetadata{
		VolumeID:         "vol-busy",
		SizeGiB:          10,
		State:            "in-use",
		AttachedInstance: "i-running",
	})

	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-busy"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorVolumeInUse, err.Error())
}

func TestDeleteVolume_VolumeAttachedButAvailable(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	// State != "available" triggers the check even without "in-use"
	// Actually: the code checks `State != "available" || AttachedInstance != ""`
	// So having AttachedInstance set while state is "available" still triggers VolumeInUse
	createVolumeInStoreWithMeta(t, store, "vol-attached", viperblock.VolumeMetadata{
		VolumeID:         "vol-attached",
		SizeGiB:          10,
		State:            "available",
		AttachedInstance: "i-stopped",
	})

	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-attached"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorVolumeInUse, err.Error())
}

func TestDeleteVolume_EmptyStateUnattachedDeletable(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv

	// Drift: a detach/terminate left State empty with no attachment. The volume
	// is not in use and must be deletable, not VolumeInUse (mulga-siv-409).
	createVolumeInStoreWithMeta(t, store, "vol-drift", viperblock.VolumeMetadata{
		VolumeID: "vol-drift",
		SizeGiB:  10,
		State:    "",
	})

	_, err := svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String("vol-drift"),
	}, "")
	require.NoError(t, err)
}

func TestDescribeVolumes_EmptyStateDerivedFromAttachment(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-empty-unattached", "", "")
	seedVolume(t, svc, "vol-empty-attached", "", "i-attached00000000")

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-empty-unattached"), aws.String("vol-empty-attached")},
	}, testVolAccountID)
	require.NoError(t, err)

	got := map[string]string{}
	for _, v := range out.Volumes {
		got[aws.StringValue(v.VolumeId)] = aws.StringValue(v.State)
	}
	assert.Equal(t, "available", got["vol-empty-unattached"], "empty state + no attachment renders available")
	assert.Equal(t, "in-use", got["vol-empty-attached"], "empty state + attachment must not be masked as available (mulga-siv-409)")
}

func TestUpdateVolumeState_EmptyUnattachedNormalizesToAvailable(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	seedVolume(t, svc, "vol-norm", "in-use", "i-x")

	// A detach writeback that clears the attachment without a state must not
	// strand the volume with an empty State (mulga-siv-409).
	require.NoError(t, svc.UpdateVolumeState("vol-norm", "", "", ""))
	cfg, err := svc.GetVolumeConfig("vol-norm")
	require.NoError(t, err)
	assert.Equal(t, "available", cfg.VolumeMetadata.State)
	assert.Empty(t, cfg.VolumeMetadata.AttachedInstance)
}

func TestDeleteVolume_WithNATSNotification(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()

	// Set up NATS server and connection for this test
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv
	svc.natsConn = nc

	volumeID := "vol-natsok"
	createVolumeInStoreWithMeta(t, store, volumeID, viperblock.VolumeMetadata{
		VolumeID: volumeID, SizeGiB: 10, State: "available",
	})

	// Subscribe to ebs.delete and reply with success
	sub, err := nc.Subscribe("ebs.delete", func(msg *nats.Msg) {
		resp := types.EBSDeleteResponse{Volume: volumeID, Success: true}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	_, err = svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.NoError(t, err)

	// Verify all objects deleted
	assert.Equal(t, 0, store.Count())
}

func TestDescribeVolumeStatus_SlowPath_WithVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	for _, id := range []string{"vol-s1", "vol-s2"} {
		createVolumeInStoreWithMeta(t, store, id, viperblock.VolumeMetadata{
			VolumeID:         id,
			SizeGiB:          10,
			State:            "available",
			AvailabilityZone: "ap-southeast-2a",
		})
	}

	output, err := svc.DescribeVolumeStatus(nil, "")
	require.NoError(t, err)
	assert.Len(t, output.VolumeStatuses, 2)

	for _, item := range output.VolumeStatuses {
		assert.Equal(t, "ok", *item.VolumeStatus.Status)
		assert.Equal(t, "ap-southeast-2a", *item.AvailabilityZone)
		assert.Len(t, item.VolumeStatus.Details, 2)
	}
}

func TestDescribeVolumeStatus_FastPath_WithVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-status1", viperblock.VolumeMetadata{
		VolumeID:         "vol-status1",
		SizeGiB:          10,
		State:            "in-use",
		AvailabilityZone: "ap-southeast-2a",
	})

	output, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-status1")},
	}, "")
	require.NoError(t, err)
	require.Len(t, output.VolumeStatuses, 1)
	assert.Equal(t, "vol-status1", *output.VolumeStatuses[0].VolumeId)
	assert.Equal(t, "ok", *output.VolumeStatuses[0].VolumeStatus.Status)
}

func TestDescribeVolumes_SlowPath_SkipsBrokenConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	// Good volume
	createVolumeInStoreWithMeta(t, store, "vol-good", viperblock.VolumeMetadata{
		VolumeID: "vol-good", SizeGiB: 10, State: "available",
	})
	// Bad volume: zero size triggers error in getVolumeByID
	createVolumeInStoreWithMeta(t, store, "vol-bad", viperblock.VolumeMetadata{
		VolumeID: "vol-bad", SizeGiB: 0,
	})

	output, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "")
	require.NoError(t, err)
	// Only the good volume should be returned
	assert.Len(t, output.Volumes, 1)
	assert.Equal(t, "vol-good", *output.Volumes[0].VolumeId)
}

func TestDescribeVolumeStatus_SlowPath_SkipsBrokenConfig(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-ok", viperblock.VolumeMetadata{
		VolumeID: "vol-ok", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, store, "vol-broken", viperblock.VolumeMetadata{
		VolumeID: "vol-broken", SizeGiB: 0,
	})

	output, err := svc.DescribeVolumeStatus(nil, "")
	require.NoError(t, err)
	assert.Len(t, output.VolumeStatuses, 1)
}

func TestDeleteVolume_NATSErrorResponse(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv
	svc.natsConn = nc

	volumeID := "vol-natserr"
	createVolumeInStoreWithMeta(t, store, volumeID, viperblock.VolumeMetadata{
		VolumeID: volumeID, SizeGiB: 10, State: "available",
	})

	// Subscribe and respond with an error
	sub, err := nc.Subscribe("ebs.delete", func(msg *nats.Msg) {
		resp := types.EBSDeleteResponse{Volume: volumeID, Error: "volume still mounted"}
		data, _ := json.Marshal(resp)
		msg.Respond(data)
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	_, err = svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDeleteVolume_NATSTimeout(t *testing.T) {
	kv := setupTestVolumeKV(t)
	store := objectstore.NewMemoryObjectStore()

	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })

	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	svc.snapshotKV = kv
	svc.natsConn = nc

	volumeID := "vol-natstimeout"
	createVolumeInStoreWithMeta(t, store, volumeID, viperblock.VolumeMetadata{
		VolumeID: volumeID, SizeGiB: 10, State: "available",
	})

	// No subscriber → NATS request will timeout, but delete proceeds (best-effort)
	_, err = svc.DeleteVolume(&ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, "")
	require.NoError(t, err)
}

// --- DescribeVolumes filter tests ---

func TestDescribeVolumes_FilterByStatus(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-avail", viperblock.VolumeMetadata{
		VolumeID: "vol-avail", SizeGiB: 10, State: "available", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-inuse", viperblock.VolumeMetadata{
		VolumeID: "vol-inuse", SizeGiB: 20, State: "in-use", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-avail", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByVolumeType(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-gp3", viperblock.VolumeMetadata{
		VolumeID: "vol-gp3", SizeGiB: 10, State: "available", VolumeType: "gp3", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-io1", viperblock.VolumeMetadata{
		VolumeID: "vol-io1", SizeGiB: 10, State: "available", VolumeType: "io1", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-type"), Values: []*string{aws.String("gp3")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-gp3", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterBySize(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-small", viperblock.VolumeMetadata{
		VolumeID: "vol-small", SizeGiB: 10, State: "available", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-big", viperblock.VolumeMetadata{
		VolumeID: "vol-big", SizeGiB: 100, State: "available", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("size"), Values: []*string{aws.String("100")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-big", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAttachmentInstanceId(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-att", viperblock.VolumeMetadata{
		VolumeID: "vol-att", SizeGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd0", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-free", viperblock.VolumeMetadata{
		VolumeID: "vol-free", SizeGiB: 10, State: "available", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []*string{aws.String("i-12345")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-att", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAttachmentDevice(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-nbd0", viperblock.VolumeMetadata{
		VolumeID: "vol-nbd0", SizeGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd0", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-nbd1", viperblock.VolumeMetadata{
		VolumeID: "vol-nbd1", SizeGiB: 10, State: "in-use",
		AttachedInstance: "i-12345", DeviceName: "/dev/nbd1", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.device"), Values: []*string{aws.String("/dev/nbd1")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-nbd1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByAZ(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-az1", viperblock.VolumeMetadata{
		VolumeID: "vol-az1", SizeGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2a", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-az2", viperblock.VolumeMetadata{
		VolumeID: "vol-az2", SizeGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2b", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-az1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterMultipleValues_OR(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-avail", viperblock.VolumeMetadata{
		VolumeID: "vol-avail", SizeGiB: 10, State: "available", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-inuse", viperblock.VolumeMetadata{
		VolumeID: "vol-inuse", SizeGiB: 10, State: "in-use",
		AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-del", viperblock.VolumeMetadata{
		VolumeID: "vol-del", SizeGiB: 10, State: "deleted", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available"), aws.String("in-use")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 2)
}

func TestDescribeVolumes_FilterMultipleFilters_AND(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-match", viperblock.VolumeMetadata{
		VolumeID: "vol-match", SizeGiB: 10, State: "available",
		VolumeType: "gp3", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-nomatch", viperblock.VolumeMetadata{
		VolumeID: "vol-nomatch", SizeGiB: 10, State: "in-use",
		VolumeType: "gp3", AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
			{Name: aws.String("volume-type"), Values: []*string{aws.String("gp3")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-match", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterUnknownName_Error(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("val")}},
		},
	}, "acct1")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeVolumes_FilterNoResults(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-one", viperblock.VolumeMetadata{
		VolumeID: "vol-one", SizeGiB: 10, State: "available", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("deleted")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Empty(t, out.Volumes)
}

func TestDescribeVolumes_FilterNoFilters(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-a", viperblock.VolumeMetadata{
		VolumeID: "vol-a", SizeGiB: 10, State: "available", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-b", viperblock.VolumeMetadata{
		VolumeID: "vol-b", SizeGiB: 20, State: "available", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 2)
}

func TestDescribeVolumes_FilterWildcard(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-az1", viperblock.VolumeMetadata{
		VolumeID: "vol-az1", SizeGiB: 10, State: "available",
		AvailabilityZone: "ap-southeast-2a", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-az2", viperblock.VolumeMetadata{
		VolumeID: "vol-az2", SizeGiB: 10, State: "available",
		AvailabilityZone: "us-east-1a", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-*")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-az1", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterByTag(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-tagged", viperblock.VolumeMetadata{
		VolumeID: "vol-tagged", SizeGiB: 10, State: "available", TenantID: "acct1",
		Tags: map[string]string{"Environment": "prod"},
	})
	createVolumeInStoreWithMeta(t, store, "vol-untagged", viperblock.VolumeMetadata{
		VolumeID: "vol-untagged", SizeGiB: 10, State: "available", TenantID: "acct1",
	})

	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Environment"), Values: []*string{aws.String("prod")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-tagged", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumes_FilterWithVolumeIds(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-a", viperblock.VolumeMetadata{
		VolumeID: "vol-a", SizeGiB: 10, State: "available", TenantID: "acct1",
	})
	createVolumeInStoreWithMeta(t, store, "vol-b", viperblock.VolumeMetadata{
		VolumeID: "vol-b", SizeGiB: 10, State: "in-use",
		AttachedInstance: "i-1", DeviceName: "/dev/nbd0", TenantID: "acct1",
	})

	// Request both by ID but filter to only available
	out, err := svc.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String("vol-a"), aws.String("vol-b")},
		Filters: []*ec2.Filter{
			{Name: aws.String("status"), Values: []*string{aws.String("available")}},
		},
	}, "acct1")
	require.NoError(t, err)
	assert.Len(t, out.Volumes, 1)
	assert.Equal(t, "vol-a", *out.Volumes[0].VolumeId)
}

func TestDescribeVolumeStatus_FilterByVolumeId(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vs1", viperblock.VolumeMetadata{
		VolumeID: "vol-vs1", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, store, "vol-vs2", viperblock.VolumeMetadata{
		VolumeID: "vol-vs2", SizeGiB: 20, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vs1")}},
		},
	}, "")
	require.NoError(t, err)
	require.Len(t, out.VolumeStatuses, 1)
	assert.Equal(t, "vol-vs1", *out.VolumeStatuses[0].VolumeId)
}

func TestDescribeVolumeStatus_FilterByStatus(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vss1", viperblock.VolumeMetadata{
		VolumeID: "vol-vss1", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	// Status is always "ok" in Spinifex
	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("ok")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	out, err = svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("impaired")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterByAZ(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vsaz", viperblock.VolumeMetadata{
		VolumeID: "vol-vsaz", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	out, err = svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("us-east-1a")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterMultipleValues_OR(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vsor1", viperblock.VolumeMetadata{
		VolumeID: "vol-vsor1", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, store, "vol-vsor2", viperblock.VolumeMetadata{
		VolumeID: "vol-vsor2", SizeGiB: 20, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, store, "vol-vsor3", viperblock.VolumeMetadata{
		VolumeID: "vol-vsor3", SizeGiB: 30, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsor1"), aws.String("vol-vsor3")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 2)
}

func TestDescribeVolumeStatus_FilterMultipleFilters_AND(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vsand", viperblock.VolumeMetadata{
		VolumeID: "vol-vsand", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	// Both match
	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsand")}},
			{Name: aws.String("volume-status.status"), Values: []*string{aws.String("ok")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)

	// Mismatch
	out, err = svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vsand")}},
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("us-east-1a")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterUnknownName_Error(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	_, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, "")
	assert.Error(t, err)
}

func TestDescribeVolumeStatus_FilterWildcard(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vswild", viperblock.VolumeMetadata{
		VolumeID: "vol-vswild", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-vswild*")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)
}

func TestDescribeVolumeStatus_FilterNoResults(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vsnr", viperblock.VolumeMetadata{
		VolumeID: "vol-vsnr", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})

	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-nonexistent")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Empty(t, out.VolumeStatuses)
}

func TestDescribeVolumeStatus_FilterWithVolumeIds(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-vsf1", viperblock.VolumeMetadata{
		VolumeID: "vol-vsf1", SizeGiB: 10, State: "available", AvailabilityZone: "ap-southeast-2a",
	})
	createVolumeInStoreWithMeta(t, store, "vol-vsf2", viperblock.VolumeMetadata{
		VolumeID: "vol-vsf2", SizeGiB: 20, State: "available", AvailabilityZone: "us-east-1a",
	})

	// Fast path with VolumeIds + filter: should apply filter to requested IDs
	out, err := svc.DescribeVolumeStatus(&ec2.DescribeVolumeStatusInput{
		VolumeIds: []*string{aws.String("vol-vsf1"), aws.String("vol-vsf2")},
		Filters: []*ec2.Filter{
			{Name: aws.String("availability-zone"), Values: []*string{aws.String("ap-southeast-2a")}},
		},
	}, "")
	require.NoError(t, err)
	assert.Len(t, out.VolumeStatuses, 1)
	assert.Equal(t, "vol-vsf1", *out.VolumeStatuses[0].VolumeId)
}

// --- Group: DescribeVolumesModifications tests ---

// TestDescribeVolumesModifications_RoundTrip proves ModifyVolume persists the
// modification record into cfg.Modification AND that DescribeVolumesModifications
// reads it back. Guards the load-bearing wiring between the two APIs.
func TestDescribeVolumesModifications_RoundTrip(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-rt", viperblock.VolumeMetadata{
		VolumeID: "vol-rt", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-rt"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	// Confirm Modification was persisted on cfg.
	cfg, err := svc.GetVolumeConfig("vol-rt")
	require.NoError(t, err)
	require.NotNil(t, cfg.Modification)
	assert.Equal(t, int64(10), cfg.Modification.OriginalSize)
	assert.Equal(t, int64(20), cfg.Modification.TargetSize)

	out, err := svc.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-rt")},
	}, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1)
	mod := out.VolumesModifications[0]
	assert.Equal(t, "vol-rt", *mod.VolumeId)
	assert.Equal(t, "completed", *mod.ModificationState)
	assert.Equal(t, int64(100), *mod.Progress)
	assert.Equal(t, int64(10), *mod.OriginalSize)
	assert.Equal(t, int64(20), *mod.TargetSize)
}

// TestDescribeVolumesModifications_OverwriteSemantics guards the single-record
// storage design: a second ModifyVolume must overwrite, never append. The
// second call's OriginalSize must equal the first call's TargetSize.
func TestDescribeVolumesModifications_OverwriteSemantics(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-ow", viperblock.VolumeMetadata{
		VolumeID: "vol-ow", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-ow"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	_, err = svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-ow"),
		Size:     aws.Int64(40),
	}, "111111111111")
	require.NoError(t, err)

	out, err := svc.DescribeVolumesModifications(nil, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1, "expected single record after two ModifyVolume calls")
	mod := out.VolumesModifications[0]
	assert.Equal(t, int64(20), *mod.OriginalSize, "second modification's OriginalSize must equal first's TargetSize")
	assert.Equal(t, int64(40), *mod.TargetSize)
}

// TestDescribeVolumesModifications_CrossTenantFastPath guards tenant isolation:
// querying another tenant's volume by explicit ID must return InvalidVolume.NotFound,
// not leak the modification record.
func TestDescribeVolumesModifications_CrossTenantFastPath(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-tenantA", viperblock.VolumeMetadata{
		VolumeID: "vol-tenantA", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-tenantA"),
		Size:     aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)

	_, err = svc.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-tenantA")},
	}, "222222222222")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDescribeVolumesModifications_SlowPathScoping covers the list-all path:
// unmodified volumes and cross-tenant volumes must both be silently omitted.
func TestDescribeVolumesModifications_SlowPathScoping(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-modA", viperblock.VolumeMetadata{
		VolumeID: "vol-modA", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-unmodA", viperblock.VolumeMetadata{
		VolumeID: "vol-unmodA", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-modB", viperblock.VolumeMetadata{
		VolumeID: "vol-modB", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "222222222222",
	})

	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modA"), Size: aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	_, err = svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-modB"), Size: aws.Int64(30),
	}, "222222222222")
	require.NoError(t, err)

	out, err := svc.DescribeVolumesModifications(nil, "111111111111")
	require.NoError(t, err)
	require.Len(t, out.VolumesModifications, 1)
	assert.Equal(t, "vol-modA", *out.VolumesModifications[0].VolumeId)
}

// TestDescribeVolumesModifications_FilterMatching exercises the filter switch
// for the field types whose comparison logic differs (string equality, numeric
// stringification, and the volume-id pre-filter on the slow path).
func TestDescribeVolumesModifications_FilterMatching(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	createVolumeInStoreWithMeta(t, store, "vol-fa", viperblock.VolumeMetadata{
		VolumeID: "vol-fa", SizeGiB: 10, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	createVolumeInStoreWithMeta(t, store, "vol-fb", viperblock.VolumeMetadata{
		VolumeID: "vol-fb", SizeGiB: 50, State: "available",
		VolumeType: "gp3", IOPS: 3000, TenantID: "111111111111",
	})
	_, err := svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-fa"), Size: aws.Int64(20),
	}, "111111111111")
	require.NoError(t, err)
	_, err = svc.ModifyVolume(&ec2.ModifyVolumeInput{
		VolumeId: aws.String("vol-fb"), Size: aws.Int64(100), VolumeType: aws.String("io1"), Iops: aws.Int64(8000),
	}, "111111111111")
	require.NoError(t, err)

	tests := []struct {
		name      string
		filter    *ec2.Filter
		wantIDs   []string
		wantEmpty bool
	}{
		{
			name:    "ByVolumeID",
			filter:  &ec2.Filter{Name: aws.String("volume-id"), Values: []*string{aws.String("vol-fa")}},
			wantIDs: []string{"vol-fa"},
		},
		{
			name:    "ByModificationState",
			filter:  &ec2.Filter{Name: aws.String("modification-state"), Values: []*string{aws.String("completed")}},
			wantIDs: []string{"vol-fa", "vol-fb"},
		},
		{
			name:      "ByModificationStateNoMatch",
			filter:    &ec2.Filter{Name: aws.String("modification-state"), Values: []*string{aws.String("failed")}},
			wantEmpty: true,
		},
		{
			name:    "ByTargetSizeNumeric",
			filter:  &ec2.Filter{Name: aws.String("target-size"), Values: []*string{aws.String("20")}},
			wantIDs: []string{"vol-fa"},
		},
		{
			name:    "ByTargetVolumeType",
			filter:  &ec2.Filter{Name: aws.String("target-volume-type"), Values: []*string{aws.String("io1")}},
			wantIDs: []string{"vol-fb"},
		},
		{
			name:      "TagFilterAlwaysEmpty",
			filter:    &ec2.Filter{Name: aws.String("tag:env"), Values: []*string{aws.String("prod")}},
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := svc.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
				Filters: []*ec2.Filter{tt.filter},
			}, "111111111111")
			require.NoError(t, err)
			if tt.wantEmpty {
				assert.Empty(t, out.VolumesModifications)
				return
			}
			got := make([]string, 0, len(out.VolumesModifications))
			for _, m := range out.VolumesModifications {
				got = append(got, *m.VolumeId)
			}
			assert.ElementsMatch(t, tt.wantIDs, got)
		})
	}
}

// TestDescribeVolumesModifications_UnknownVolumeIDFastPath covers the goroutine
// GetVolumeConfig-error branch and the caller's per-result error scan.
func TestDescribeVolumesModifications_UnknownVolumeIDFastPath(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
		VolumeIds: []*string{aws.String("vol-doesnotexist")},
	}, "111111111111")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidVolumeNotFound, err.Error())
}

// TestDescribeVolumesModifications_UnknownFilter proves the filter validator is
// wired up: an unknown filter name must fail with InvalidParameterValue.
func TestDescribeVolumesModifications_UnknownFilter(t *testing.T) {
	svc := newTestVolumeService("ap-southeast-2a")

	_, err := svc.DescribeVolumesModifications(&ec2.DescribeVolumesModificationsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("not-a-real-filter"), Values: []*string{aws.String("x")}},
		},
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
