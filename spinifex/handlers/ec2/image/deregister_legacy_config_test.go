package handlers_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strictDeleteStore answers a delete of an absent key with NoSuchKey, which is
// what Predastore does. MemoryObjectStore silently succeeds instead, so a
// handler that treats a missing key as a failure passes every test and still
// breaks in production.
type strictDeleteStore struct {
	*objectstore.MemoryObjectStore
}

func (s strictDeleteStore) DeleteObject(ctx context.Context, input *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	if _, err := s.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: input.Bucket, Key: input.Key}); err != nil {
		return nil, err
	}
	return s.MemoryObjectStore.DeleteObject(ctx, input)
}

// TestDeregisterImage_NoLegacyConfig covers every AMI created since the
// metadata store landed: they have no {imageID}/config.json, so deleting one
// answers NoSuchKey. Treating that as a failure returned InternalError after
// the AMI document was already gone, and a retrying caller then saw
// InvalidAMIID.NotFound for an image it had just been told it could not delete.
func TestDeregisterImage_NoLegacyConfig(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	svc := NewImageServiceImplWithStore(strictDeleteStore{MemoryObjectStore: mem}, testBucket)

	amiID := "ami-nolegacy001"
	createTestAMIConfigWithOwner(t, mem, amiID, "no-legacy-config", testAccountID)

	out, err := svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, out)

	_, getErr := mem.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(amiDocumentKey(t, amiID)),
	})
	require.Error(t, getErr)
	assert.True(t, objectstore.IsNoSuchKeyError(getErr), "the AMI document must still be deleted")
}
