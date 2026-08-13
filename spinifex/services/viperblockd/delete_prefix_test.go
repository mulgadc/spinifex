package viperblockd

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go/aws"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// racingDeleteStore answers every delete the way a backend does once a
// concurrent sweep has already removed the key.
type racingDeleteStore struct {
	objectstore.ObjectStore

	deletes int
	err     error
}

func (r *racingDeleteStore) DeleteObject(_ context.Context, _ *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	r.deletes++
	return nil, r.err
}

func newPrefixTestStore(t *testing.T, keys ...string) *objectstore.MemoryObjectStore {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	for _, key := range keys {
		_, err := store.PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket: awssdk.String("test-bucket"),
			Key:    awssdk.String(key),
			Body:   strings.NewReader("chunk"),
		})
		require.NoError(t, err)
	}
	return store
}

func TestDeleteObjectPrefixTreatsMissingObjectAsDeleted(t *testing.T) {
	backing := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000028.bin", "vol-abc/config.json")
	store := &racingDeleteStore{
		ObjectStore: backing,
		err:         &objectstore.NoSuchKeyError{Key: "vol-abc/chunks/chunk.00000028.bin"},
	}

	err := deleteObjectPrefix(context.Background(), store, "test-bucket", "vol-abc/")

	require.NoError(t, err, "an object another sweep already removed must not fail the delete")
	assert.Equal(t, 2, store.deletes, "the sweep must continue past a missing object")
}

func TestDeleteObjectPrefixPropagatesBackendFailure(t *testing.T) {
	backing := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000028.bin")
	store := &racingDeleteStore{ObjectStore: backing, err: errors.New("connection refused")}

	err := deleteObjectPrefix(context.Background(), store, "test-bucket", "vol-abc/")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestDeleteObjectPrefixRemovesEveryObject(t *testing.T) {
	store := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000000.bin", "vol-abc/config.json")

	require.NoError(t, deleteObjectPrefix(context.Background(), store, "test-bucket", "vol-abc/"))

	out, err := store.ListObjectsV2(context.Background(), &awss3.ListObjectsV2Input{
		Bucket: awssdk.String("test-bucket"),
		Prefix: awssdk.String("vol-abc/"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.Contents)
}
