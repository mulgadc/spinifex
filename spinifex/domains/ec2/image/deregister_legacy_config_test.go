package image

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeregisterImage_NoLegacyConfig: deregistering deletes the AMI document
// and nothing under {imageID}/, where a system AMI's image volume keeps its
// own config.json under the same ID.
func TestDeregisterImage_NoLegacyConfig(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	svc := NewImageServiceImplWithStore(mem, testBucket)

	amiID := "ami-nolegacy001"
	createTestAMIConfigWithOwner(t, mem, amiID, "no-legacy-config", testAccountID)
	volumeConfigKey := amiID + "/config.json"
	_, err := mem.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(volumeConfigKey),
		Body:   bytes.NewReader([]byte(`{"volume":"state"}`)),
	})
	require.NoError(t, err)

	out, err := svc.DeregisterImage(context.Background(), &ec2.DeregisterImageInput{ImageId: aws.String(amiID)}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, out)

	_, getErr := mem.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(amiDocumentKey(t, amiID)),
	})
	require.Error(t, getErr)
	assert.True(t, objectstore.IsNoSuchKeyError(getErr), "the AMI document must be deleted")

	kept, err := mem.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(volumeConfigKey),
	})
	require.NoError(t, err, "the object under {imageID}/ must be left alone")
	body, err := io.ReadAll(kept.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"volume":"state"}`, string(body))
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
