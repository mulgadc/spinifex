package ebsmetadata

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagedListStore serves CommonPrefixes one page at a time. MemoryObjectStore
// never sets IsTruncated, so it cannot exercise the paging path that a real
// bucket with more than a page of volumes would take.
type pagedListStore struct {
	objectstore.ObjectStore

	pages [][]string
	calls int
}

func (p *pagedListStore) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	page := 0
	if input.ContinuationToken != nil {
		page = int(aws.StringValue(input.ContinuationToken)[0] - '0')
	}
	p.calls++

	out := &s3.ListObjectsV2Output{}
	for _, prefix := range p.pages[page] {
		out.CommonPrefixes = append(out.CommonPrefixes, &s3.CommonPrefix{Prefix: aws.String(prefix)})
	}
	if page+1 < len(p.pages) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(string(rune('0' + page + 1)))
	}
	return out, nil
}

func TestLegacyPrefixIDs_PagesToExhaustion(t *testing.T) {
	store := &pagedListStore{pages: [][]string{
		{"vol-a/", "vol-b/"},
		{"vol-c/", "ami-1/"},
		{"vol-d/"},
	}}

	ids, err := LegacyPrefixIDs(context.Background(), store, "bucket", "vol-")
	require.NoError(t, err)

	// A caller that stopped at the first page would return only vol-a/vol-b,
	// and the backfill would stamp itself complete having missed the rest.
	assert.Equal(t, []string{"vol-a", "vol-b", "vol-c", "vol-d"}, ids)
	assert.Equal(t, 3, store.calls, "must request every page")
}

func TestLegacyPrefixIDs_ExcludesInternalSubVolumesAcrossPages(t *testing.T) {
	store := &pagedListStore{pages: [][]string{
		{"vol-a/", "vol-a-efi/"},
		{"vol-b-cloudinit/", "vol-b/"},
	}}

	ids, err := LegacyPrefixIDs(context.Background(), store, "bucket", "vol-", "-efi", "-cloudinit")
	require.NoError(t, err)
	assert.Equal(t, []string{"vol-a", "vol-b"}, ids)
}
