// Package pagedstore provides an objectstore.ObjectStore fake that serves
// ListObjectsV2 across multiple deterministic pages, honouring
// ContinuationToken and IsTruncated the way a real paginating S3-compatible
// store does. It exists so tests can prove a caller follows continuation
// tokens to exhaustion instead of stopping after the first page.
package pagedstore

import (
	"context"
	"sort"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Store wraps a MemoryObjectStore and splits ListObjectsV2's answer into
// pages of at most PageSize objects, sorted by key so pagination is
// deterministic across runs (MemoryObjectStore itself iterates a map).
type Store struct {
	*objectstore.MemoryObjectStore

	PageSize int

	// Calls counts ListObjectsV2 invocations, so tests can assert every page
	// was actually walked rather than the loop stopping early by luck.
	Calls int
}

// New returns a paging store with the given per-page object limit.
func New(pageSize int) *Store {
	return &Store{MemoryObjectStore: objectstore.NewMemoryObjectStore(), PageSize: pageSize}
}

var _ objectstore.ObjectStore = (*Store)(nil)

// ListObjectsV2 delegates to MemoryObjectStore for prefix/delimiter matching,
// then slices the sorted result into one page per call, resuming from the
// offset encoded in ContinuationToken.
func (s *Store) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	s.Calls++

	full, err := s.MemoryObjectStore.ListObjectsV2(ctx, input)
	if err != nil {
		return nil, err
	}

	sort.Slice(full.Contents, func(i, j int) bool {
		return aws.StringValue(full.Contents[i].Key) < aws.StringValue(full.Contents[j].Key)
	})

	start := 0
	if tok := aws.StringValue(input.ContinuationToken); tok != "" {
		start, err = strconv.Atoi(tok)
		if err != nil {
			return nil, err
		}
	}
	end := min(start+s.PageSize, len(full.Contents))
	if start > end {
		start = end
	}

	page := &s3.ListObjectsV2Output{
		Contents:       full.Contents[start:end],
		CommonPrefixes: full.CommonPrefixes,
		Name:           input.Bucket,
		KeyCount:       aws.Int64(int64(end - start)),
	}
	if end < len(full.Contents) {
		page.IsTruncated = aws.Bool(true)
		page.NextContinuationToken = aws.String(strconv.Itoa(end))
	} else {
		page.IsTruncated = aws.Bool(false)
	}
	return page, nil
}
