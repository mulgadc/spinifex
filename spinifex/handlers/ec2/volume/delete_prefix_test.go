package handlers_ec2_volume

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPrefixTestConfig() *config.Config {
	return &config.Config{
		AZ: "ap-southeast-2a",
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			Region:    "ap-southeast-2",
			Host:      fakeS3Host,
			AccessKey: "testkey",
			SecretKey: "testsecret",
		},
		WalDir: "/tmp/test-wal",
	}
}

// racingDeleteStore answers every delete the way a backend does once a
// concurrent sweep has already removed the key.
type racingDeleteStore struct {
	objectstore.ObjectStore

	deletes int
	err     error
}

func (r *racingDeleteStore) DeleteObject(_ context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	r.deletes++
	return nil, r.err
}

func newPrefixTestStore(t *testing.T, keys ...string) *objectstore.MemoryObjectStore {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	for _, key := range keys {
		_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String("test-bucket"),
			Key:    aws.String(key),
			Body:   strings.NewReader("chunk"),
		})
		require.NoError(t, err)
	}
	return store
}

func TestDeleteS3PrefixTreatsMissingObjectAsDeleted(t *testing.T) {
	backing := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000028.bin", "vol-abc/config.json")
	store := &racingDeleteStore{
		ObjectStore: backing,
		err:         &objectstore.NoSuchKeyError{Key: "vol-abc/chunks/chunk.00000028.bin"},
	}
	svc := NewVolumeServiceImplWithStore(newPrefixTestConfig(), store, nil)

	err := svc.deleteS3Prefix(context.Background(), "vol-abc/")

	require.NoError(t, err, "an object another sweep already removed must not fail the delete")
	assert.Equal(t, 2, store.deletes, "the sweep must continue past a missing object")
}

func TestDeleteS3PrefixPropagatesBackendFailure(t *testing.T) {
	backing := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000028.bin")
	store := &racingDeleteStore{ObjectStore: backing, err: errors.New("connection refused")}
	svc := NewVolumeServiceImplWithStore(newPrefixTestConfig(), store, nil)

	err := svc.deleteS3Prefix(context.Background(), "vol-abc/")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestDeleteS3PrefixRemovesEveryObject(t *testing.T) {
	store := newPrefixTestStore(t, "vol-abc/chunks/chunk.00000000.bin", "vol-abc/config.json")
	svc := NewVolumeServiceImplWithStore(newPrefixTestConfig(), store, nil)

	require.NoError(t, svc.deleteS3Prefix(context.Background(), "vol-abc/"))

	out, err := store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"),
		Prefix: aws.String("vol-abc/"),
	})
	require.NoError(t, err)
	assert.Empty(t, out.Contents)
}
