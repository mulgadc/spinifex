// Package objectstore provides a common S3-like storage abstraction used by handlers
// to work with real backends (Predastore) or in-memory stores for testing.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// NoSuchKeyError represents a missing object error, compatible with AWS S3 errors.
type NoSuchKeyError struct {
	Key string
}

func (e *NoSuchKeyError) Error() string {
	return "NoSuchKey: " + e.Key
}

func (e *NoSuchKeyError) Code() string {
	return "NoSuchKey"
}

// IsNoSuchKeyError reports whether err is a NoSuchKey error.
func IsNoSuchKeyError(err error) bool {
	var noSuchKey *NoSuchKeyError
	return errors.As(err, &noSuchKey)
}

// ErrUnsafeKey is returned for an object key with a "." or ".." segment. The S3
// client resolves those before sending, so the request would address a
// different object, possibly in another bucket, from the one named.
var ErrUnsafeKey = errors.New("object key has a . or .. segment")

// checkKey refuses key if any of its segments is "." or "..".
func checkKey(key *string) error {
	for seg := range strings.SplitSeq(aws.StringValue(key), "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrUnsafeKey, aws.StringValue(key))
		}
	}
	return nil
}

// ObjectStore defines the interface for S3-like object storage operations.
// ctx carries the request trace context onto the backend HTTP request so
// Predastore spans join the caller's trace.
type ObjectStore interface {
	GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)
	// EnsureBucket idempotently makes bucket exist. It is safe to call
	// concurrently and on an already-present bucket.
	EnsureBucket(ctx context.Context, bucket string) error
}

// NewS3ObjectStoreFromConfig creates an S3ObjectStore from Predastore connection parameters.
func NewS3ObjectStoreFromConfig(host, region, accessKey, secretKey string) *S3ObjectStore {
	// otelhttp emits a client span per S3 request and injects traceparent,
	// but only when the request context already carries a span: background
	// jobs (state sync, cleanup loops) would otherwise root a trace per S3
	// call, surfacing as method-only "HTTP GET" transactions in APM.
	client := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithFilter(func(r *http.Request) bool {
			return trace.SpanFromContext(r.Context()).SpanContext().IsValid()
		})),
		Timeout: 120 * time.Second,
	}
	sess := session.Must(session.NewSession(&aws.Config{
		Endpoint:         aws.String(host),
		Region:           aws.String(region),
		Credentials:      credentials.NewStaticCredentials(accessKey, secretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient:       client,
	}))

	return NewS3ObjectStore(s3.New(sess))
}

// S3ObjectStore wraps the AWS S3 client to implement ObjectStore.
type S3ObjectStore struct {
	client *s3.S3
}

var _ ObjectStore = (*S3ObjectStore)(nil)

// NewS3ObjectStore creates an ObjectStore backed by AWS S3 or S3-compatible storage.
func NewS3ObjectStore(client *s3.S3) *S3ObjectStore {
	return &S3ObjectStore{client: client}
}

func (s *S3ObjectStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObjectWithContext(ctx, input)
	if err != nil {
		var aerr awserr.Error
		if errors.As(err, &aerr) &&
			(aerr.Code() == s3.ErrCodeNoSuchKey || aerr.Code() == "NotFound") {
			return nil, &NoSuchKeyError{Key: aws.StringValue(input.Key)}
		}
		return nil, err
	}
	return out, nil
}

func (s *S3ObjectStore) HeadObject(ctx context.Context, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	out, err := s.client.HeadObjectWithContext(ctx, input)
	if err != nil {
		var aerr awserr.Error
		if errors.As(err, &aerr) &&
			(aerr.Code() == s3.ErrCodeNoSuchKey || aerr.Code() == "NotFound") {
			return nil, &NoSuchKeyError{Key: aws.StringValue(input.Key)}
		}
		return nil, err
	}
	return out, nil
}

func (s *S3ObjectStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	return s.client.PutObjectWithContext(ctx, input)
}

// DeleteObject reports a missing key as NoSuchKeyError so callers can tell an
// already-deleted object from a backend failure. S3 itself answers 204 here, but
// Predastore answers 404, and a delete sweep must treat both as done.
func (s *S3ObjectStore) DeleteObject(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	out, err := s.client.DeleteObjectWithContext(ctx, input)
	if err != nil {
		var aerr awserr.Error
		if errors.As(err, &aerr) &&
			(aerr.Code() == s3.ErrCodeNoSuchKey || aerr.Code() == "NotFound") {
			return nil, &NoSuchKeyError{Key: aws.StringValue(input.Key)}
		}
		return nil, err
	}
	return out, nil
}

func (s *S3ObjectStore) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return s.client.ListObjectsV2WithContext(ctx, input)
}

// EnsureBucket creates bucket when absent. A successful HeadBucket short-circuits
// the create; an already-owned/existing bucket from a racing creator is treated
// as success so concurrent callers converge.
func (s *S3ObjectStore) EnsureBucket(ctx context.Context, bucket string) error {
	if _, err := s.client.HeadBucketWithContext(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		return nil
	}
	_, err := s.client.CreateBucketWithContext(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		if aerr, ok := errors.AsType[awserr.Error](err); ok {
			switch aerr.Code() {
			case s3.ErrCodeBucketAlreadyOwnedByYou, s3.ErrCodeBucketAlreadyExists:
				return nil
			}
		}
		return err
	}
	return nil
}

// MemoryObjectStore implements ObjectStore using in-memory storage for testing.
type MemoryObjectStore struct {
	objects  map[string][]byte    // key: bucket/key -> value: object data
	modified map[string]time.Time // key: bucket/key -> last write time
	mu       sync.RWMutex
}

var _ ObjectStore = (*MemoryObjectStore)(nil)

// NewMemoryObjectStore creates an in-memory ObjectStore for testing.
func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{
		objects:  make(map[string][]byte),
		modified: make(map[string]time.Time),
	}
}

// makeKey creates a storage key from bucket and key.
func makeKey(bucket, key string) string {
	return bucket + "/" + key
}

func (m *MemoryObjectStore) GetObject(_ context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	storageKey := makeKey(*input.Bucket, *input.Key)
	data, exists := m.objects[storageKey]
	if !exists {
		return nil, &NoSuchKeyError{Key: *input.Key}
	}

	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: aws.Int64(int64(len(data))),
	}, nil
}

func (m *MemoryObjectStore) HeadObject(_ context.Context, input *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	storageKey := makeKey(*input.Bucket, *input.Key)
	data, exists := m.objects[storageKey]
	if !exists {
		return nil, &NoSuchKeyError{Key: *input.Key}
	}

	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(data))),
	}, nil
}

func (m *MemoryObjectStore) PutObject(_ context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	storageKey := makeKey(*input.Bucket, *input.Key)

	data, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}

	m.objects[storageKey] = data
	m.modified[storageKey] = time.Now().UTC()

	return &s3.PutObjectOutput{}, nil
}

// DeleteObject answers NoSuchKeyError for an absent key, matching what
// S3ObjectStore returns against Predastore.
func (m *MemoryObjectStore) DeleteObject(_ context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if err := checkKey(input.Key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	storageKey := makeKey(*input.Bucket, *input.Key)
	if _, ok := m.objects[storageKey]; !ok {
		return nil, &NoSuchKeyError{Key: *input.Key}
	}
	delete(m.objects, storageKey)
	delete(m.modified, storageKey)

	return &s3.DeleteObjectOutput{}, nil
}

// ListObjectsV2 pages like S3: keys and common prefixes in UTF-8 binary order,
// at most MaxKeys (default 1000) entries per page, resumed by StartAfter or
// the returned NextContinuationToken.
func (m *MemoryObjectStore) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bucket := *input.Bucket
	prefix := aws.StringValue(input.Prefix)
	delimiter := aws.StringValue(input.Delimiter)
	startAfter := aws.StringValue(input.StartAfter)
	token := aws.StringValue(input.ContinuationToken)
	maxKeys := int64(memoryListMaxKeys)
	if input.MaxKeys != nil && *input.MaxKeys < maxKeys {
		maxKeys = max(*input.MaxKeys, 0)
	}

	// S3 lists in UTF-8 binary key order; Go string comparison is bytewise.
	var keys []string
	for key := range m.objects {
		if objectKey, ok := strings.CutPrefix(key, bucket+"/"); ok && strings.HasPrefix(objectKey, prefix) && objectKey > startAfter {
			keys = append(keys, objectKey)
		}
	}
	slices.Sort(keys)

	out := &s3.ListObjectsV2Output{
		Name:              input.Bucket,
		Prefix:            input.Prefix,
		Delimiter:         input.Delimiter,
		StartAfter:        input.StartAfter,
		ContinuationToken: input.ContinuationToken,
		MaxKeys:           aws.Int64(maxKeys),
		IsTruncated:       aws.Bool(false),
	}
	// The token is the last key or common prefix returned. Every key under a
	// returned common prefix rolls up to that same entry, so "entry <= token"
	// skips the whole group on the next page.
	var count int64
	last := ""
	for _, objectKey := range keys {
		entry, isPrefix := objectKey, false
		if delimiter != "" {
			if idx := strings.Index(objectKey[len(prefix):], delimiter); idx >= 0 {
				entry, isPrefix = objectKey[:len(prefix)+idx+len(delimiter)], true
			}
		}
		if (token != "" && entry <= token) || (isPrefix && entry == last) {
			continue
		}
		if maxKeys == 0 {
			break
		}
		if count == maxKeys {
			out.IsTruncated = aws.Bool(true)
			out.NextContinuationToken = aws.String(last)
			break
		}
		last = entry
		count++
		if isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, &s3.CommonPrefix{Prefix: aws.String(entry)})
			continue
		}
		full := bucket + "/" + objectKey
		out.Contents = append(out.Contents, &s3.Object{
			Key:          aws.String(objectKey),
			Size:         aws.Int64(int64(len(m.objects[full]))),
			LastModified: aws.Time(m.modified[full]),
		})
	}
	out.KeyCount = aws.Int64(count)
	return out, nil
}

// memoryListMaxKeys is S3's per-page cap on ListObjectsV2 results.
const memoryListMaxKeys = 1000

// EnsureBucket is a no-op: the memory store has no bucket namespace, keys are
// prefixed with the bucket name on write.
func (m *MemoryObjectStore) EnsureBucket(_ context.Context, bucket string) error { return nil }

// Clear removes all objects from the memory store (useful for test cleanup).
func (m *MemoryObjectStore) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects = make(map[string][]byte)
	m.modified = make(map[string]time.Time)
}

// Count returns the number of objects in the memory store.
func (m *MemoryObjectStore) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}
