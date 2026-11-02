// Package pagedstore provides an objectstore.ObjectStore fake that serves
// ListObjectsV2 across multiple deterministic pages, honouring
// ContinuationToken and IsTruncated the way a real paginating S3-compatible
// store does. It exists so tests can prove a caller follows continuation
// tokens to exhaustion instead of stopping after the first page.
package pagedstore

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

// Store wraps a MemoryObjectStore whose ListObjectsV2 pages like S3, capping
// every page at PageSize entries so small fixtures span several pages.
type Store struct {
	*objectstore.MemoryObjectStore

	PageSize int

	// Calls counts ListObjectsV2 invocations, so tests can assert every page
	// was actually walked rather than the loop stopping early by luck.
	Calls int
}

// New returns an empty Store that serves at most pageSize entries per page.
func New(pageSize int) *Store {
	return &Store{MemoryObjectStore: objectstore.NewMemoryObjectStore(), PageSize: pageSize}
}

var _ objectstore.ObjectStore = (*Store)(nil)

// ListObjectsV2 lowers the caller's MaxKeys to PageSize and lets the memory
// store page, so tokens, ordering and common prefixes follow S3's rules.
func (s *Store) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	s.Calls++
	page := *input
	if page.MaxKeys == nil || *page.MaxKeys > int64(s.PageSize) {
		page.MaxKeys = aws.Int64(int64(s.PageSize))
	}
	return s.MemoryObjectStore.ListObjectsV2(ctx, &page)
}
