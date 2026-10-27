package image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeregisterImage_NoLegacyConfig covers every AMI created since the
// metadata store landed: they have no {imageID}/config.json, so deleting one
// answers NoSuchKey. Treating that as a failure returned InternalError after
// the AMI document was already gone, and a retrying caller then saw
// InvalidAMIID.NotFound for an image it had just been told it could not delete.
func TestDeregisterImage_NoLegacyConfig(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	svc := NewImageServiceImplWithStore(mem, testBucket)

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

// racingDeleteStore lets a concurrent deleter win every DeleteObject, so the
// caller's own delete answers NoSuchKey as Predastore would.
type racingDeleteStore struct {
	*objectstore.MemoryObjectStore
}

func (s racingDeleteStore) DeleteObject(ctx context.Context, input *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	_, _ = s.MemoryObjectStore.DeleteObject(ctx, input)
	return s.MemoryObjectStore.DeleteObject(ctx, input)
}

// Two deregisters that both pass the ownership read race to delete the
// document; the loser's NoSuchKey means the image is already gone.
func TestDeregisterImage_ConcurrentlyDeletedDocumentIsSuccess(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	svc := NewImageServiceImplWithStore(racingDeleteStore{MemoryObjectStore: mem}, testBucket)

	amiID := "ami-racing00001"
	createTestAMIConfigWithOwner(t, mem, amiID, "racing-deregister", testAccountID)

	_, err := svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)
}
