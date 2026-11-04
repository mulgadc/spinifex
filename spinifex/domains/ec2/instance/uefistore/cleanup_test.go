package uefistore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const bucket = "spinifex"

func putObject(t *testing.T, objects objectstore.ObjectStore, key string) {
	t.Helper()
	_, err := objects.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader([]byte("vars")),
	})
	require.NoError(t, err)
}

func keysUnder(t *testing.T, objects objectstore.ObjectStore, prefix string) []string {
	t.Helper()
	contents, _, err := objectstore.ListAll(t.Context(), objects, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	require.NoError(t, err)
	var keys []string
	for _, o := range contents {
		keys = append(keys, aws.StringValue(o.Key))
	}
	return keys
}

// Deletion is final: a writer holding the revision of a deleting record, such
// as a write-back retry, must not bring the store back to live.
func TestCompareAndSet_RefusesDeletingToLive(t *testing.T) {
	s, _ := openStore(t)
	rev, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	deleting := liveRecord()
	deleting.State = uefistore.StateDeleting
	rev, err = s.CompareAndSet(t.Context(), deleting, rev)
	require.NoError(t, err)

	revived := liveRecord()
	revived.WriteBack = writeBack()
	_, err = s.CompareAndSet(t.Context(), revived, rev)
	require.ErrorIs(t, err, uefistore.ErrDeleting)

	got, gotRev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, uefistore.StateDeleting, got.State)
}

// A write-back owed on a deleting store would put an object behind the delete.
func TestCompareAndSet_RefusesDeletingWithAWriteBack(t *testing.T) {
	s, _ := openStore(t)
	rev, err := s.Create(t.Context(), liveRecord())
	require.NoError(t, err)
	rec := liveRecord()
	rec.State = uefistore.StateDeleting
	rec.WriteBack = writeBack()

	_, err = s.CompareAndSet(t.Context(), rec, rev)
	require.ErrorIs(t, err, uefistore.ErrInvalidRecord)
}

func TestMarkDeleting_MovesLiveToDeletingAndDropsTheWriteBack(t *testing.T) {
	s, _ := openStore(t)
	rec := liveRecord()
	rec.WriteBack = writeBack()
	_, err := s.Create(t.Context(), rec)
	require.NoError(t, err)

	has, err := s.MarkDeleting(t.Context(), instanceID)
	require.NoError(t, err)
	assert.True(t, has)

	got, rev, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, uefistore.StateDeleting, got.State)
	assert.Nil(t, got.WriteBack)
	assert.Equal(t, "111122223333", got.AccountID)

	has, err = s.MarkDeleting(t.Context(), instanceID)
	require.NoError(t, err)
	assert.True(t, has, "a repeated terminate finds the obligation already recorded")
	_, again, err := s.Get(t.Context(), instanceID)
	require.NoError(t, err)
	assert.Equal(t, rev, again, "a repeat writes nothing")
}

func TestMarkDeleting_NoRecordIsNoStore(t *testing.T) {
	s, _ := openStore(t)
	has, err := s.MarkDeleting(t.Context(), instanceID)
	require.NoError(t, err)
	assert.False(t, has)
	_, _, err = s.Get(t.Context(), instanceID)
	require.ErrorIs(t, err, kvstore.ErrNotFound, "marking must not create a record")
}

// A record this node cannot trust is reported, never rewritten as deleting.
func TestMarkDeleting_RefusesAnUntrustedRecord(t *testing.T) {
	s, js := openStore(t)
	rawPut(t, js, instanceID, `{"schema_version":2,"instance_id":"i-0123456789abcdef0","state":"live"}`)

	_, err := s.MarkDeleting(t.Context(), instanceID)
	require.ErrorIs(t, err, uefistore.ErrInvalidRecord)
}

func TestExists_CountsAnUndecodableRecord(t *testing.T) {
	s, js := openStore(t)
	has, err := s.Exists(t.Context(), instanceID)
	require.NoError(t, err)
	assert.False(t, has)

	rawPut(t, js, instanceID, `not json`)
	has, err = s.Exists(t.Context(), instanceID)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestInstanceIDs_ListsRecordsButNotTheVersionKey(t *testing.T) {
	s, _ := openStore(t)
	ids, err := s.InstanceIDs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, ids)

	for _, id := range []string{"i-0000000000000000b", "i-0000000000000000a"} {
		rec := liveRecord()
		rec.InstanceID = id
		_, err := s.Create(t.Context(), rec)
		require.NoError(t, err)
	}
	ids, err = s.InstanceIDs(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"i-0000000000000000a", "i-0000000000000000b"}, ids)
}

// The prefix carries its trailing slash, so an instance whose ID extends
// another's keeps its objects.
func TestDeleteObjects_RemovesOnlyTheOwnersPrefix(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	const other = instanceID + "1"
	for _, key := range []string{"a", "b", "c"} {
		putObject(t, objects, uefistore.KeyPrefix(instanceID)+key)
	}
	putObject(t, objects, uefistore.KeyPrefix(other)+"a")
	putObject(t, objects, uefistore.KeyPrefix("ami-0123456789abcdef0")+"a")

	deleted, err := uefistore.DeleteObjects(t.Context(), objects, bucket, instanceID)
	require.NoError(t, err)
	assert.Equal(t, 3, deleted)
	assert.Empty(t, keysUnder(t, objects, uefistore.KeyPrefix(instanceID)))
	assert.Len(t, keysUnder(t, objects, uefistore.KeyPrefix(other)), 1)
	assert.Len(t, keysUnder(t, objects, uefistore.KeyPrefix("ami-0123456789abcdef0")), 1)

	deleted, err = uefistore.DeleteObjects(t.Context(), objects, bucket, instanceID)
	require.NoError(t, err, "an empty prefix is already deleted")
	assert.Zero(t, deleted)
}

func TestDeleteObjects_RefusesAnIDThatWidensTheDelete(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	putObject(t, objects, uefistore.KeyPrefix(instanceID)+"a")
	for _, id := range []string{"", "i-", "ami-", "vol-0123", "i-0/../", "i-*", "x"} {
		_, err := uefistore.DeleteObjects(t.Context(), objects, bucket, id)
		require.Errorf(t, err, "owner %q", id)
	}
	assert.Len(t, keysUnder(t, objects, uefistore.ObjectRoot), 1)
}

// failingDeletes refuses every delete, as predastore does while it is down.
type failingDeletes struct {
	objectstore.ObjectStore
}

func (failingDeletes) DeleteObject(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	return nil, errors.New("connection refused")
}

func TestDeleteObjects_ReportsAFailedDelete(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	putObject(t, mem, uefistore.KeyPrefix(instanceID)+"a")

	_, err := uefistore.DeleteObjects(t.Context(), failingDeletes{mem}, bucket, instanceID)
	require.Error(t, err)
	assert.Len(t, keysUnder(t, mem, uefistore.KeyPrefix(instanceID)), 1)
}

// noopDeletes reports success without deleting, so the closing listing is
// what notices the prefix is not empty.
type noopDeletes struct {
	objectstore.ObjectStore
}

func (noopDeletes) DeleteObject(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}

func TestDeleteObjects_FailsWhileAnObjectRemains(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	putObject(t, mem, uefistore.KeyPrefix(instanceID)+"a")

	_, err := uefistore.DeleteObjects(t.Context(), noopDeletes{mem}, bucket, instanceID)
	require.Error(t, err)
}
