package objectstore

import (
	"context"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagingStore replays a fixed sequence of ListObjectsV2 pages, honouring
// continuation tokens the way a real paginating store would: each call
// returns the next page and, while pages remain, a token pointing at it.
type pagingStore struct {
	pages []*s3.ListObjectsV2Output
	calls int
}

func (p *pagingStore) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	wantToken := ""
	if p.calls > 0 {
		wantToken = tokenFor(p.calls)
	}
	if aws.StringValue(input.ContinuationToken) != wantToken {
		panic("pagingStore: unexpected continuation token")
	}
	out := p.pages[p.calls]
	p.calls++
	if p.calls < len(p.pages) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(tokenFor(p.calls))
	} else {
		out.IsTruncated = aws.Bool(false)
		out.NextContinuationToken = nil
	}
	return out, nil
}

func (p *pagingStore) GetObject(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	return nil, nil
}
func (p *pagingStore) HeadObject(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	return nil, nil
}
func (p *pagingStore) PutObject(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	return nil, nil
}
func (p *pagingStore) DeleteObject(context.Context, *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	return nil, nil
}
func (p *pagingStore) EnsureBucket(context.Context, string) error { return nil }

func tokenFor(page int) string {
	return "page-" + strconv.Itoa(page)
}

var _ ObjectStore = (*pagingStore)(nil)

// truncatedNoTokenStore always reports truncation but never supplies a
// continuation token to resume from — the shape a broken or misbehaving
// store could produce.
type truncatedNoTokenStore struct{ pagingStore }

func (t *truncatedNoTokenStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true)}, nil
}

// neverEndingStore always claims truncation with a fresh token, modelling a
// server that never finishes a listing.
type neverEndingStore struct{ pagingStore }

func (n *neverEndingStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	n.calls++
	return &s3.ListObjectsV2Output{
		IsTruncated:           aws.Bool(true),
		NextContinuationToken: aws.String("next"),
	}, nil
}

func TestListAll_SinglePage(t *testing.T) {
	store := &pagingStore{pages: []*s3.ListObjectsV2Output{
		{Contents: []*s3.Object{{Key: aws.String("a")}, {Key: aws.String("b")}}},
	}}

	contents, prefixes, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("p/"),
	})
	require.NoError(t, err)
	assert.Empty(t, prefixes)
	require.Len(t, contents, 2)
	assert.Equal(t, "a", aws.StringValue(contents[0].Key))
	assert.Equal(t, "b", aws.StringValue(contents[1].Key))
	assert.Equal(t, 1, store.calls)
}

// A listing larger than one page must follow the continuation token to
// exhaustion rather than stopping after the first page.
func TestListAll_MultiPageFollowsContinuationToken(t *testing.T) {
	store := &pagingStore{pages: []*s3.ListObjectsV2Output{
		{Contents: []*s3.Object{{Key: aws.String("a")}, {Key: aws.String("b")}}},
		{Contents: []*s3.Object{{Key: aws.String("c")}}},
		{Contents: []*s3.Object{{Key: aws.String("d")}, {Key: aws.String("e")}}},
	}}

	contents, _, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("p/"),
	})
	require.NoError(t, err)
	require.Len(t, contents, 5)
	var keys []string
	for _, obj := range contents {
		keys = append(keys, aws.StringValue(obj.Key))
	}
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, keys)
	assert.Equal(t, 3, store.calls)
}

// A delimited listing accumulates CommonPrefixes across pages the same way
// Contents does.
func TestListAll_MultiPageCommonPrefixes(t *testing.T) {
	store := &pagingStore{pages: []*s3.ListObjectsV2Output{
		{CommonPrefixes: []*s3.CommonPrefix{{Prefix: aws.String("snap-1/")}}},
		{CommonPrefixes: []*s3.CommonPrefix{{Prefix: aws.String("snap-2/")}}},
	}}

	_, prefixes, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("snap-"), Delimiter: aws.String("/"),
	})
	require.NoError(t, err)
	require.Len(t, prefixes, 2)
	assert.Equal(t, "snap-1/", aws.StringValue(prefixes[0].Prefix))
	assert.Equal(t, "snap-2/", aws.StringValue(prefixes[1].Prefix))
}

func TestListAll_ListError(t *testing.T) {
	store := &erroringStore{err: assert.AnError}
	_, _, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("p/"),
	})
	require.Error(t, err)
}

type erroringStore struct {
	pagingStore

	err error
}

func (e *erroringStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return nil, e.err
}

// Truncation reported with no continuation token to resume from must fail
// rather than silently returning the partial page it already read.
func TestListAll_TruncatedWithNoTokenErrors(t *testing.T) {
	store := &truncatedNoTokenStore{}
	_, _, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("p/"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reported truncation with no continuation token")
}

// A store that never stops claiming truncation must fail once the page
// budget is exhausted rather than looping forever.
func TestListAll_NeverEndingTruncationErrors(t *testing.T) {
	store := &neverEndingStore{}
	_, _, err := ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String("bucket"), Prefix: aws.String("p/"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not finish within")
	assert.Equal(t, maxListPages, store.calls)
}
