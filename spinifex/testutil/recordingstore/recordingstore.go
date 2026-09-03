// Package recordingstore provides an objectstore.ObjectStore fake that records
// every key it is asked to read and every prefix it is asked to list. It exists
// so tests can assert an access pattern rather than only an answer: that an
// account-scoped listing never touched another account's prefix, which an
// in-memory filter over a whole-cluster read would satisfy just as well.
package recordingstore

import (
	"context"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
)

// Store wraps a MemoryObjectStore, appending to Gets and ListPrefixes before
// delegating. Both slices are safe to read once the call under test returns.
type Store struct {
	*objectstore.MemoryObjectStore

	mu           sync.Mutex
	gets         []string
	listPrefixes []string
}

// New returns a recording store backed by an empty in-memory store.
func New() *Store {
	return &Store{MemoryObjectStore: objectstore.NewMemoryObjectStore()}
}

var _ objectstore.ObjectStore = (*Store)(nil)

func (s *Store) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	s.gets = append(s.gets, aws.StringValue(input.Key))
	s.mu.Unlock()
	return s.MemoryObjectStore.GetObject(ctx, input)
}

func (s *Store) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	s.mu.Lock()
	s.listPrefixes = append(s.listPrefixes, aws.StringValue(input.Prefix))
	s.mu.Unlock()
	return s.MemoryObjectStore.ListObjectsV2(ctx, input)
}

// Gets returns every key read so far.
func (s *Store) Gets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gets...)
}

// ListPrefixes returns every prefix listed so far.
func (s *Store) ListPrefixes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.listPrefixes...)
}

// Reset drops what has been recorded, so a test can seed fixtures and then
// assert only on the call it is actually exercising.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets, s.listPrefixes = nil, nil
}

// TouchedPrefix reports whether any recorded get or listing named a key under
// prefix. This is the blast-radius assertion: it must be false for every
// account other than the caller's.
func (s *Store) TouchedPrefix(prefix string) bool {
	for _, key := range s.Gets() {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	for _, listed := range s.ListPrefixes() {
		if strings.HasPrefix(listed, prefix) || strings.HasPrefix(prefix, listed) {
			return true
		}
	}
	return false
}
