package image

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how the AMI tag mirror writes
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

func (s *interleavingStore) GetObject(ctx context.Context, input *awss3.GetObjectInput) (*awss3.GetObjectOutput, error) {
	out, err := s.ObjectStore.GetObject(ctx, input)
	if aws.StringValue(input.Key) == s.key {
		s.once.Do(s.between)
	}
	return out, err
}

// The object store offers no conditional put, so the AMI mirror reads the
// document and writes it back unchecked. A non-tag change and a second tag
// write that commit between that read and that write are both reverted.
func TestCurrentBehaviour_Tagging_ImageMirrorRevertsInterleavedWrites(t *testing.T) {
	ctx := context.Background()
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-interleave", "original")

	interleaved := false
	other := ebsmetadata.NewStore(store, testBucket)
	svc.metadata = ebsmetadata.NewStore(&interleavingStore{
		ObjectStore: store,
		key:         amiDocumentKey(t, "ami-interleave"),
		between: func() {
			interleaved = true
			meta, err := other.GetAMI(ctx, "ami-interleave")
			require.NoError(t, err)
			meta.Description = "changed"
			meta.Tags = map[string]string{"b": "2"}
			require.NoError(t, other.PutAMI(ctx, meta))
		},
	}, testBucket)

	require.NoError(t, svc.ApplyRecordTags(createTagsInput("ami-interleave", map[string]string{"a": "1"}), testAccountID))

	require.True(t, interleaved, "the concurrent write must land inside the mirror's read-then-put")
	final, err := other.GetAMI(ctx, "ami-interleave")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, final.Tags, "the interleaved tag b is lost")
	assert.Empty(t, final.Description, "the interleaved description change is reverted")
}
