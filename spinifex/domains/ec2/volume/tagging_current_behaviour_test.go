package volume

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how the volume tag mirror writes
// the ebsmetadata document today. They describe current behaviour, not the
// target design, and a failure means the behaviour changed.

// interleavingStore runs between once, after the first read of key returns
// and before the caller can write, standing in for a writer on another node.
type interleavingStore struct {
	objectstore.ObjectStore

	key     string
	once    sync.Once
	between func()
}

func (s *interleavingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	out, err := s.ObjectStore.GetObject(ctx, input)
	if aws.StringValue(input.Key) == s.key {
		s.once.Do(s.between)
	}
	return out, err
}

// The object store offers no conditional put, so the volume mirror reads the
// document and writes it back unchecked. A state change and a second tag
// write that commit between that read and that write are both reverted.
func TestCurrentBehaviour_Tagging_VolumeMirrorRevertsInterleavedWrites(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	volumeID := "vol-interleavetag1"
	seedVolume(t, svc, volumeID, "available", "")

	interleaved := false
	other := ebsmetadata.NewStore(store, "test-bucket")
	svc.metadata = ebsmetadata.NewStore(&interleavingStore{
		ObjectStore: store,
		key:         metadataKey(t, volumeID),
		between: func() {
			interleaved = true
			meta, err := other.GetVolume(ctx, testVolAccountID, volumeID)
			require.NoError(t, err)
			meta.State = "in-use"
			meta.Tags = map[string]string{"b": "2"}
			require.NoError(t, other.PutVolume(ctx, meta))
		},
	}, "test-bucket")

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(volumeID)},
		Tags:      []*ec2.Tag{{Key: aws.String("a"), Value: aws.String("1")}},
	}, testVolAccountID))

	require.True(t, interleaved, "the concurrent write must land inside the mirror's read-then-put")
	final, err := other.GetVolume(ctx, testVolAccountID, volumeID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, final.Tags, "the interleaved tag b is lost")
	assert.Equal(t, "available", final.State, "the interleaved state change is reverted")
}
