package viperblockd

import (
	"bytes"
	"testing"

	awssdk "github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const listTestBucket = "test-bucket"

func putListObject(t *testing.T, store objectstore.ObjectStore, key string) {
	t.Helper()
	_, err := store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: awssdk.String(listTestBucket),
		Key:    awssdk.String(key),
		Body:   bytes.NewReader([]byte("{}")),
	})
	require.NoError(t, err)
}

// TestListVolumePrefixes_SkipsWhatIsNotAVolume locks what a live env19 run
// found: the bucket's top level holds more than volumes, and reporting a
// snapshot, the key store or the control plane's own metadata as a volume
// makes every one of them look like an orphan.
func TestListVolumePrefixes_SkipsWhatIsNotAVolume(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putListObject(t, store, "vol-0123456789abc/config.json")
	putListObject(t, store, "ami-dd7f063caded1ff82/config.json")
	putListObject(t, store, "snap-ami-dd7f063caded1ff82/config.json")
	putListObject(t, store, "keys/cluster.key")
	putListObject(t, store, "spinifex/ebsmetadata/v2/volumes/000000000001/vol-0123456789abc.json")

	ids, err := listVolumePrefixes(t.Context(), store, listTestBucket)
	require.NoError(t, err)

	assert.Equal(t, []string{"ami-dd7f063caded1ff82", "vol-0123456789abc"}, ids,
		"only volumes may be enumerated: a snapshot is a separate resource, and keys/ and spinifex/ are not volumes at all")
}

// An AMI's blocks are a real provider volume, so enumeration must report it.
// Excusing it belongs to the orphan report, which knows an AMI is documented
// as an image; the provider must not hide storage it genuinely holds.
func TestListVolumePrefixes_ReportsAMIBackingVolumes(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putListObject(t, store, "ami-dd7f063caded1ff82/config.json")

	ids, err := listVolumePrefixes(t.Context(), store, listTestBucket)
	require.NoError(t, err)
	assert.Equal(t, []string{"ami-dd7f063caded1ff82"}, ids)
}

func TestListVolumePrefixes_EmptyBucket(t *testing.T) {
	ids, err := listVolumePrefixes(t.Context(), objectstore.NewMemoryObjectStore(), listTestBucket)
	require.NoError(t, err)
	assert.Empty(t, ids)
}
