package uefistore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clockedStore lists each object at the time the test wrote it, so which
// objects are the newest does not depend on the wall clock's resolution.
type clockedStore struct {
	*objectstore.MemoryObjectStore

	written   map[string]time.Time
	listErr   error
	deleteErr map[string]error
	deleted   []string
}

func newClockedStore() *clockedStore {
	return &clockedStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), written: map[string]time.Time{}, deleteErr: map[string]error{}}
}

func (c *clockedStore) putAt(t *testing.T, key string, at time.Time) {
	t.Helper()
	_, err := c.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(testBucket), Key: aws.String(key), Body: bytes.NewReader([]byte("x"))})
	require.NoError(t, err)
	c.written[key] = at
}

func (c *clockedStore) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	out, err := c.MemoryObjectStore.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	for _, o := range out.Contents {
		o.LastModified = aws.Time(c.written[aws.StringValue(o.Key)])
	}
	return out, nil
}

func (c *clockedStore) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if err := c.deleteErr[aws.StringValue(in.Key)]; err != nil {
		return nil, err
	}
	c.deleted = append(c.deleted, aws.StringValue(in.Key))
	return c.MemoryObjectStore.DeleteObject(ctx, in)
}

func (c *clockedStore) keys(t *testing.T, owner string) []string {
	t.Helper()
	listed, _, err := objectstore.ListAll(t.Context(), c.MemoryObjectStore, &s3.ListObjectsV2Input{Bucket: aws.String(testBucket), Prefix: aws.String(uefistore.KeyPrefix(owner))})
	require.NoError(t, err)
	var keys []string
	for _, o := range listed {
		keys = append(keys, aws.StringValue(o.Key))
	}
	return keys
}

var t0 = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

func version(n int) string { return uefistore.KeyPrefix(instanceID) + fmt.Sprintf("v%02d", n) }

// The three newest versions and the pointed one are kept; everything else
// under the owner's prefix goes.
func TestPrune_KeepsTheThreeNewestAndThePointedVersion(t *testing.T) {
	store := newClockedStore()
	for n := 1; n <= 6; n++ {
		store.putAt(t, version(n), t0.Add(time.Duration(n)*time.Minute))
	}

	deleted, err := uefistore.Prune(t.Context(), store, testBucket, instanceID, version(1))

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{version(2), version(3)}, deleted)
	assert.ElementsMatch(t, []string{version(1), version(4), version(5), version(6)}, store.keys(t, instanceID))
}

// pruneS7Clause is quoted by every invariant failure below. Fix Prune, not the
// assertion.
const pruneS7Clause = "ADR-0003:S7: a reaper's judgement is never the decision that deletes customer data; " +
	"fix uefistore/prune.go so the pointed and the retained versions survive"

// Pruning deletes customer data on its own, so whatever the listing order,
// times and keep set, it never removes a kept key, one of the three newest,
// or anything outside the owner's own prefix.
func TestS7_UEFIPruneNeverDeletesThePointedObjectOrARetainedVersion(t *testing.T) {
	const other = "i-0fedcba9876543210"
	rng := rand.New(rand.NewPCG(1, 2))
	for trial := range 200 {
		store := newClockedStore()
		n := 1 + rng.IntN(8)
		times := map[string]time.Time{}
		var keys []string
		for i := range n {
			key := version(i)
			at := t0.Add(time.Duration(rng.IntN(5)) * time.Second)
			store.putAt(t, key, at)
			times[key] = at
			keys = append(keys, key)
		}
		store.putAt(t, uefistore.KeyPrefix(other)+"newest", t0.Add(time.Hour))
		store.putAt(t, uefistore.KeyPrefix(instanceID)+"nested/obj", t0)
		keep := []string{keys[rng.IntN(n)]}
		if rng.IntN(2) == 0 {
			keep = append(keep, keys[rng.IntN(n)])
		}

		deleted, err := uefistore.Prune(t.Context(), store, testBucket, instanceID, keep...)
		require.NoError(t, err)

		for _, key := range deleted {
			require.NotContains(t, keep, key, "%s: trial %d: a kept key was deleted", pruneS7Clause, trial)
			require.Contains(t, keys, key, "%s: trial %d: deleted %s outside the owner's own objects", pruneS7Clause, trial, key)
			survivors := 0
			for _, k := range keys {
				if !slices.Contains(deleted, k) && !times[k].Before(times[key]) {
					survivors++
				}
			}
			require.GreaterOrEqual(t, survivors, uefistore.RetainedVersions,
				"%s: trial %d: deleted %s while only %d versions at least as new survive", pruneS7Clause, trial, key, survivors)
		}
		remaining := store.keys(t, instanceID)
		require.Contains(t, remaining, uefistore.KeyPrefix(instanceID)+"nested/obj", pruneS7Clause)
		require.LessOrEqual(t, len(remaining)-1, uefistore.RetainedVersions+len(keep), "trial %d", trial)
		require.Len(t, store.keys(t, other), 1, pruneS7Clause)
	}
}

// A listing that fails decides nothing, so nothing is deleted on a guess.
func TestPrune_FailedListDeletesNothing(t *testing.T) {
	store := newClockedStore()
	for n := 1; n <= 5; n++ {
		store.putAt(t, version(n), t0.Add(time.Duration(n)*time.Minute))
	}
	store.listErr = errors.New("predastore unavailable")

	deleted, err := uefistore.Prune(t.Context(), store, testBucket, instanceID)

	require.Error(t, err)
	assert.Empty(t, deleted)
	assert.Empty(t, store.deleted)
	assert.Len(t, store.keys(t, instanceID), 5)
}

// One failed delete is reported and does not stop the others; an object
// already gone counts as deleted.
func TestPrune_FailedDeleteIsReportedAndTheRestStillRun(t *testing.T) {
	store := newClockedStore()
	for n := 1; n <= 6; n++ {
		store.putAt(t, version(n), t0.Add(time.Duration(n)*time.Minute))
	}
	store.deleteErr[version(1)] = errors.New("500")
	store.deleteErr[version(2)] = &objectstore.NoSuchKeyError{Key: version(2)}

	deleted, err := uefistore.Prune(t.Context(), store, testBucket, instanceID)

	require.ErrorContains(t, err, version(1))
	assert.ElementsMatch(t, []string{version(2), version(3)}, deleted)
}
