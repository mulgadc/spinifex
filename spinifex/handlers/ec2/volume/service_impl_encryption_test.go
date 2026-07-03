package handlers_ec2_volume

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetVolumeConfig_CorruptJSON(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	const volumeID = "vol-bad-json"
	// Body is not a valid VBState (BlockSize stays 0 → falls through) and is
	// not a valid volumeConfigWrapper either (unmarshal fails) — must surface
	// "failed to unmarshal config" from getVolumeConfigAndEncryption.
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader("not valid json {{{"),
	})
	require.NoError(t, err)

	_, err = svc.GetVolumeConfig(volumeID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal config")
}

func TestMergeVolumeConfig_RefusesEncryptedVBState(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	const volumeID = "vol-encrypted"
	const configKey = volumeID + "/config.json"

	// Seed an encrypted-at-rest VBState. mergeVolumeConfig must refuse to
	// re-marshal it because spinifex does not currently hold the master key
	// and a tag-less rewrite would brick the volume.
	state := viperblock.VBState{
		VolumeName:        volumeID,
		VolumeSize:        1024 * 1024 * 1024,
		BlockSize:         4096,
		EncryptionEnabled: true,
		VolumeConfig: viperblock.VolumeConfig{
			VolumeMetadata: viperblock.VolumeMetadata{
				VolumeID: volumeID,
				SizeGiB:  1,
			},
		},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(configKey),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)

	_, err = svc.mergeVolumeConfig(configKey, &viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{VolumeID: volumeID, SizeGiB: 2},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to merge encrypted VBState")
}

// newTestVolumeServiceWithEncryptionKey wires CreateVolume to a configured
// (but unreadable) EncryptionKeyFile so the master-key load error path is hit.
func newTestVolumeServiceWithEncryptionKey(az, keyFile string) *VolumeServiceImpl {
	cfg := &config.Config{
		AZ: az,
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			Region:    "ap-southeast-2",
			Host:      "localhost:9000",
			AccessKey: "testkey",
			SecretKey: "testsecret",
		},
		Viperblock: config.ViperblockConfig{EncryptionKeyFile: keyFile},
		WalDir:     "/tmp/test-wal",
	}
	return NewVolumeServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nil)
}

func TestCreateVolume_EncryptionKeyLoadError(t *testing.T) {
	// Point EncryptionKeyFile at a non-existent path so LoadViperblockMasterKey
	// fails — CreateVolume must abort with ServerInternal, before any backend
	// init / SaveState call.
	missing := filepath.Join(t.TempDir(), "absent.key")
	svc := newTestVolumeServiceWithEncryptionKey("ap-southeast-2a", missing)

	_, err := svc.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(1),
		AvailabilityZone: aws.String("ap-southeast-2a"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
