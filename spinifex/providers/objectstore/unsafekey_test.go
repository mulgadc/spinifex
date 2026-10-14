package objectstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pathRecorder answers every request 200 and records the path it was sent to.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// keyedOps runs each keyed operation against key and returns their errors.
func keyedOps(store ObjectStore, key string) map[string]error {
	ctx := context.Background()
	bucket := aws.String("bucket")
	k := aws.String(key)
	_, getErr := store.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: k})
	_, headErr := store.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: k})
	_, putErr := store.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: k, Body: strings.NewReader("x")})
	_, delErr := store.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: k})
	return map[string]error{"Get": getErr, "Head": headErr, "Put": putErr, "Delete": delErr}
}

// The S3 client cleans ".." out of the request path before signing, so a key
// with such a segment would be sent to a different object, or another bucket.
func TestS3ObjectStore_RefusesDotSegmentKeysBeforeSending(t *testing.T) {
	for _, key := range []string{"keys/111/../222/k", "../../other-bucket/k", "keys/./k", "keys/111/..", "."} {
		t.Run(key, func(t *testing.T) {
			rec := &pathRecorder{}
			srv := httptest.NewServer(rec)
			defer srv.Close()

			for op, err := range keyedOps(newS3Store(t, srv.URL), key) {
				assert.ErrorIs(t, err, ErrUnsafeKey, op)
			}
			assert.Empty(t, rec.paths)
		})
	}
}

func TestS3ObjectStore_SendsDottedNamesUnchanged(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	for op, err := range keyedOps(newS3Store(t, srv.URL), "a/.b/c..d/...") {
		require.NoError(t, err, op)
	}
	require.Len(t, rec.paths, 4)
	for _, p := range rec.paths {
		assert.Equal(t, "/bucket/a/.b/c..d/...", p)
	}
}

func TestMemoryObjectStore_RefusesDotSegmentKeys(t *testing.T) {
	for op, err := range keyedOps(NewMemoryObjectStore(), "keys/111/../222/k") {
		assert.ErrorIs(t, err, ErrUnsafeKey, op)
	}
}
