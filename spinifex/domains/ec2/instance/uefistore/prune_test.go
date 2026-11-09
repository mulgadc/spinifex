package uefistore_test

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// datedStore lists each object with the modification time the test set; a
// key it has none for is listed with none.
type datedStore struct {
	*objectstore.MemoryObjectStore

	modified map[string]time.Time
}

func (s *datedStore) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	out, err := s.MemoryObjectStore.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	for _, obj := range out.Contents {
		obj.LastModified = nil
		if at, ok := s.modified[aws.StringValue(obj.Key)]; ok {
			obj.LastModified = aws.Time(at)
		}
	}
	return out, nil
}

// A prune keeps the key it is told to and every object not older than the
// cutoff, counting those it kept for age, and an undated object counts as
// young (ADR-0003:S7).
func TestS7_PruneObjectsKeepsThePointerAndEveryYoungObject(t *testing.T) {
	const owner = "i-0123456789abcdef0"
	prefix := uefistore.KeyPrefix(owner)
	cutoff := time.Unix(1700000000, 0)
	keep, stale, young, undated, other := prefix+"keep", prefix+"stale", prefix+"young", prefix+"undated", uefistore.KeyPrefix("i-0fedcba9876543210")+"stale"
	store := &datedStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), modified: map[string]time.Time{
		keep:  cutoff.Add(-time.Hour),
		stale: cutoff.Add(-time.Second),
		young: cutoff,
		other: cutoff.Add(-time.Hour),
	}}
	for _, k := range []string{keep, stale, young, undated, other} {
		putObject(t, store, k)
	}

	res, err := uefistore.PruneObjects(t.Context(), store, bucket, owner, keep, cutoff)

	require.NoError(t, err)
	assert.Equal(t, uefistore.PruneResult{Deleted: 1, Young: 2}, res)
	assert.ElementsMatch(t, []string{keep, young, undated}, keysUnder(t, store, prefix),
		"ADR-0003:S7: a prune must keep the pointed object and every object younger than the cutoff")
	assert.Equal(t, []string{other}, keysUnder(t, store, uefistore.KeyPrefix("i-0fedcba9876543210")), "another owner's prefix is untouched")
}

// With no key to keep, or one outside the owner's prefix, a prune cannot tell
// the pointer from a leftover, so it deletes nothing (ADR-0003:S7).
func TestS7_PruneObjectsWithoutAKeyToKeepDeletesNothing(t *testing.T) {
	const owner = "i-0123456789abcdef0"
	prefix := uefistore.KeyPrefix(owner)
	store := &datedStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), modified: map[string]time.Time{
		prefix + "stale": time.Unix(1, 0),
	}}
	putObject(t, store, prefix+"stale")

	for _, keep := range []string{"", uefistore.KeyPrefix("i-0fedcba9876543210") + "stale", prefix + "nested/key"} {
		_, err := uefistore.PruneObjects(t.Context(), store, bucket, owner, keep, time.Now())
		require.Error(t, err, keep)
	}
	_, err := uefistore.PruneObjects(t.Context(), store, bucket, "i-*", prefix+"stale", time.Now())
	require.Error(t, err)

	assert.Equal(t, []string{prefix + "stale"}, keysUnder(t, store, prefix),
		"ADR-0003:S7: a prune with no valid pointer to keep must delete nothing")
}
