package utils

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tagMirrorRecord struct {
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
}

func TestMergeTagsMut(t *testing.T) {
	mut := MergeTagsMut(&ec2.CreateTagsInput{
		Tags: []*ec2.Tag{
			{Key: aws.String("a"), Value: aws.String("1")},
			{Key: aws.String("b"), Value: aws.String("2")},
			{Key: nil, Value: aws.String("skip")},
			{Key: aws.String("skip"), Value: nil},
		},
	})
	tags := map[string]string{"a": "old", "keep": "x"}
	mut(tags)
	assert.Equal(t, map[string]string{"a": "1", "b": "2", "keep": "x"}, tags)
}

func TestRemoveTagsMut(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"a": "1", "b": "2", "c": "3"}
	}

	t.Run("empty input clears all", func(t *testing.T) {
		tags := base()
		RemoveTagsMut(&ec2.DeleteTagsInput{})(tags)
		assert.Empty(t, tags)
	})

	t.Run("value match deletes, mismatch keeps", func(t *testing.T) {
		tags := base()
		RemoveTagsMut(&ec2.DeleteTagsInput{Tags: []*ec2.Tag{
			{Key: aws.String("a"), Value: aws.String("1")},
			{Key: aws.String("b"), Value: aws.String("wrong")},
		}})(tags)
		assert.Equal(t, map[string]string{"b": "2", "c": "3"}, tags)
	})

	t.Run("nil value deletes unconditionally, nil key skipped", func(t *testing.T) {
		tags := base()
		RemoveTagsMut(&ec2.DeleteTagsInput{Tags: []*ec2.Tag{
			{Key: aws.String("c"), Value: nil},
			{Key: nil, Value: aws.String("1")},
		}})(tags)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, tags)
	})
}

func TestUpdateKVRecordTags(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	rec, err := json.Marshal(&tagMirrorRecord{Name: "r1", Tags: map[string]string{"a": "1"}})
	require.NoError(t, err)
	kv := testutil.SeedKV(t, js, "tagmirror-test", map[string][]byte{
		AccountKey("acct", "res-1"):   rec,
		AccountKey("acct", "res-bad"): []byte("not-json"),
	})

	t.Run("mutates and persists", func(t *testing.T) {
		require.NoError(t, UpdateKVRecordTags(kv, "acct", "res-1", func(r *tagMirrorRecord) {
			r.Tags["b"] = "2"
		}))
		entry, err := kv.Get(AccountKey("acct", "res-1"))
		require.NoError(t, err)
		var got tagMirrorRecord
		require.NoError(t, json.Unmarshal(entry.Value(), &got))
		assert.Equal(t, "r1", got.Name)
		assert.Equal(t, map[string]string{"a": "1", "b": "2"}, got.Tags)
	})

	t.Run("absent key is a no-op", func(t *testing.T) {
		require.NoError(t, UpdateKVRecordTags(kv, "acct", "res-missing", func(r *tagMirrorRecord) {
			t.Fatal("mutator must not run for absent record")
		}))
	})

	t.Run("corrupt record returns internal error", func(t *testing.T) {
		err := UpdateKVRecordTags(kv, "acct", "res-bad", func(r *tagMirrorRecord) {})
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	})
}

func TestMirrorKVRecordTags(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	withTags, err := json.Marshal(&tagMirrorRecord{Tags: map[string]string{"drop": "v", "keep": "yes"}})
	require.NoError(t, err)
	nilTags, err := json.Marshal(&tagMirrorRecord{})
	require.NoError(t, err)
	kv := testutil.SeedKV(t, js, "tagmirror-mirror-test", map[string][]byte{
		AccountKey("acct", "vol-1"): withTags,
		AccountKey("acct", "vol-2"): nilTags,
	})

	tagsOf := func(r *tagMirrorRecord) *map[string]string { return &r.Tags }

	readTags := func(t *testing.T, id string) map[string]string {
		entry, err := kv.Get(AccountKey("acct", id))
		require.NoError(t, err)
		var got tagMirrorRecord
		require.NoError(t, json.Unmarshal(entry.Value(), &got))
		return got.Tags
	}

	resources := []*string{
		aws.String("vol-1"),
		aws.String("vol-2"),
		aws.String("snap-other"),
		aws.String("vol-absent"),
		nil,
	}

	require.NoError(t, MirrorKVRecordTags(kv, "acct", "vol-", resources, tagsOf,
		MergeTagsMut(&ec2.CreateTagsInput{Tags: []*ec2.Tag{
			{Key: aws.String("added"), Value: aws.String("1")},
		}})))
	assert.Equal(t, map[string]string{"drop": "v", "keep": "yes", "added": "1"}, readTags(t, "vol-1"))
	assert.Equal(t, map[string]string{"added": "1"}, readTags(t, "vol-2"), "nil tag map initialized in place")

	require.NoError(t, MirrorKVRecordTags(kv, "acct", "vol-", resources, tagsOf,
		RemoveTagsMut(&ec2.DeleteTagsInput{Tags: []*ec2.Tag{
			{Key: aws.String("drop"), Value: aws.String("v")},
			{Key: aws.String("keep"), Value: aws.String("wrong")},
		}})))
	assert.Equal(t, map[string]string{"keep": "yes", "added": "1"}, readTags(t, "vol-1"))

	// snap-other carries the wrong prefix, so a record stored under that id
	// must never be touched even though it exists in this bucket.
	otherRec, err := json.Marshal(&tagMirrorRecord{Tags: map[string]string{"x": "y"}})
	require.NoError(t, err)
	_, err = kv.Put(AccountKey("acct", "snap-other"), otherRec)
	require.NoError(t, err)
	require.NoError(t, MirrorKVRecordTags(kv, "acct", "vol-", resources, tagsOf,
		MergeTagsMut(&ec2.CreateTagsInput{Tags: []*ec2.Tag{
			{Key: aws.String("added"), Value: aws.String("2")},
		}})))
	assert.Equal(t, map[string]string{"x": "y"}, readTags(t, "snap-other"))
}
