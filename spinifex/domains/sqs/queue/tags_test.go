package queue

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTagged(f *fixture, name string, tags map[string]string) (Record, error) {
	return f.store.Create(context.Background(), CreateRequest{Account: acct, Region: region, Name: name, Tags: tags})
}

func tag(f *fixture, name string, tags map[string]string) (Record, error) {
	return f.store.Tag(context.Background(), acct, region, name, tags)
}

func untag(f *fixture, name string, keys ...string) (Record, error) {
	return f.store.Untag(context.Background(), acct, region, name, keys)
}

func manyTags(n int) map[string]string {
	m := make(map[string]string, n)
	for i := range n {
		m[fmt.Sprintf("k%02d", i)] = "v"
	}
	return m
}

func TestValidateTags(t *testing.T) {
	ok := []map[string]string{
		{"env": ""},
		{strings.Repeat("é", maxTagKeyLen): strings.Repeat("ü", maxTagValueLen)},
		manyTags(maxTags),
		{"Aws-team": "x", "awsome": "y"},
	}
	for _, m := range ok {
		assert.NoError(t, ValidateTags(m, false))
	}
	bad := []map[string]string{
		nil,
		{},
		{"": "v"},
		{strings.Repeat("k", maxTagKeyLen+1): "v"},
		{"k": strings.Repeat("v", maxTagValueLen+1)},
		{"aws:team": "x"},
		{"AWS:team": "x"},
		{"\xff": "v"},
		{"k": "\xff"},
		manyTags(maxTags + 1),
	}
	for _, m := range bad {
		assert.ErrorIs(t, ValidateTags(m, false), ErrInvalidTag, "%q", m)
	}
	assert.NoError(t, ValidateTags(nil, true))

	assert.ErrorIs(t, ValidateTagKeys(nil), ErrInvalidTag)
	assert.ErrorIs(t, ValidateTagKeys([]string{"ok", ""}), ErrInvalidTag)
	assert.ErrorIs(t, ValidateTagKeys([]string{"aws:x"}), ErrInvalidTag)
	assert.NoError(t, ValidateTagKeys([]string{"ok"}))
}

func TestCreateWithTags(t *testing.T) {
	f := newFixture(t)
	q, err := createTagged(f, "orders", map[string]string{"env": "prod"})
	require.NoError(t, err)
	got, err := f.store.Tags(context.Background(), acct, region, "orders")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod"}, got)

	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, map[string]string{"env": "prod"}, stored.Tags)
	assert.Equal(t, uint64(1), stored.Generation)

	_, err = create(f, "plain", nil)
	require.NoError(t, err)
	got, err = f.store.Tags(context.Background(), acct, region, "plain")
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestTagQueue(t *testing.T) {
	f := newFixture(t)
	q, err := createTagged(f, "orders", map[string]string{"env": "prod", "team": "a"})
	require.NoError(t, err)
	f.now = f.now.Add(time.Hour)

	r, err := tag(f, "orders", map[string]string{"team": "b", "cost": "42"})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), r.Generation)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, map[string]string{"env": "prod", "team": "b", "cost": "42"}, stored.Tags)
	assert.Equal(t, uint64(2), stored.Generation)
	assert.True(t, stored.AttributesModifiedAt.Equal(q.AttributesModifiedAt), "tags do not move the attribute modification time")
	assert.Equal(t, q.Attributes, stored.Attributes)
}

func TestTagQueueAllOrNothing(t *testing.T) {
	f := newFixture(t)
	q, err := createTagged(f, "orders", manyTags(maxTags-1))
	require.NoError(t, err)
	_, before := f.record(t, q.QueueID)

	for _, tags := range []map[string]string{
		{},
		{"new": "1", "aws:x": "2"},
		{"new": "1", strings.Repeat("k", maxTagKeyLen+1): "2"},
		{"new": "1", "other": "2"},
	} {
		_, err := tag(f, "orders", tags)
		assert.ErrorIs(t, err, ErrInvalidTag, "%v", tags)
	}
	stored, after := f.record(t, q.QueueID)
	assert.Equal(t, before, after, "a rejected request writes nothing")
	assert.Len(t, stored.Tags, maxTags-1)

	_, err = tag(f, "orders", map[string]string{"k00": "overwrite", "last": "1"})
	require.NoError(t, err, "overwriting an existing key does not count against the limit")
	stored, _ = f.record(t, q.QueueID)
	assert.Len(t, stored.Tags, maxTags)
}

func TestUntagQueue(t *testing.T) {
	f := newFixture(t)
	q, err := createTagged(f, "orders", map[string]string{"env": "prod", "team": "a", "Team": "b"})
	require.NoError(t, err)

	r, err := untag(f, "orders", "team", "absent")
	require.NoError(t, err)
	assert.Equal(t, uint64(2), r.Generation)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, map[string]string{"env": "prod", "Team": "b"}, stored.Tags, "keys are case-sensitive")

	r, err = untag(f, "orders", "team", "absent")
	require.NoError(t, err)
	assert.Equal(t, uint64(3), r.Generation, "an accepted call advances the generation even if it removes nothing")
	stored, rev := f.record(t, q.QueueID)
	assert.Equal(t, map[string]string{"env": "prod", "Team": "b"}, stored.Tags)

	for _, keys := range [][]string{nil, {"env", ""}, {"env", "aws:x"}} {
		_, err := untag(f, "orders", keys...)
		assert.ErrorIs(t, err, ErrInvalidTag)
	}
	_, after := f.record(t, q.QueueID)
	assert.Equal(t, rev, after, "an invalid request writes nothing")
}

// Concurrent tag and attribute updates each apply exactly once.
func TestTagAndAttributeUpdatesConcurrent(t *testing.T) {
	f := newFixture(t)
	q, err := createTagged(f, "orders", map[string]string{"drop": "x"})
	require.NoError(t, err)
	ops := []func() error{
		func() error { _, err := tag(f, "orders", map[string]string{"a": "1"}); return err },
		func() error { _, err := tag(f, "orders", map[string]string{"b": "2"}); return err },
		func() error { _, err := untag(f, "orders", "drop"); return err },
		func() error { _, err := set(f, "orders", map[string]string{AttrDelaySeconds: "9"}); return err },
		func() error { _, err := set(f, "orders", map[string]string{AttrVisibilityTimeout: "99"}); return err },
	}
	errs := make([]error, len(ops))
	var wg sync.WaitGroup
	for i, op := range ops {
		wg.Go(func() { errs[i] = op() })
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	r, _ := f.record(t, q.QueueID)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, r.Tags)
	assert.Equal(t, "9", r.Attributes[AttrDelaySeconds])
	assert.Equal(t, "99", r.Attributes[AttrVisibilityTimeout])
	assert.Equal(t, uint64(1+len(ops)), r.Generation)
}

func TestTagQueueAppliedReplyLost(t *testing.T) {
	f := newFixture(t)
	q, err := create(f, "orders", nil)
	require.NoError(t, err)
	f.fault.onUpdate = func(key string, value []byte, rev uint64) (uint64, error, bool) {
		f.fault.onUpdate = nil
		_, err := f.fault.real.update(context.Background(), key, value, rev)
		require.NoError(t, err)
		return 0, errLostReply, true
	}
	_, err = tag(f, "orders", map[string]string{"env": "prod"})
	assert.ErrorIs(t, err, ErrUnavailable)
	stored, _ := f.record(t, q.QueueID)
	assert.Equal(t, uint64(2), stored.Generation)
	assert.Equal(t, "prod", stored.Tags["env"])
}
