package handlers_ec2_image

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "test-bucket"
const testAccountID = "000000000001"

// setupTestImageService creates an image service with in-memory storage for testing
func setupTestImageService(t *testing.T) (*ImageServiceImpl, *objectstore.MemoryObjectStore) {
	store := objectstore.NewMemoryObjectStore()
	svc := NewImageServiceImplWithStore(store, testBucket)
	return svc, store
}

// createTestVolumeConfig creates a test volume config in the mock store
func createTestVolumeConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, sizeGiB int) {
	volumeState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				SizeGiB: uint64(sizeGiB),
			},
		},
	}
	data, err := json.Marshal(volumeState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(volumeID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// createTestAMIConfig creates a test AMI config in the mock store
func createTestAMIConfig(t *testing.T, store *objectstore.MemoryObjectStore, imageID string) {
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         imageID,
				Name:            "test-ami",
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				Virtualization:  "hvm",
				RootDeviceType:  "ebs",
				VolumeSizeGiB:   8,
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(imageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// createTestAMIConfigWithName creates a test AMI config with a specified name.
// Owner defaults to testAccountID so the AMI is visible to the default caller —
// an empty ImageOwnerAlias would be filtered out as a corrupt config.
func createTestAMIConfigWithName(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name string) {
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         imageID,
				Name:            name,
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				Virtualization:  "hvm",
				RootDeviceType:  "ebs",
				VolumeSizeGiB:   8,
				ImageOwnerAlias: testAccountID,
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(imageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// createTestAMIConfigWithOwner creates a test AMI config with a specified name and owner
func createTestAMIConfigWithOwner(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner string) {
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         imageID,
				Name:            name,
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				Virtualization:  "hvm",
				RootDeviceType:  "ebs",
				VolumeSizeGiB:   8,
				ImageOwnerAlias: owner,
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(imageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

func TestCreateImageFromInstance_NilInput(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.CreateImageFromInstance(CreateImageParams{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestCreateImageFromInstance_RunningInstance_NoNATS(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create volume and AMI configs
	createTestVolumeConfig(t, store, "vol-root123", 10)
	createTestAMIConfig(t, store, "ami-source123")

	// Running instance without NATS should fail (natsConn is nil)
	_, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-test123"),
			Name:       aws.String("my-image"),
		},
		RootVolumeID:  "vol-root123",
		SourceImageID: "ami-source123",
		IsRunning:     true,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestCreateImageFromInstance_StoppedInstance_NoConfig(t *testing.T) {
	svc, _ := setupTestImageService(t)

	// Stopped instance without config will fail (no config to create viperblock)
	_, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-test123"),
			Name:       aws.String("my-image"),
		},
		RootVolumeID:  "vol-nonexistent",
		SourceImageID: "ami-source123",
		IsRunning:     false,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeImages_AfterCreate(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Manually create an AMI config with the caller as owner
	amiID := "ami-testimage123"
	createTestAMIConfigWithOwner(t, store, amiID, "test-ami", testAccountID)

	// Describe images should find it
	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Images, 1)
	assert.Equal(t, amiID, *result.Images[0].ImageId)
	assert.Equal(t, "test-ami", *result.Images[0].Name)
	assert.Equal(t, "x86_64", *result.Images[0].Architecture)
	assert.Equal(t, testAccountID, *result.Images[0].OwnerId)
}

func TestGetVolumeConfig(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestVolumeConfig(t, store, "vol-abc123", 20)

	cfg, err := svc.getVolumeConfig("vol-abc123")
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, uint64(20), cfg.VolumeMetadata.SizeGiB)
}

func TestGetVolumeConfig_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.getVolumeConfig("vol-nonexistent")
	require.Error(t, err)
}

func TestGetAMIConfig(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfig(t, store, "ami-abc123")

	meta, err := svc.GetAMIConfig("ami-abc123")
	require.NoError(t, err)
	assert.Equal(t, "ami-abc123", meta.ImageID)
	assert.Equal(t, "test-ami", meta.Name)
	assert.Equal(t, "x86_64", meta.Architecture)
	assert.Equal(t, "Linux/UNIX", meta.PlatformDetails)
	assert.Equal(t, "hvm", meta.Virtualization)
}

func TestGetAMIConfig_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.GetAMIConfig("ami-nonexistent")
	require.Error(t, err)
}

func TestPutSnapshotMetadata(t *testing.T) {
	svc, store := setupTestImageService(t)

	err := svc.putSnapshotMetadata("snap-abc123", "vol-xyz789", 10, testAccountID)
	require.NoError(t, err)

	// Verify the metadata was written correctly
	result, err := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("snap-abc123/metadata.json"),
	})
	require.NoError(t, err)
	defer result.Body.Close()

	var cfg handlers_ec2_snapshot.SnapshotConfig
	err = json.NewDecoder(result.Body).Decode(&cfg)
	require.NoError(t, err)
	assert.Equal(t, "snap-abc123", cfg.SnapshotID)
	assert.Equal(t, "vol-xyz789", cfg.VolumeID)
	assert.Equal(t, int64(10), cfg.VolumeSize)
	assert.Equal(t, "completed", cfg.State)
	assert.Equal(t, "100%", cfg.Progress)
	assert.Equal(t, testAccountID, cfg.OwnerID)
}

func TestCreateImageFromInstance_SourceAMIReadFailure(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create volume config but NOT the source AMI config
	createTestVolumeConfig(t, store, "vol-root123", 10)

	// With non-empty SourceImageID, missing AMI config should be a hard error
	_, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-test123"),
			Name:       aws.String("my-image"),
		},
		RootVolumeID:  "vol-root123",
		SourceImageID: "ami-nonexistent",
		IsRunning:     true, // will fail at snapshot step first (no NATS)
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeImages_NotFound(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create one AMI
	createTestAMIConfig(t, store, "ami-exists123")

	// Request a non-existent AMI ID — should return InvalidAMIID.NotFound
	_, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-nonexistent")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDescribeImages_MixedExistingAndMissing(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create one AMI
	createTestAMIConfig(t, store, "ami-exists123")

	// Request one existing + one non-existent — should return NotFound
	_, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{
			aws.String("ami-exists123"),
			aws.String("ami-missing456"),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestCreateImageFromInstance_DuplicateName(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create an existing AMI with name "my-image"
	createTestAMIConfigWithName(t, store, "ami-existing123", "my-image")

	// Create volume config for the snapshot step
	createTestVolumeConfig(t, store, "vol-root123", 10)

	// Attempt to create another image with the same name — should fail with duplicate error
	_, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-test123"),
			Name:       aws.String("my-image"),
		},
		RootVolumeID: "vol-root123",
		IsRunning:    true,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMINameDuplicate, err.Error())
}

func TestCreateImageFromInstance_UniqueNameAllowed(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create an existing AMI with a different name
	createTestAMIConfigWithName(t, store, "ami-existing123", "other-image")

	// Create volume config
	createTestVolumeConfig(t, store, "vol-root123", 10)

	// Creating with a unique name should NOT fail at the name check stage
	// (it will fail later at the snapshot step since natsConn is nil, but that's expected)
	_, err := svc.CreateImageFromInstance(CreateImageParams{
		Input: &ec2.CreateImageInput{
			InstanceId: aws.String("i-test123"),
			Name:       aws.String("unique-image"),
		},
		RootVolumeID: "vol-root123",
		IsRunning:    true,
	}, testAccountID)
	require.Error(t, err)
	// Should fail at snapshot, NOT at duplicate name check
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestPutAMIConfig_RoundTrip(t *testing.T) {
	svc, _ := setupTestImageService(t)

	meta := viperblock.AMIMetadata{
		ImageID:         "ami-roundtrip01",
		Name:            "round-trip",
		Description:     "hello",
		SnapshotID:      "snap-rt01",
		Architecture:    "x86_64",
		PlatformDetails: "Linux/UNIX",
		Virtualization:  "hvm",
		VolumeSizeGiB:   16,
		RootDeviceType:  "ebs",
		ImageOwnerAlias: testAccountID,
	}

	require.NoError(t, svc.putAMIConfig(meta.ImageID, meta))

	got, err := svc.GetAMIConfig(meta.ImageID)
	require.NoError(t, err)
	assert.Equal(t, meta.Name, got.Name)
	assert.Equal(t, meta.Description, got.Description)
	assert.Equal(t, meta.SnapshotID, got.SnapshotID)
	assert.Equal(t, meta.VolumeSizeGiB, got.VolumeSizeGiB)
	assert.Equal(t, meta.ImageOwnerAlias, got.ImageOwnerAlias)
}

func TestCheckAMIOwnership(t *testing.T) {
	svc, _ := setupTestImageService(t)

	caller := testAccountID
	other := "000000000002"

	tests := []struct {
		name    string
		owner   string
		wantErr string // "" = no error
	}{
		{"OwnedByCaller", caller, ""},
		{"OwnedByOtherAccount", other, awserrors.ErrorUnauthorizedOperation},
		{"SystemAMI", "spinifex", awserrors.ErrorUnauthorizedOperation},
		// Empty owner indicates corrupt config — callers must see the real
		// failure, not a misleading 403.
		{"EmptyOwnerIsCorrupt", "", awserrors.ErrorServerInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.checkAMIOwnership(viperblock.AMIMetadata{ImageOwnerAlias: tt.owner}, caller)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
			}
		})
	}
}

func TestDeregisterImage_HappyPath(t *testing.T) {
	svc, store := setupTestImageService(t)

	amiID := "ami-dereg001"
	createTestAMIConfigWithOwner(t, store, amiID, "ami-to-delete", testAccountID)

	out, err := svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, out)

	// AMI config gone from S3
	_, getErr := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(amiID + "/config.json"),
	})
	require.Error(t, getErr)
	assert.True(t, objectstore.IsNoSuchKeyError(getErr))

	// DescribeImages no longer returns it
	_, descErr := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	}, testAccountID)
	require.Error(t, descErr)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, descErr.Error())
}

func TestDeregisterImage_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.DeregisterImage(&ec2.DeregisterImageInput{
		ImageId: aws.String("ami-doesnotexist"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDeregisterImage_Idempotent(t *testing.T) {
	svc, store := setupTestImageService(t)

	amiID := "ami-idem001"
	createTestAMIConfigWithOwner(t, store, amiID, "idempotent", testAccountID)

	_, err := svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDeregisterImage_CrossAccount(t *testing.T) {
	svc, store := setupTestImageService(t)

	amiID := "ami-other001"
	createTestAMIConfigWithOwner(t, store, amiID, "other-acct", "000000000002")

	_, err := svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())

	// Confirm AMI still present after rejected mutation.
	_, getErr := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(amiID + "/config.json"),
	})
	require.NoError(t, getErr)
}

func TestDeregisterImage_SystemAMI(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, "ami-sys001", "system-ami", "spinifex")

	_, err := svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String("ami-sys001")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

func TestDeregisterImage_DoesNotTouchSnapshot(t *testing.T) {
	svc, store := setupTestImageService(t)

	amiID := "ami-keepsnap001"
	snapID := "snap-keep001"

	// AMI pointing at a snapshot
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         amiID,
				Name:            "with-snapshot",
				SnapshotID:      snapID,
				ImageOwnerAlias: testAccountID,
				Architecture:    "x86_64",
				Virtualization:  "hvm",
				RootDeviceType:  "ebs",
				VolumeSizeGiB:   8,
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(amiID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	// Backing snapshot metadata
	require.NoError(t, svc.putSnapshotMetadata(snapID, "vol-keep", 8, testAccountID))

	_, err = svc.DeregisterImage(&ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)

	// Snapshot metadata still present
	_, snapErr := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(snapID + "/metadata.json"),
	})
	require.NoError(t, snapErr)
}

func TestDescribeImages_AccountScoping(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create an AMI owned by a specific account
	createTestAMIConfigWithOwner(t, store, "ami-scoped123", "test-ami", "000000000001")

	// DescribeImages from the owning account should return the image with correct OwnerId
	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-scoped123")},
	}, "000000000001")
	require.NoError(t, err)
	require.Len(t, result.Images, 1)
	assert.Equal(t, "000000000001", *result.Images[0].OwnerId)

	// DescribeImages from a DIFFERENT account should NOT see the image
	_, err = svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-scoped123")},
	}, "000000000002")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDescribeImages_SystemAMIVisibleToAll(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create a system/pre-phase4 AMI (non-account-ID owner)
	createTestAMIConfigWithOwner(t, store, "ami-system123", "system-ami", "spinifex")

	// Any account should be able to see system AMIs
	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-system123")},
	}, "000000000001")
	require.NoError(t, err)
	require.Len(t, result.Images, 1)
	assert.Equal(t, "000000000000", *result.Images[0].OwnerId) // System AMIs report global account

	result2, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-system123")},
	}, "000000000002")
	require.NoError(t, err)
	require.Len(t, result2.Images, 1)
}

func TestDescribeImages_FilterSystemAMIByGlobalAccountID(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create a system AMI (non-account-ID owner like "spinifex")
	createTestAMIConfigWithOwner(t, store, "ami-sysfilter", "system-debian", "spinifex")

	// Filtering by GlobalAccountID ("000000000000") should match system AMIs
	// because that's the OwnerId returned in the response
	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Owners: []*string{aws.String("000000000000")},
	}, "000000000001")
	require.NoError(t, err)
	require.Len(t, result.Images, 1)
	assert.Equal(t, "ami-sysfilter", *result.Images[0].ImageId)
	assert.Equal(t, "000000000000", *result.Images[0].OwnerId)
}

func TestDescribeImages_NilInput(t *testing.T) {
	svc, _ := setupTestImageService(t)

	result, err := svc.DescribeImages(nil, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Images)
}

func TestDescribeImages_EmptyBucket(t *testing.T) {
	svc, _ := setupTestImageService(t)

	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Images)
}

func TestDescribeImages_NonAMIPrefixIgnored(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Create a non-AMI object (e.g. a volume config)
	createTestVolumeConfig(t, store, "vol-abc123", 10)

	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Images)
}

func TestDescribeImages_InvalidConfigJSON(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Store invalid JSON as an AMI config
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("ami-bad123/config.json"),
		Body:   strings.NewReader("not valid json"),
	})
	require.NoError(t, err)

	// Should skip the invalid AMI without error
	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Images)
}

func TestDescribeImages_EmptyImageIDSkipped(t *testing.T) {
	svc, store := setupTestImageService(t)

	// AMI config with empty ImageID
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID: "",
				Name:    "empty-id-ami",
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("ami-emptyid/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, result.Images)
}

func TestDescribeImages_WithTags(t *testing.T) {
	svc, store := setupTestImageService(t)

	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         "ami-tagged123",
				Name:            "tagged-ami",
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				Virtualization:  "hvm",
				RootDeviceType:  "ebs",
				VolumeSizeGiB:   8,
				ImageOwnerAlias: testAccountID,
				Tags:            map[string]string{"Environment": "test", "Name": "my-ami"},
			},
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("ami-tagged123/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String("ami-tagged123")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Images, 1)

	img := result.Images[0]
	assert.Len(t, img.Tags, 2)
	assert.NotNil(t, img.RootDeviceName)
	assert.Equal(t, "/dev/sda1", *img.RootDeviceName)
	assert.Len(t, img.BlockDeviceMappings, 1)
}

func TestDescribeImages_OwnerFilterNilEntry(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithOwner(t, store, "ami-test1", "test-ami", testAccountID)

	result, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Owners: []*string{nil, aws.String(testAccountID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, result.Images, 1)
}

func TestGetAMIConfig_InvalidJSON(t *testing.T) {
	svc, store := setupTestImageService(t)

	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("ami-badjson/config.json"),
		Body:   strings.NewReader("not json"),
	})
	require.NoError(t, err)

	_, err = svc.GetAMIConfig("ami-badjson")
	assert.Error(t, err)
}

func TestGetVolumeConfig_InvalidJSON(t *testing.T) {
	svc, store := setupTestImageService(t)

	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("vol-badjson/config.json"),
		Body:   strings.NewReader("{invalid"),
	})
	require.NoError(t, err)

	_, err = svc.getVolumeConfig("vol-badjson")
	assert.Error(t, err)
}

func TestAmiNameExists_NoAMIs(t *testing.T) {
	svc, _ := setupTestImageService(t)

	exists, err := svc.amiNameExists("nonexistent")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestAmiNameExists_Found(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithName(t, store, "ami-found123", "target-name")

	exists, err := svc.amiNameExists("target-name")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestAmiNameExists_NotFound(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithName(t, store, "ami-other123", "other-name")

	exists, err := svc.amiNameExists("different-name")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestAmiNameExists_InvalidJSON(t *testing.T) {
	svc, store := setupTestImageService(t)

	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("ami-bad/config.json"),
		Body:   strings.NewReader("not json"),
	})
	require.NoError(t, err)

	// A corrupt AMI config is a real store-side problem. Surface it rather
	// than silently under-counting names (which would let a caller write a
	// duplicate and mask the corruption).
	_, err = svc.amiNameExists("any-name")
	require.Error(t, err)
}

// --- DescribeImages filter tests ---

// createTestAMIConfigFull creates an AMI with all metadata fields for filter testing.
// Owner defaults to testAccountID when the caller leaves ImageOwnerAlias empty —
// an empty owner would be filtered out as corrupt.
func createTestAMIConfigFull(t *testing.T, store *objectstore.MemoryObjectStore, meta viperblock.AMIMetadata) {
	t.Helper()
	if meta.ImageOwnerAlias == "" {
		meta.ImageOwnerAlias = testAccountID
	}
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: meta,
		},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(meta.ImageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// TestDescribeImages_FilterBy verifies single-attribute filters. Each subtest
// builds its own AMI fixtures and asserts which IDs the filter selects.
// Multi-filter, wildcard, error-path, and no-filter cases live in their own
// dedicated tests below.
func TestDescribeImages_FilterBy(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, store *objectstore.MemoryObjectStore)
		input    *ec2.DescribeImagesInput
		wantIDs  []string // matched image IDs (order not asserted)
		wantNone bool     // expect zero results
	}{
		{
			name: "owner self excludes other owners",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithOwner(t, store, "ami-selfowned", "self-owned-ami", testAccountID)
				createTestAMIConfigWithOwner(t, store, "ami-system", "system-ami", "spinifex")
			},
			input:   &ec2.DescribeImagesInput{Owners: []*string{aws.String("self")}},
			wantIDs: []string{"ami-selfowned"},
		},
		{
			name: "explicit owner ID matches caller",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithOwner(t, store, "ami-owned1", "owned-ami", testAccountID)
			},
			input:   &ec2.DescribeImagesInput{Owners: []*string{aws.String(testAccountID)}},
			wantIDs: []string{"ami-owned1"},
		},
		{
			name: "explicit owner ID with wrong account returns empty",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithOwner(t, store, "ami-owned1", "owned-ami", testAccountID)
			},
			input:    &ec2.DescribeImagesInput{Owners: []*string{aws.String("999999999999")}},
			wantNone: true,
		},
		{
			name: "name",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithName(t, store, "ami-aaa", "debian-13")
				createTestAMIConfigWithName(t, store, "ami-bbb", "ubuntu-22")
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("name"), Values: []*string{aws.String("debian-13")}},
			}},
			wantIDs: []string{"ami-aaa"},
		},
		{
			name: "architecture",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-x86", Name: "x86-img", Architecture: "x86_64",
					RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-arm", Name: "arm-img", Architecture: "arm64",
					RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("architecture"), Values: []*string{aws.String("arm64")}},
			}},
			wantIDs: []string{"ami-arm"},
		},
		{
			name: "tag",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-tagged", Name: "tagged-img", Architecture: "x86_64",
					RootDeviceType: "ebs", VolumeSizeGiB: 8,
					Tags: map[string]string{"Environment": "prod"},
				})
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-untagged", Name: "untagged-img", Architecture: "x86_64",
					RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("tag:Environment"), Values: []*string{aws.String("prod")}},
			}},
			wantIDs: []string{"ami-tagged"},
		},
		{
			name: "state available",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithName(t, store, "ami-aaa", "test")
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("state"), Values: []*string{aws.String("available")}},
			}},
			wantIDs: []string{"ami-aaa"},
		},
		{
			name: "state deregistered returns empty",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigWithName(t, store, "ami-aaa", "test")
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("state"), Values: []*string{aws.String("deregistered")}},
			}},
			wantNone: true,
		},
		{
			name: "virtualization type",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-hvm", Name: "hvm-img", Architecture: "x86_64",
					Virtualization: "hvm", RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-pv", Name: "pv-img", Architecture: "x86_64",
					Virtualization: "paravirtual", RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("virtualization-type"), Values: []*string{aws.String("hvm")}},
			}},
			wantIDs: []string{"ami-hvm"},
		},
		{
			name: "root device type",
			setup: func(t *testing.T, store *objectstore.MemoryObjectStore) {
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-ebs", Name: "ebs-img", Architecture: "x86_64",
					Virtualization: "hvm", RootDeviceType: "ebs", VolumeSizeGiB: 8,
				})
				createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
					ImageID: "ami-is", Name: "is-img", Architecture: "x86_64",
					Virtualization: "hvm", RootDeviceType: "instance-store", VolumeSizeGiB: 8,
				})
			},
			input: &ec2.DescribeImagesInput{Filters: []*ec2.Filter{
				{Name: aws.String("root-device-type"), Values: []*string{aws.String("ebs")}},
			}},
			wantIDs: []string{"ami-ebs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, store := setupTestImageService(t)
			tt.setup(t, store)

			out, err := svc.DescribeImages(tt.input, testAccountID)
			require.NoError(t, err)
			if tt.wantNone {
				assert.Empty(t, out.Images)
				return
			}
			gotIDs := make([]string, len(out.Images))
			for i, img := range out.Images {
				gotIDs[i] = aws.StringValue(img.ImageId)
			}
			assert.ElementsMatch(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestDescribeImages_FilterMultipleValues_OR(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-aaa", "debian-13")
	createTestAMIConfigWithName(t, store, "ami-bbb", "ubuntu-22")
	createTestAMIConfigWithName(t, store, "ami-ccc", "centos-9")

	out, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String("debian-13"), aws.String("centos-9")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Images, 2)
}

func TestDescribeImages_FilterMultipleNames_AND(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
		ImageID: "ami-match", Name: "debian-13", Architecture: "x86_64",
		RootDeviceType: "ebs", VolumeSizeGiB: 8,
	})
	createTestAMIConfigFull(t, store, viperblock.AMIMetadata{
		ImageID: "ami-nomatch", Name: "debian-13", Architecture: "arm64",
		RootDeviceType: "ebs", VolumeSizeGiB: 8,
	})

	out, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String("debian-13")}},
			{Name: aws.String("architecture"), Values: []*string{aws.String("x86_64")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Images, 1)
	assert.Equal(t, "ami-match", *out.Images[0].ImageId)
}

func TestDescribeImages_FilterUnknownName_Error(t *testing.T) {
	svc, _ := setupTestImageService(t)
	_, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("val")}},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeImages_FilterWildcard(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-aaa", "prod-web-server")
	createTestAMIConfigWithName(t, store, "ami-bbb", "prod-api-server")
	createTestAMIConfigWithName(t, store, "ami-ccc", "dev-web-server")

	out, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String("prod-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Images, 2)
}

func TestDescribeImages_FilterNoResults(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-aaa", "debian-13")

	out, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("name"), Values: []*string{aws.String("nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Images)
}

func TestDescribeImages_FilterNoFilters(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-aaa", "debian-13")
	createTestAMIConfigWithName(t, store, "ami-bbb", "ubuntu-22")

	out, err := svc.DescribeImages(&ec2.DescribeImagesInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.Images, 2)
}

// --- RegisterImage tests ---

// putTestSnapshotConfig writes a SnapshotConfig at {snapshotID}/metadata.json,
// matching the layout that the snapshot service uses.
func putTestSnapshotConfig(t *testing.T, store *objectstore.MemoryObjectStore, snapshotID string, sizeGiB int64, ownerID string) {
	t.Helper()
	cfg := handlers_ec2_snapshot.SnapshotConfig{
		SnapshotID: snapshotID,
		VolumeID:   "vol-source-" + snapshotID,
		VolumeSize: sizeGiB,
		State:      "completed",
		Progress:   "100%",
		OwnerID:    ownerID,
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(snapshotID + "/metadata.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

func validRegisterImageServiceInput(snapshotID string) *ec2.RegisterImageInput {
	return &ec2.RegisterImageInput{
		Name:           aws.String("registered-ami"),
		RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					SnapshotId: aws.String(snapshotID),
				},
			},
		},
	}
}

func TestRegisterImage_HappyPath(t *testing.T) {
	svc, store := setupTestImageService(t)

	snapID := "snap-happy001"
	putTestSnapshotConfig(t, store, snapID, 8, testAccountID)

	out, err := svc.RegisterImage(validRegisterImageServiceInput(snapID), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.ImageId)
	assert.True(t, strings.HasPrefix(*out.ImageId, "ami-"))

	// AMI should be visible via DescribeImages with correct defaults.
	desc, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{out.ImageId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Images, 1)
	img := desc.Images[0]
	assert.Equal(t, "registered-ami", *img.Name)
	assert.Equal(t, "x86_64", *img.Architecture)
	assert.Equal(t, "hvm", *img.VirtualizationType)
	assert.Equal(t, "ebs", *img.RootDeviceType)
	assert.Equal(t, testAccountID, *img.OwnerId)
}

func TestRegisterImage_DuplicateName(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithOwner(t, store, "ami-existing01", "registered-ami", testAccountID)
	putTestSnapshotConfig(t, store, "snap-dup01", 8, testAccountID)

	_, err := svc.RegisterImage(validRegisterImageServiceInput("snap-dup01"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMINameDuplicate, err.Error())
}

func TestRegisterImage_SnapshotNotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.RegisterImage(validRegisterImageServiceInput("snap-missing"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, err.Error())
}

func TestRegisterImage_CrossAccountSnapshot(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-other01", 8, "000000000002")

	_, err := svc.RegisterImage(validRegisterImageServiceInput("snap-other01"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

func TestRegisterImage_SystemSnapshotAllowed(t *testing.T) {
	svc, store := setupTestImageService(t)

	// System-owned snapshot (non-account-ID owner) is launchable by anyone,
	// matching how system AMIs work.
	putTestSnapshotConfig(t, store, "snap-sys01", 8, "spinifex")

	out, err := svc.RegisterImage(validRegisterImageServiceInput("snap-sys01"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.ImageId)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, testAccountID, meta.ImageOwnerAlias)
}

func TestRegisterImage_ArchitectureAndVirtualizationDefaults(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-defaults", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-defaults")
	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "x86_64", meta.Architecture)
	assert.Equal(t, "hvm", meta.Virtualization)
	assert.Equal(t, "Linux/UNIX", meta.PlatformDetails)
	assert.Equal(t, "ebs", meta.RootDeviceType)
}

func TestRegisterImage_ExplicitArchitecture(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-arm64", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-arm64")
	input.Architecture = aws.String("arm64")
	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "arm64", meta.Architecture)
}

func TestRegisterImage_VolumeSizeSmallerThanSnapshotRejected(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-big", 20, testAccountID)

	input := validRegisterImageServiceInput("snap-big")
	input.BlockDeviceMappings[0].Ebs.VolumeSize = aws.Int64(8)

	_, err := svc.RegisterImage(input, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestRegisterImage_VolumeSizeLargerThanSnapshotHonoured(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-grow", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-grow")
	input.BlockDeviceMappings[0].Ebs.VolumeSize = aws.Int64(20)

	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, uint64(20), meta.VolumeSizeGiB)
}

func TestRegisterImage_VolumeSizeFromSnapshot(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-size", 16, testAccountID)

	out, err := svc.RegisterImage(validRegisterImageServiceInput("snap-size"), testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, uint64(16), meta.VolumeSizeGiB)
}

func TestRegisterImage_TagsPersisted(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-tags01", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-tags01")
	input.TagSpecifications = []*ec2.TagSpecification{
		{
			ResourceType: aws.String("image"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Env"), Value: aws.String("prod")},
				{Key: aws.String("Owner"), Value: aws.String("team-a")},
			},
		},
		{
			// Non-image resource type — tags must be ignored.
			ResourceType: aws.String("snapshot"),
			Tags: []*ec2.Tag{
				{Key: aws.String("ShouldNotAppear"), Value: aws.String("x")},
			},
		},
	}

	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "prod", meta.Tags["Env"])
	assert.Equal(t, "team-a", meta.Tags["Owner"])
	_, ok := meta.Tags["ShouldNotAppear"]
	assert.False(t, ok)
}

func TestRegisterImage_DescriptionPersisted(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-desc01", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-desc01")
	input.Description = aws.String("hand-built golden image")

	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "hand-built golden image", meta.Description)
}

func TestRegisterImage_RootDeviceNameSelectsBDM(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-data", 4, testAccountID)
	putTestSnapshotConfig(t, store, "snap-root", 8, testAccountID)

	input := &ec2.RegisterImageInput{
		Name:           aws.String("multi-bdm-ami"),
		RootDeviceName: aws.String("/dev/xvda"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvdb"),
				Ebs:        &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-data")},
			},
			{
				DeviceName: aws.String("/dev/xvda"),
				Ebs:        &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-root")},
			},
		},
	}

	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "snap-root", meta.SnapshotID)
	assert.Equal(t, uint64(8), meta.VolumeSizeGiB)
}

func TestRegisterImage_NilInput(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.RegisterImage(nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestRegisterImage_NoBlockDeviceMappings(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.RegisterImage(&ec2.RegisterImageInput{
		Name: aws.String("no-bdms"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestRegisterImage_BDMWithoutMatchingRootDevice(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-mismatch", 8, testAccountID)

	// Root device name doesn't match any BDM device name.
	_, err := svc.RegisterImage(&ec2.RegisterImageInput{
		Name:           aws.String("mismatched-root"),
		RootDeviceName: aws.String("/dev/xvda"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sdb"),
				Ebs:        &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-mismatch")},
			},
			nil,
			{Ebs: nil}, // skipped: no Ebs
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestRegisterImage_ExplicitVirtualizationType(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-virt", 8, testAccountID)

	input := validRegisterImageServiceInput("snap-virt")
	input.VirtualizationType = aws.String("hvm")

	out, err := svc.RegisterImage(input, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "hvm", meta.Virtualization)
}

func TestRegisterImage_NoRootDeviceNameUsesFirstSnapshotBDM(t *testing.T) {
	svc, store := setupTestImageService(t)

	putTestSnapshotConfig(t, store, "snap-first", 8, testAccountID)

	out, err := svc.RegisterImage(&ec2.RegisterImageInput{
		Name: aws.String("no-rootdevname"),
		// No RootDeviceName set; first BDM with a snapshot wins.
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{Ebs: nil}, // skipped
			{Ebs: &ec2.EbsBlockDevice{SnapshotId: aws.String("snap-first")}},
		},
	}, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "snap-first", meta.SnapshotID)
}

func TestRegisterImage_OwnerSetToCaller(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Snapshot owned by caller; resulting AMI must record caller as owner.
	putTestSnapshotConfig(t, store, "snap-own", 8, testAccountID)

	out, err := svc.RegisterImage(validRegisterImageServiceInput("snap-own"), testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, testAccountID, meta.ImageOwnerAlias)
	assert.False(t, meta.CreationDate.IsZero())
}

// --- CopyImage tests ---

// readAMIConfigBytes returns the raw bytes of {imageID}/config.json from the
// store so tests can prove the source was not mutated by a copy/modify/etc.
func readAMIConfigBytes(t *testing.T, store *objectstore.MemoryObjectStore, imageID string) []byte {
	t.Helper()
	result, err := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(imageID + "/config.json"),
	})
	require.NoError(t, err)
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	require.NoError(t, err)
	return data
}

// readSnapshotConfigBytes returns the raw bytes of {snapshotID}/metadata.json.
func readSnapshotConfigBytes(t *testing.T, store *objectstore.MemoryObjectStore, snapshotID string) []byte {
	t.Helper()
	result, err := store.GetObject(&awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(snapshotID + "/metadata.json"),
	})
	require.NoError(t, err)
	defer result.Body.Close()
	data, err := io.ReadAll(result.Body)
	require.NoError(t, err)
	return data
}

// putTestAMIConfigWithSnapshot seeds an AMI config that carries a real
// SnapshotID — distinct from createTestAMIConfigWithOwner which sets no
// snapshot and would be treated as orphaned by CopyImage.
func putTestAMIConfigWithSnapshot(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner, snapshotID string, meta viperblock.AMIMetadata) {
	t.Helper()
	meta.ImageID = imageID
	meta.Name = name
	meta.ImageOwnerAlias = owner
	meta.SnapshotID = snapshotID
	if meta.Architecture == "" {
		meta.Architecture = "x86_64"
	}
	if meta.PlatformDetails == "" {
		meta.PlatformDetails = "Linux/UNIX"
	}
	if meta.Virtualization == "" {
		meta.Virtualization = "hvm"
	}
	if meta.RootDeviceType == "" {
		meta.RootDeviceType = "ebs"
	}
	if meta.VolumeSizeGiB == 0 {
		meta.VolumeSizeGiB = 8
	}
	amiState := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{AMIMetadata: meta},
	}
	data, err := json.Marshal(amiState)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(imageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

// seedCopyableAMI writes a matching (snapshot, AMI) pair so CopyImage can
// complete end-to-end. Returns the AMI metadata that was persisted (callers
// can customise fields before seeding by tweaking the returned VolumeID /
// tags via preceding calls, but the helper takes the simple path for the
// happy case).
func seedCopyableAMI(t *testing.T, store *objectstore.MemoryObjectStore, imageID, name, owner, snapshotID, volumeID string, sizeGiB int64) {
	t.Helper()
	cfg := handlers_ec2_snapshot.SnapshotConfig{
		SnapshotID: snapshotID,
		VolumeID:   volumeID,
		VolumeSize: sizeGiB,
		State:      "completed",
		Progress:   "100%",
		OwnerID:    owner,
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(snapshotID + "/metadata.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)

	putTestAMIConfigWithSnapshot(t, store, imageID, name, owner, snapshotID, viperblock.AMIMetadata{
		VolumeSizeGiB: uint64(sizeGiB),
		Description:   "source desc",
	})
}

func validCopyImageServiceInput(sourceImageID, newName string) *ec2.CopyImageInput {
	return &ec2.CopyImageInput{
		Name:          aws.String(newName),
		SourceImageId: aws.String(sourceImageID),
		SourceRegion:  aws.String("ap-southeast-2"),
	}
}

func TestCopyImage_HappyPath(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-src001", "source-ami", testAccountID, "snap-src001", "vol-src001", 8)

	// Capture the source config bytes before the copy so we can prove the
	// source wasn't mutated (not just that name still resolves).
	srcBefore := readAMIConfigBytes(t, store, "ami-src001")
	srcSnapBefore := readSnapshotConfigBytes(t, store, "snap-src001")

	out, err := svc.CopyImage(validCopyImageServiceInput("ami-src001", "copy-of-source"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.ImageId)
	assert.True(t, strings.HasPrefix(*out.ImageId, "ami-"))
	assert.NotEqual(t, "ami-src001", *out.ImageId)

	// New AMI visible via DescribeImages, owned by caller.
	desc, err := svc.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{out.ImageId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Images, 1)
	img := desc.Images[0]
	assert.Equal(t, "copy-of-source", *img.Name)
	assert.Equal(t, testAccountID, *img.OwnerId)

	// Source AMI config and source snapshot metadata must be byte-identical.
	assert.Equal(t, srcBefore, readAMIConfigBytes(t, store, "ami-src001"),
		"source AMI config was mutated by CopyImage")
	assert.Equal(t, srcSnapBefore, readSnapshotConfigBytes(t, store, "snap-src001"),
		"source snapshot metadata was mutated by CopyImage")
}

func TestCopyImage_InheritsSourceFields(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Seed snapshot for the source AMI.
	_, err := store.PutObject(&awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String("snap-arm001/metadata.json"),
		Body: strings.NewReader(func() string {
			b, _ := json.Marshal(handlers_ec2_snapshot.SnapshotConfig{
				SnapshotID: "snap-arm001", VolumeID: "vol-arm", VolumeSize: 32,
				State: "completed", Progress: "100%", OwnerID: testAccountID,
			})
			return string(b)
		}()),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)

	// Source AMI with non-default fields that must propagate.
	putTestAMIConfigWithSnapshot(t, store, "ami-arm001", "arm-source", testAccountID, "snap-arm001", viperblock.AMIMetadata{
		Architecture:    "arm64",
		PlatformDetails: "Linux/UNIX (arm64)",
		Virtualization:  "hvm",
		VolumeSizeGiB:   32,
		RootDeviceType:  "ebs",
		Description:     "arm source",
	})

	before := time.Now()
	out, err := svc.CopyImage(validCopyImageServiceInput("ami-arm001", "arm-copy"), testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "arm64", newMeta.Architecture)
	assert.Equal(t, "Linux/UNIX (arm64)", newMeta.PlatformDetails)
	assert.Equal(t, "hvm", newMeta.Virtualization)
	assert.Equal(t, uint64(32), newMeta.VolumeSizeGiB)
	assert.Equal(t, "ebs", newMeta.RootDeviceType)
	assert.False(t, newMeta.CreationDate.Before(before), "CreationDate must be refreshed on copy, not inherited")
}

func TestCopyImage_NewSnapshotSharesSourceVolumeID(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-shareblocks", "shareblocks", testAccountID, "snap-orig", "vol-shared", 16)

	srcSnap, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, testBucket, "snap-orig")
	require.NoError(t, err)

	out, err := svc.CopyImage(validCopyImageServiceInput("ami-shareblocks", "shared-copy"), testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	require.NotEqual(t, "snap-orig", newMeta.SnapshotID)

	newSnap, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, testBucket, newMeta.SnapshotID)
	require.NoError(t, err)
	// Compare against the source snapshot, not hard-coded literals — proves
	// the new snap truly inherits the source's VolumeID rather than happening
	// to match a test fixture value.
	assert.Equal(t, srcSnap.VolumeID, newSnap.VolumeID,
		"new snapshot must share source's VolumeID (no block copy)")
	assert.Equal(t, srcSnap.VolumeSize, newSnap.VolumeSize)
	assert.Equal(t, testAccountID, newSnap.OwnerID)
}

func TestCopyImage_SystemAMICopiedIntoCallerAccount(t *testing.T) {
	svc, store := setupTestImageService(t)

	// System AMI (non-account-ID owner) with a snapshot also owned by system.
	seedCopyableAMI(t, store, "ami-system001", "debian-system", "spinifex", "snap-system001", "vol-sys", 8)

	out, err := svc.CopyImage(validCopyImageServiceInput("ami-system001", "my-debian"), testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, testAccountID, newMeta.ImageOwnerAlias)

	// Source unchanged — still owned by "spinifex".
	srcMeta, err := svc.GetAMIConfig("ami-system001")
	require.NoError(t, err)
	assert.Equal(t, "spinifex", srcMeta.ImageOwnerAlias)
}

// Bundled system AMIs (admin-imported) have no standalone snap-xxx/metadata.json
// — their SnapshotID refers to a viperblock-internal snap, and blocks live under
// ami-xxx/. CopyImage must still succeed (AWS parity: copy of a public AMI works),
// falling back to synthesizing a snap view where VolumeID = sourceImageID.
func TestCopyImage_BundledSystemAMINoStandaloneSnap(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Seed only the AMI config, no snap-xxx/metadata.json object — matches the
	// on-disk layout produced by `spx admin images import`.
	putTestAMIConfigWithSnapshot(t, store, "ami-bundled01", "alpine-bundled",
		"system", "snap-ami-bundled01", viperblock.AMIMetadata{
			VolumeSizeGiB: 8,
			Description:   "bundled source",
		})

	out, err := svc.CopyImage(validCopyImageServiceInput("ami-bundled01", "my-alpine"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.ImageId)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, testAccountID, newMeta.ImageOwnerAlias)
	assert.Equal(t, uint64(8), newMeta.VolumeSizeGiB)

	// New snap must exist in predastore, be owned by the caller, and point at
	// the bundled AMI's prefix (VolumeID = sourceImageID), not at the borrowed
	// snap-ami-bundled01 viperblock reference.
	require.NotEqual(t, "snap-ami-bundled01", newMeta.SnapshotID,
		"copy must mint a new user-owned snap id, not borrow the source's")
	newSnap, err := handlers_ec2_snapshot.ReadSnapshotConfig(store, testBucket, newMeta.SnapshotID)
	require.NoError(t, err)
	assert.Equal(t, "ami-bundled01", newSnap.VolumeID,
		"bundled fallback must point the new snap at the source AMI's prefix")
	assert.Equal(t, int64(8), newSnap.VolumeSize)
	assert.Equal(t, testAccountID, newSnap.OwnerID)
}

func TestCopyImage_CrossAccountHidesExistence(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-other001", "other-acct", "000000000002", "snap-other001", "vol-other", 8)

	srcBefore := readAMIConfigBytes(t, store, "ami-other001")
	srcSnapBefore := readSnapshotConfigBytes(t, store, "snap-other001")

	_, err := svc.CopyImage(validCopyImageServiceInput("ami-other001", "stolen-copy"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())

	// Rejection must not touch the source AMI config or its snapshot metadata.
	assert.Equal(t, srcBefore, readAMIConfigBytes(t, store, "ami-other001"),
		"source AMI config altered after rejected cross-account copy")
	assert.Equal(t, srcSnapBefore, readSnapshotConfigBytes(t, store, "snap-other001"),
		"source snapshot metadata altered after rejected cross-account copy")
}

func TestCopyImage_SourceNotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.CopyImage(validCopyImageServiceInput("ami-missing", "copy"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestCopyImage_OrphanedSource_MissingSnapshot(t *testing.T) {
	svc, store := setupTestImageService(t)

	// AMI config points at a snapshot that doesn't exist on S3.
	putTestAMIConfigWithSnapshot(t, store, "ami-orphan", "orphan", testAccountID, "snap-ghost", viperblock.AMIMetadata{})

	_, err := svc.CopyImage(validCopyImageServiceInput("ami-orphan", "orphan-copy"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestCopyImage_OrphanedSource_NoSnapshotID(t *testing.T) {
	svc, store := setupTestImageService(t)

	// Admin-imported bundled-storage AMI: no SnapshotID. Not copyable by this API.
	createTestAMIConfigWithOwner(t, store, "ami-bundled", "bundled", testAccountID)

	_, err := svc.CopyImage(validCopyImageServiceInput("ami-bundled", "bundled-copy"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestCopyImage_DuplicateName(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-dup-src", "source", testAccountID, "snap-dup-src", "vol-dup", 8)
	createTestAMIConfigWithOwner(t, store, "ami-collide", "already-taken", testAccountID)

	_, err := svc.CopyImage(validCopyImageServiceInput("ami-dup-src", "already-taken"), testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMINameDuplicate, err.Error())
}

func TestCopyImage_CopyImageTagsInheritsSourceTags(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-tagged-src", "tagged", testAccountID, "snap-tagged", "vol-tagged", 8)

	// Overlay source tags on the seeded AMI.
	srcMeta, err := svc.GetAMIConfig("ami-tagged-src")
	require.NoError(t, err)
	srcMeta.Tags = map[string]string{"Env": "prod", "Owner": "team-a"}
	require.NoError(t, svc.putAMIConfig("ami-tagged-src", srcMeta))

	input := validCopyImageServiceInput("ami-tagged-src", "copy-inherit-tags")
	input.CopyImageTags = aws.Bool(true)

	out, err := svc.CopyImage(input, testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "prod", newMeta.Tags["Env"])
	assert.Equal(t, "team-a", newMeta.Tags["Owner"])
}

func TestCopyImage_ExplicitTagsOverrideSourceTags(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-merge-src", "merge-src", testAccountID, "snap-merge", "vol-merge", 8)

	srcMeta, err := svc.GetAMIConfig("ami-merge-src")
	require.NoError(t, err)
	srcMeta.Tags = map[string]string{"Env": "prod", "Owner": "team-a"}
	require.NoError(t, svc.putAMIConfig("ami-merge-src", srcMeta))

	input := validCopyImageServiceInput("ami-merge-src", "merge-copy")
	input.CopyImageTags = aws.Bool(true)
	input.TagSpecifications = []*ec2.TagSpecification{
		{
			ResourceType: aws.String("image"),
			Tags: []*ec2.Tag{
				// Override a source tag and add a new one.
				{Key: aws.String("Env"), Value: aws.String("staging")},
				{Key: aws.String("Region"), Value: aws.String("apse2")},
			},
		},
		{
			// Non-image specs are ignored.
			ResourceType: aws.String("snapshot"),
			Tags: []*ec2.Tag{
				{Key: aws.String("ShouldNotAppear"), Value: aws.String("x")},
			},
		},
	}

	out, err := svc.CopyImage(input, testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "staging", newMeta.Tags["Env"])  // overridden
	assert.Equal(t, "team-a", newMeta.Tags["Owner"]) // inherited
	assert.Equal(t, "apse2", newMeta.Tags["Region"]) // added
	_, ok := newMeta.Tags["ShouldNotAppear"]
	assert.False(t, ok)
}

func TestCopyImage_CopyImageTagsFalseDropsSourceTags(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-drop-src", "drop-src", testAccountID, "snap-drop", "vol-drop", 8)

	srcMeta, err := svc.GetAMIConfig("ami-drop-src")
	require.NoError(t, err)
	srcMeta.Tags = map[string]string{"Env": "prod"}
	require.NoError(t, svc.putAMIConfig("ami-drop-src", srcMeta))

	input := validCopyImageServiceInput("ami-drop-src", "drop-copy")
	// CopyImageTags unset — default behaviour drops source tags.
	input.TagSpecifications = []*ec2.TagSpecification{
		{
			ResourceType: aws.String("image"),
			Tags: []*ec2.Tag{
				{Key: aws.String("New"), Value: aws.String("yes")},
			},
		},
	}

	out, err := svc.CopyImage(input, testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "yes", newMeta.Tags["New"])
	_, hasEnv := newMeta.Tags["Env"]
	assert.False(t, hasEnv)
}

func TestCopyImage_DescriptionInheritedWhenUnset(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-desc-src", "desc-src", testAccountID, "snap-desc", "vol-desc", 8)

	out, err := svc.CopyImage(validCopyImageServiceInput("ami-desc-src", "desc-inherit"), testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "source desc", newMeta.Description)
}

func TestCopyImage_DescriptionOverriddenWhenSet(t *testing.T) {
	svc, store := setupTestImageService(t)

	seedCopyableAMI(t, store, "ami-desc-ov", "desc-ov", testAccountID, "snap-ov", "vol-ov", 8)

	input := validCopyImageServiceInput("ami-desc-ov", "desc-override")
	input.Description = aws.String("explicit override")

	out, err := svc.CopyImage(input, testAccountID)
	require.NoError(t, err)

	newMeta, err := svc.GetAMIConfig(*out.ImageId)
	require.NoError(t, err)
	assert.Equal(t, "explicit override", newMeta.Description)
}

func TestCopyImage_MissingRequiredFields(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.CopyImage(nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.CopyImage(&ec2.CopyImageInput{
		SourceImageId: aws.String("ami-123"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.CopyImage(&ec2.CopyImageInput{
		Name: aws.String("only-name"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// --- Image attribute tests ---

func createTestAMIConfigRich(t *testing.T, store *objectstore.MemoryObjectStore, meta viperblock.AMIMetadata) {
	t.Helper()
	state := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{AMIMetadata: meta},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)

	_, err = store.PutObject(&awss3.PutObjectInput{
		Bucket:      aws.String(testBucket),
		Key:         aws.String(meta.ImageID + "/config.json"),
		Body:        strings.NewReader(string(data)),
		ContentType: aws.String("application/json"),
	})
	require.NoError(t, err)
}

func TestDescribeImageAttribute_Description(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-desc01",
		Name:            "desc-ami",
		Description:     "the stored description",
		Architecture:    "x86_64",
		PlatformDetails: "Linux/UNIX",
		Virtualization:  "hvm",
		RootDeviceType:  "ebs",
		VolumeSizeGiB:   8,
		ImageOwnerAlias: testAccountID,
	})

	out, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-desc01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.ImageId)
	assert.Equal(t, "ami-desc01", *out.ImageId)
	require.NotNil(t, out.Description)
	require.NotNil(t, out.Description.Value)
	assert.Equal(t, "the stored description", *out.Description.Value)
	assert.Nil(t, out.BlockDeviceMappings)
}

func TestDescribeImageAttribute_BlockDeviceMapping(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-bdm01",
		Name:            "bdm-ami",
		SnapshotID:      "snap-bdm01",
		Architecture:    "x86_64",
		PlatformDetails: "Linux/UNIX",
		Virtualization:  "hvm",
		RootDeviceType:  "ebs",
		VolumeSizeGiB:   20,
		ImageOwnerAlias: testAccountID,
	})

	out, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-bdm01"),
		Attribute: aws.String("blockDeviceMapping"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.BlockDeviceMappings, 1)
	bdm := out.BlockDeviceMappings[0]
	require.NotNil(t, bdm.DeviceName)
	assert.Equal(t, "/dev/sda1", *bdm.DeviceName)
	require.NotNil(t, bdm.Ebs)
	require.NotNil(t, bdm.Ebs.SnapshotId)
	assert.Equal(t, "snap-bdm01", *bdm.Ebs.SnapshotId)
	require.NotNil(t, bdm.Ebs.VolumeSize)
	assert.Equal(t, int64(20), *bdm.Ebs.VolumeSize)
	assert.Nil(t, out.Description)
}

func TestDescribeImageAttribute_MissingParameters(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.DescribeImageAttribute(nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId: aws.String("ami-xx"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		Attribute: aws.String("description"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeImageAttribute_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-missing"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDescribeImageAttribute_CrossAccountHidesExistence(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-cross01",
		Name:            "cross-ami",
		Description:     "secret",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: "000000000002",
	})

	_, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-cross01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestDescribeImageAttribute_SystemAMIReadable(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-sys01",
		Name:            "system-ami",
		Description:     "baked-in",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: "system",
	})

	out, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-sys01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Description)
	require.NotNil(t, out.Description.Value)
	assert.Equal(t, "baked-in", *out.Description.Value)
}

func TestDescribeImageAttribute_UnsupportedAttribute(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithOwner(t, store, "ami-unsup01", "u", testAccountID)

	for _, attr := range []string{"launchPermission", "bootMode", "kernel", "ramdisk"} {
		t.Run(attr, func(t *testing.T) {
			_, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
				ImageId:   aws.String("ami-unsup01"),
				Attribute: aws.String(attr),
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestModifyImageAttribute_Description(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-mod01",
		Name:            "mod-ami",
		Description:     "old",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: testAccountID,
	})

	_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId:   aws.String("ami-mod01"),
		Attribute: aws.String("description"),
		Value:     aws.String("new description"),
	}, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig("ami-mod01")
	require.NoError(t, err)
	assert.Equal(t, "new description", meta.Description)

	// Round-trips via DescribeImageAttribute.
	out, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-mod01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Description)
	require.NotNil(t, out.Description.Value)
	assert.Equal(t, "new description", *out.Description.Value)
}

func TestModifyImageAttribute_DescriptionEmptyValueClears(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-modclr01",
		Name:            "modclr-ami",
		Description:     "will-be-cleared",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: testAccountID,
	})

	_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId:   aws.String("ami-modclr01"),
		Attribute: aws.String("description"),
		Value:     aws.String(""),
	}, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig("ami-modclr01")
	require.NoError(t, err)
	assert.Equal(t, "", meta.Description)
}

func TestModifyImageAttribute_CrossAccount(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-modx01",
		Name:            "modx-ami",
		Description:     "dont-touch",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: "000000000002",
	})

	_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId:   aws.String("ami-modx01"),
		Attribute: aws.String("description"),
		Value:     aws.String("evil"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())

	// Value untouched on S3.
	meta, err := svc.GetAMIConfig("ami-modx01")
	require.NoError(t, err)
	assert.Equal(t, "dont-touch", meta.Description)
}

func TestModifyImageAttribute_SystemAMIImmutable(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-modsys01",
		Name:            "sys-ami",
		Description:     "baked-in",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: "system",
	})

	_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId:   aws.String("ami-modsys01"),
		Attribute: aws.String("description"),
		Value:     aws.String("tampered"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

func TestModifyImageAttribute_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId:   aws.String("ami-missing"),
		Attribute: aws.String("description"),
		Value:     aws.String("x"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestModifyImageAttribute_UnsupportedAttribute(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithOwner(t, store, "ami-modunsup01", "u", testAccountID)

	for _, attr := range []string{"bootMode", "launchPermission", "blockDeviceMapping"} {
		t.Run(attr, func(t *testing.T) {
			_, err := svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
				ImageId:   aws.String("ami-modunsup01"),
				Attribute: aws.String(attr),
				Value:     aws.String("x"),
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestModifyImageAttribute_MissingParameters(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.ModifyImageAttribute(nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.ModifyImageAttribute(&ec2.ModifyImageAttributeInput{
		ImageId: aws.String("ami-x"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestResetImageAttribute_Description(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-reset01",
		Name:            "reset-ami",
		Description:     "will-be-cleared",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: testAccountID,
	})

	_, err := svc.ResetImageAttribute(&ec2.ResetImageAttributeInput{
		ImageId:   aws.String("ami-reset01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig("ami-reset01")
	require.NoError(t, err)
	assert.Equal(t, "", meta.Description)

	out, err := svc.DescribeImageAttribute(&ec2.DescribeImageAttributeInput{
		ImageId:   aws.String("ami-reset01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Description)
	require.NotNil(t, out.Description.Value)
	assert.Equal(t, "", *out.Description.Value)
}

func TestResetImageAttribute_CrossAccount(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigRich(t, store, viperblock.AMIMetadata{
		ImageID:         "ami-rstx01",
		Name:            "rstx",
		Description:     "dont-touch",
		RootDeviceType:  "ebs",
		ImageOwnerAlias: "000000000002",
	})

	_, err := svc.ResetImageAttribute(&ec2.ResetImageAttributeInput{
		ImageId:   aws.String("ami-rstx01"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorUnauthorizedOperation, err.Error())

	meta, err := svc.GetAMIConfig("ami-rstx01")
	require.NoError(t, err)
	assert.Equal(t, "dont-touch", meta.Description)
}

func TestResetImageAttribute_NotFound(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.ResetImageAttribute(&ec2.ResetImageAttributeInput{
		ImageId:   aws.String("ami-missing"),
		Attribute: aws.String("description"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAMIIDNotFound, err.Error())
}

func TestResetImageAttribute_UnsupportedAttribute(t *testing.T) {
	svc, store := setupTestImageService(t)

	createTestAMIConfigWithOwner(t, store, "ami-rstunsup01", "u", testAccountID)

	for _, attr := range []string{"launchPermission", "bootMode", "blockDeviceMapping"} {
		t.Run(attr, func(t *testing.T) {
			_, err := svc.ResetImageAttribute(&ec2.ResetImageAttributeInput{
				ImageId:   aws.String("ami-rstunsup01"),
				Attribute: aws.String(attr),
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
		})
	}
}

func TestResetImageAttribute_MissingParameters(t *testing.T) {
	svc, _ := setupTestImageService(t)

	_, err := svc.ResetImageAttribute(nil, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = svc.ResetImageAttribute(&ec2.ResetImageAttributeInput{
		ImageId: aws.String("ami-x"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}
