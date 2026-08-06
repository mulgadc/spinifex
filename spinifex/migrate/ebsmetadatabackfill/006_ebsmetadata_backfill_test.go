package ebsmetadatabackfill

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/volumestate"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "test-bucket"

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func testOctx(objects objectstore.ObjectStore) migrate.ObjectContext {
	return migrate.ObjectContext{Objects: objects, Bucket: testBucket, Logger: discardLogger()}
}

// putRaw writes body verbatim under key in the memory store.
func putRaw(t *testing.T, store objectstore.ObjectStore, key string, body []byte) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testBucket), Key: aws.String(key), Body: bytes.NewReader(body),
	})
	require.NoError(t, err)
}

// seedPlainVolume writes an unencrypted, unsealed config.json for volumeID.
func seedPlainVolume(t *testing.T, store objectstore.ObjectStore, volumeID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName: volumeID,
		VolumeSize: 10 * 1024 * 1024 * 1024,
		BlockSize:  4096,
		SeqNum:     3,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:         volumeID,
				TenantID:         "111122223333",
				SizeGiB:          10,
				State:            "available",
				AvailabilityZone: "ap-southeast-2a",
				VolumeType:       "gp3",
				CreatedAt:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Tags:             map[string]string{"Name": "from-config"},
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	putRaw(t, store, volumeID+"/config.json", data)
}

// seedEnvelopedVolume writes a config.json wrapped in the at-rest encryption
// envelope StateBody unwraps ({"payload": ..., "authtag": ...}).
func seedEnvelopedVolume(t *testing.T, store objectstore.ObjectStore, volumeID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeName:        volumeID,
		VolumeSize:        20 * 1024 * 1024 * 1024,
		BlockSize:         4096,
		SeqNum:            5,
		EncryptionEnabled: true,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID:         volumeID,
				TenantID:         "111122223333",
				SizeGiB:          20,
				State:            "available",
				AvailabilityZone: "ap-southeast-2a",
				VolumeType:       "gp3",
				CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	payload, err := json.Marshal(state)
	require.NoError(t, err)
	envelope := struct {
		Payload json.RawMessage `json:"payload"`
		AuthTag string          `json:"authtag"`
	}{Payload: payload, AuthTag: "test-authtag"}
	data, err := json.Marshal(envelope)
	require.NoError(t, err)
	putRaw(t, store, volumeID+"/config.json", data)
}

func seedAMI(t *testing.T, store objectstore.ObjectStore, imageID string) {
	t.Helper()
	state := viperblock.VBState{
		VolumeConfig: viperblock.VolumeConfig{
			AMIMetadata: viperblock.AMIMetadata{
				ImageID:         imageID,
				Name:            "debian-12-cloud",
				Architecture:    "x86_64",
				PlatformDetails: "Linux/UNIX",
				CreationDate:    time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
				RootDeviceType:  "ebs",
				Virtualization:  "hvm",
				VolumeSizeGiB:   8,
				Distro:          "debian",
				DistroFamily:    "debian",
				State:           "available",
				Tags:            map[string]string{"env": "test"},
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	putRaw(t, store, imageID+"/config.json", data)
}

func TestLegacyVolumeFromLegacyState_Plain(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-plain")

	volume, found, err := LegacyVolumeFromLegacyState(context.Background(), store, testBucket, "vol-plain")
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, "vol-plain", volume.VolumeID)
	assert.Equal(t, "111122223333", volume.TenantID)
	assert.Equal(t, uint64(10), volume.CapacityGiB)
	assert.Equal(t, "available", volume.State)
	assert.Equal(t, "gp3", volume.VolumeType)
	assert.False(t, volume.Encrypted)
	assert.Equal(t, map[string]string{"Name": "from-config"}, volume.Tags)
}

func TestLegacyVolumeFromLegacyState_EncryptionEnveloped(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedEnvelopedVolume(t, store, "vol-enc")

	volume, found, err := LegacyVolumeFromLegacyState(context.Background(), store, testBucket, "vol-enc")
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, "vol-enc", volume.VolumeID)
	assert.Equal(t, uint64(20), volume.CapacityGiB)
	assert.True(t, volume.Encrypted, "EncryptionEnabled inside the envelope must convert to Volume.Encrypted")
}

func TestLegacyVolumeFromLegacyState_OverlaysStateAndTagsPrecedence(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-overlay")

	// state.json is authoritative over config.json's (stale) State/attachment fields.
	require.NoError(t, volumestate.Write(context.Background(), store, testBucket, "vol-overlay", volumestate.Record{
		State: "in-use", AttachedInstance: "i-overlay", DeviceName: "/dev/nbd3",
		AttachedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
	}))
	// tags.json is authoritative over config.json's tags, including an empty map.
	tagsData, err := json.Marshal(map[string]string{"Name": "from-tags-json"})
	require.NoError(t, err)
	putRaw(t, store, volumeTagsKey("vol-overlay"), tagsData)

	volume, found, err := LegacyVolumeFromLegacyState(context.Background(), store, testBucket, "vol-overlay")
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, "in-use", volume.State)
	assert.Equal(t, "i-overlay", volume.AttachedInstance)
	assert.Equal(t, "/dev/nbd3", volume.DeviceName)
	assert.Equal(t, map[string]string{"Name": "from-tags-json"}, volume.Tags)
}

func TestLegacyVolumeFromLegacyState_NotFound(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	_, found, err := LegacyVolumeFromLegacyState(context.Background(), store, testBucket, "vol-missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestLegacyAMIFromLegacyState_Converts(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedAMI(t, store, "ami-test1")

	ami, found, err := LegacyAMIFromLegacyState(context.Background(), store, testBucket, "ami-test1")
	require.NoError(t, err)
	require.True(t, found)

	assert.Equal(t, "ami-test1", ami.ImageID)
	assert.Equal(t, "debian-12-cloud", ami.Name)
	assert.Equal(t, uint64(8), ami.VolumeSizeGiB)
	assert.Equal(t, map[string]string{"env": "test"}, ami.Tags)
}

func TestBackfill_ConvertsVolumesAndAMIs(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-a")
	seedEnvelopedVolume(t, store, "vol-b")
	seedAMI(t, store, "ami-a")

	octx := testOctx(store)
	require.NoError(t, backfill(context.Background(), octx))

	metaStore := ebsmetadata.NewStore(store, testBucket)
	got, err := metaStore.GetVolume(context.Background(), "vol-a")
	require.NoError(t, err)
	assert.Equal(t, "vol-a", got.VolumeID)

	got2, err := metaStore.GetVolume(context.Background(), "vol-b")
	require.NoError(t, err)
	assert.True(t, got2.Encrypted)

	gotAMI, err := metaStore.GetAMI(context.Background(), "ami-a")
	require.NoError(t, err)
	assert.Equal(t, "ami-a", gotAMI.ImageID)
}

func TestBackfill_SkipsInternalSubVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-main")
	seedPlainVolume(t, store, "vol-main-efi")
	seedPlainVolume(t, store, "vol-main-cloudinit")

	converted, skipped, err := backfillVolumes(context.Background(), testOctx(store))
	require.NoError(t, err)
	assert.Equal(t, 1, converted)
	assert.Equal(t, 0, skipped)
}

func TestBackfill_SkipsCorruptObjectAndConvertsRest(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-good")
	putRaw(t, store, "vol-bad/config.json", []byte("not json at all"))

	converted, skipped, err := backfillVolumes(context.Background(), testOctx(store))
	require.NoError(t, err)
	assert.Equal(t, 1, converted, "the readable volume must still convert")
	assert.Equal(t, 1, skipped, "the corrupt volume must be counted as skipped, not fail the run")

	metaStore := ebsmetadata.NewStore(store, testBucket)
	_, err = metaStore.GetVolume(context.Background(), "vol-good")
	assert.NoError(t, err)
}

func TestBackfill_Idempotent(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	seedPlainVolume(t, store, "vol-idem")
	seedAMI(t, store, "ami-idem")

	octx := testOctx(store)
	require.NoError(t, backfill(context.Background(), octx))

	volKey := ebsmetadataVolumeKeyForTest(t, "vol-idem")
	amiKey := ebsmetadataAMIKeyForTest(t, "ami-idem")

	firstVol := rawObject(t, store, volKey)
	firstAMI := rawObject(t, store, amiKey)

	require.NoError(t, backfill(context.Background(), octx))

	secondVol := rawObject(t, store, volKey)
	secondAMI := rawObject(t, store, amiKey)

	assert.Equal(t, string(firstVol), string(secondVol), "re-running the migration must overwrite with identical content")
	assert.Equal(t, string(firstAMI), string(secondAMI), "re-running the migration must overwrite with identical content")
}

func ebsmetadataVolumeKeyForTest(t *testing.T, volumeID string) string {
	t.Helper()
	key, err := ebsmetadata.VolumeKey(volumeID)
	require.NoError(t, err)
	return key
}

func ebsmetadataAMIKeyForTest(t *testing.T, imageID string) string {
	t.Helper()
	key, err := ebsmetadata.AMIKey(imageID)
	require.NoError(t, err)
	return key
}

func rawObject(t *testing.T, store objectstore.ObjectStore, key string) []byte {
	t.Helper()
	res, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket), Key: aws.String(key),
	})
	require.NoError(t, err)
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return data
}
