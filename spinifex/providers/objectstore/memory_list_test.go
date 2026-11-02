package objectstore_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func putKeys(t *testing.T, store *objectstore.MemoryObjectStore, bucket string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		_, err := store.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader(k)})
		require.NoError(t, err)
	}
}

func listedKeys(out *s3.ListObjectsV2Output) []string {
	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, aws.StringValue(o.Key))
	}
	return keys
}

func listedPrefixes(out *s3.ListObjectsV2Output) []string {
	var ps []string
	for _, p := range out.CommonPrefixes {
		ps = append(ps, aws.StringValue(p.Prefix))
	}
	return ps
}

// S3 returns keys in UTF-8 binary order, so uppercase sorts before lowercase
// and a multi-byte rune after every ASCII byte. A map-ordered double let tests
// pass or fail depending on iteration order.
func TestMemoryListObjectsV2_KeysInUTF8BinaryOrder(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putKeys(t, store, "b", "k/é", "k/b", "k/B", "k/a10", "k/a2", "k/~", "other/x")
	putKeys(t, store, "b2", "k/0")

	want := []string{"k/B", "k/a10", "k/a2", "k/b", "k/~", "k/é"}
	for range 20 {
		out, err := store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("b"), Prefix: aws.String("k/")})
		require.NoError(t, err)
		require.Equal(t, want, listedKeys(out))
		assert.False(t, aws.BoolValue(out.IsTruncated))
		assert.EqualValues(t, len(want), aws.Int64Value(out.KeyCount))
	}
}

func TestMemoryListObjectsV2_DelimiterRollsUpCommonPrefixesInOrder(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putKeys(t, store, "b", "p/z/1", "p/a/2", "p/a/1", "p/m", "p/b/1", "p/0")

	out, err := store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("b"), Prefix: aws.String("p/"), Delimiter: aws.String("/"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"p/0", "p/m"}, listedKeys(out))
	assert.Equal(t, []string{"p/a/", "p/b/", "p/z/"}, listedPrefixes(out))
	assert.EqualValues(t, 5, aws.Int64Value(out.KeyCount), "KeyCount counts keys and common prefixes")
}

// Common prefixes count toward MaxKeys, and a continuation never repeats a
// prefix or a key from an earlier page.
func TestMemoryListObjectsV2_PagesByMaxKeysAndContinuationToken(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putKeys(t, store, "b", "a/1", "a/2", "a/3", "b", "c/1", "d", "e/1", "e/2")

	var keys, prefixes []string
	var token *string
	pages := 0
	for {
		out, err := store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("b"), Delimiter: aws.String("/"), MaxKeys: aws.Int64(2), ContinuationToken: token,
		})
		require.NoError(t, err)
		pages++
		require.LessOrEqual(t, aws.Int64Value(out.KeyCount), int64(2))
		keys = append(keys, listedKeys(out)...)
		prefixes = append(prefixes, listedPrefixes(out)...)
		if !aws.BoolValue(out.IsTruncated) {
			break
		}
		require.NotEmpty(t, aws.StringValue(out.NextContinuationToken))
		token = out.NextContinuationToken
	}
	assert.Equal(t, []string{"b", "d"}, keys)
	assert.Equal(t, []string{"a/", "c/", "e/"}, prefixes)
	assert.Equal(t, 3, pages)
}

func TestMemoryListObjectsV2_DefaultPageIs1000AndListAllDrainsIt(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	for i := range 1001 {
		putKeys(t, store, "b", fmt.Sprintf("k/%04d", i))
	}

	out, err := store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("b")})
	require.NoError(t, err)
	assert.Len(t, out.Contents, 1000)
	assert.True(t, aws.BoolValue(out.IsTruncated))

	all, _, err := objectstore.ListAll(t.Context(), store, &s3.ListObjectsV2Input{Bucket: aws.String("b")})
	require.NoError(t, err)
	require.Len(t, all, 1001)
	assert.Equal(t, "k/1000", aws.StringValue(all[1000].Key))
}

func TestMemoryListObjectsV2_StartAfterIsExclusiveAndKeepsLaterPrefixGroups(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	putKeys(t, store, "b", "a/1", "a/9", "b", "c")

	out, err := store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("b"), StartAfter: aws.String("b")})
	require.NoError(t, err)
	assert.Equal(t, []string{"c"}, listedKeys(out))

	out, err = store.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String("b"), Delimiter: aws.String("/"), StartAfter: aws.String("a/5"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a/"}, listedPrefixes(out), "a/9 sorts after StartAfter, so its group is listed")
	assert.Equal(t, []string{"b", "c"}, listedKeys(out))
}
