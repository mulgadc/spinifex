// Package readonlystore wraps an object store or a JetStream KV bucket so that
// any write fails the test. It exists to prove a code path is read-only, which
// an assertion on the final store contents cannot: a write that is undone
// before the check would pass it.
package readonlystore

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrWrite is returned by every write after the test has been failed.
var ErrWrite = errors.New("write to a read-only store")

// ObjectStore fails the test on any write and records every key it reads.
type ObjectStore struct {
	objectstore.ObjectStore

	t testing.TB

	mu   sync.Mutex
	gets []string
}

var _ objectstore.ObjectStore = (*ObjectStore)(nil)

// NewObjectStore wraps inner for t.
func NewObjectStore(t testing.TB, inner objectstore.ObjectStore) *ObjectStore {
	return &ObjectStore{ObjectStore: inner, t: t}
}

func (s *ObjectStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	s.mu.Lock()
	s.gets = append(s.gets, aws.StringValue(in.Key))
	s.mu.Unlock()
	return s.ObjectStore.GetObject(ctx, in)
}

// Gets returns every key read so far.
func (s *ObjectStore) Gets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gets...)
}

func (s *ObjectStore) PutObject(_ context.Context, in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	s.t.Errorf("read-only store: PutObject %s", aws.StringValue(in.Key))
	return nil, ErrWrite
}

func (s *ObjectStore) DeleteObject(_ context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	s.t.Errorf("read-only store: DeleteObject %s", aws.StringValue(in.Key))
	return nil, ErrWrite
}

func (s *ObjectStore) EnsureBucket(_ context.Context, bucket string) error {
	s.t.Errorf("read-only store: EnsureBucket %s", bucket)
	return ErrWrite
}

// KV fails the test on any write to the wrapped bucket.
type KV struct {
	jetstream.KeyValue

	t testing.TB
}

// NewKV wraps inner for t.
func NewKV(t testing.TB, inner jetstream.KeyValue) *KV {
	return &KV{KeyValue: inner, t: t}
}

func (k *KV) write(op, key string) error {
	k.t.Errorf("read-only KV %s: %s %s", k.Bucket(), op, key)
	return ErrWrite
}

func (k *KV) Put(_ context.Context, key string, _ []byte) (uint64, error) {
	return 0, k.write("Put", key)
}

func (k *KV) PutString(_ context.Context, key string, _ string) (uint64, error) {
	return 0, k.write("PutString", key)
}

func (k *KV) Create(_ context.Context, key string, _ []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	return 0, k.write("Create", key)
}

func (k *KV) Update(_ context.Context, key string, _ []byte, _ uint64) (uint64, error) {
	return 0, k.write("Update", key)
}

func (k *KV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	return k.write("Delete", key)
}

func (k *KV) Purge(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	return k.write("Purge", key)
}

func (k *KV) PurgeDeletes(_ context.Context, _ ...jetstream.KVPurgeOpt) error {
	return k.write("PurgeDeletes", "")
}
