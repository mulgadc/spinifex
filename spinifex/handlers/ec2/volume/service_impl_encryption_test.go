package handlers_ec2_volume

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/migrate/ebsmetadatabackfill"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A legacy config.json that decodes as neither a VBState nor a config wrapper
// must surface as an error rather than resolving to an empty volume.
func TestGetVolumeMetadata_CorruptJSON(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)

	const volumeID = "vol-bad-json"
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader("not valid json {{{"),
	})
	require.NoError(t, err)

	_, err = svc.GetVolumeMetadata(volumeID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal config")
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
	svc := NewVolumeServiceImplWithStore(cfg, objectstore.NewMemoryObjectStore(), nil)
	svc.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	svc.metadata.SetLegacyVolumeFallback(ebsmetadatabackfill.LegacyVolumeFromLegacyState)
	return svc
}

func TestCreateVolume_EncryptionKeyLoadError(t *testing.T) {
	// Point EncryptionKeyFile at a non-existent path so LoadViperblockMasterKey
	// fails — CreateVolume must abort with ServerInternal and roll the
	// provider's allocation back rather than leave an orphaned volume.
	missing := filepath.Join(t.TempDir(), "absent.key")
	svc := newTestVolumeServiceWithEncryptionKey("ap-southeast-2a", missing)

	_, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size:             aws.Int64(1),
		AvailabilityZone: aws.String("ap-southeast-2a"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	out, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, "")
	require.NoError(t, err)
	assert.Empty(t, out.Volumes, "a failed create must not leave a volume behind")
}
