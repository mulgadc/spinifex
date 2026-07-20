package vbscan

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeObject is one object in fakeStore, keyed by bucket/key.
type fakeObject struct {
	body []byte
}

// fakeStore is a minimal in-memory ObjectStore for exercising Scanner without
// a real S3-compatible backend. Pagination is driven by maxKeysPerPage so
// tests can exercise the ContinuationToken loop without needing thousands of
// objects.
type fakeStore struct {
	objects         map[string]fakeObject
	maxKeysPerPage  int
	listErr         error
	getErrForPrefix string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string]fakeObject), maxKeysPerPage: 0}
}

func (f *fakeStore) put(bucket, key string, body []byte) {
	f.objects[bucket+"/"+key] = fakeObject{body: body}
}

func (f *fakeStore) GetObject(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if f.getErrForPrefix != "" && aws.StringValue(in.Key) == f.getErrForPrefix {
		return nil, errors.New("simulated get error")
	}
	obj, ok := f.objects[aws.StringValue(in.Bucket)+"/"+aws.StringValue(in.Key)]
	if !ok {
		return nil, errors.New("NoSuchKey: " + aws.StringValue(in.Key))
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(obj.body))}, nil
}

// listing key/size pairs under a bucket+prefix, in insertion-independent
// (sorted) order so pagination is deterministic across test runs.
func (f *fakeStore) matchingKeys(bucket, prefix string) []string {
	var keys []string
	for k := range f.objects {
		b, key, ok := bytes.Cut([]byte(k), []byte("/"))
		_ = ok
		if string(b) != bucket {
			continue
		}
		if len(prefix) == 0 || bytes.HasPrefix(key, []byte(prefix)) {
			keys = append(keys, string(key))
		}
	}
	// Simple insertion sort; test object counts are tiny.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func (f *fakeStore) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	bucket := aws.StringValue(in.Bucket)
	prefix := aws.StringValue(in.Prefix)
	keys := f.matchingKeys(bucket, prefix)

	start := 0
	if in.ContinuationToken != nil {
		n, err := strconv.Atoi(aws.StringValue(in.ContinuationToken))
		if err != nil {
			return nil, err
		}
		start = n
	}

	pageSize := len(keys)
	if f.maxKeysPerPage > 0 {
		pageSize = f.maxKeysPerPage
	}

	end := start + pageSize
	truncated := false
	if end < len(keys) {
		truncated = true
	} else {
		end = len(keys)
	}

	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(truncated)}
	for _, k := range keys[start:end] {
		obj := f.objects[bucket+"/"+k]
		out.Contents = append(out.Contents, &s3.Object{
			Key:  aws.String(k),
			Size: aws.Int64(int64(len(obj.body))),
		})
	}
	if truncated {
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

const testBucket = "test-bucket"

// validConfigJSON is a minimal but realistic VBState blob for a freshly
// created 8 GiB volume, matching the shape CreateVolume actually persists
// (verified against a real predastore fixture).
const validConfigJSON = `{
	"VolumeName": "vol-abc123",
	"VolumeSize": 8589934592,
	"BlockSize": 4096,
	"ObjBlockSize": 4194304,
	"SeqNum": 0,
	"ObjectNum": 0,
	"WALNum": 0,
	"Version": 1,
	"VolumeConfig": {
		"VolumeMetadata": {
			"VolumeID": "vol-abc123",
			"TenantID": "000000000000",
			"SizeGiB": 8,
			"State": "available",
			"AvailabilityZone": "us-east-1a",
			"VolumeType": "gp3"
		}
	}
}`

func TestInspect_FreshVolumeNoChunks(t *testing.T) {
	store := newFakeStore()
	store.put(testBucket, "vol-abc123/config.json", []byte(validConfigJSON))

	scanner := NewScanner(store, testBucket)
	rep, err := scanner.Inspect(context.Background(), "vol-abc123")
	require.NoError(t, err)

	assert.Equal(t, "vol-abc123", rep.Volume)
	assert.Equal(t, uint32(4096), rep.State.BlockSize)
	assert.Equal(t, uint64(8589934592), rep.State.VolumeSize)
	assert.Equal(t, uint64(0), rep.State.ObjectNum)
	assert.Equal(t, "vol-abc123", rep.State.VolumeConfig.VolumeMetadata.VolumeID)
	assert.Equal(t, uint64(8), rep.State.VolumeConfig.VolumeMetadata.SizeGiB)
	assert.Equal(t, "available", rep.State.VolumeConfig.VolumeMetadata.State)

	assert.Zero(t, rep.LiveChunkCount, "fresh volume should report no live chunks")
	assert.Zero(t, rep.LiveChunkBytes, "fresh volume should report no live chunk bytes")
}

func TestInspect_CountsChunkObjectsAndIgnoresOthers(t *testing.T) {
	store := newFakeStore()
	store.put(testBucket, "vol-xyz/config.json", []byte(validConfigJSON))
	store.put(testBucket, "vol-xyz/chunks/chunk.00000000.bin", make([]byte, 100))
	store.put(testBucket, "vol-xyz/chunks/chunk.00000001.bin", make([]byte, 250))
	// Not a chunk object -- must not be counted.
	store.put(testBucket, "vol-xyz/state.json", []byte(`{}`))
	// Malformed chunk-like key -- must be ignored by the regex, not error.
	store.put(testBucket, "vol-xyz/chunks/not-a-chunk.bin", make([]byte, 999))

	scanner := NewScanner(store, testBucket)
	rep, err := scanner.Inspect(context.Background(), "vol-xyz")
	require.NoError(t, err)

	assert.Equal(t, 2, rep.LiveChunkCount)
	assert.Equal(t, int64(350), rep.LiveChunkBytes)
}

func TestInspect_PaginatesChunkListing(t *testing.T) {
	store := newFakeStore()
	store.maxKeysPerPage = 1
	store.put(testBucket, "vol-page/config.json", []byte(validConfigJSON))
	store.put(testBucket, "vol-page/chunks/chunk.00000000.bin", make([]byte, 10))
	store.put(testBucket, "vol-page/chunks/chunk.00000001.bin", make([]byte, 20))
	store.put(testBucket, "vol-page/chunks/chunk.00000002.bin", make([]byte, 30))

	scanner := NewScanner(store, testBucket)
	rep, err := scanner.Inspect(context.Background(), "vol-page")
	require.NoError(t, err)

	assert.Equal(t, 3, rep.LiveChunkCount, "pagination must not drop or double count objects")
	assert.Equal(t, int64(60), rep.LiveChunkBytes)
}

func TestInspect_MissingConfigIsError(t *testing.T) {
	store := newFakeStore()
	scanner := NewScanner(store, testBucket)

	_, err := scanner.Inspect(context.Background(), "vol-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.json")
}

func TestInspect_MalformedConfigIsError(t *testing.T) {
	store := newFakeStore()
	store.put(testBucket, "vol-bad/config.json", []byte("not json"))
	scanner := NewScanner(store, testBucket)

	_, err := scanner.Inspect(context.Background(), "vol-bad")
	require.Error(t, err)
}

func TestInspect_ListErrorPropagates(t *testing.T) {
	store := newFakeStore()
	store.put(testBucket, "vol-listerr/config.json", []byte(validConfigJSON))
	store.listErr = errors.New("simulated list failure")

	scanner := NewScanner(store, testBucket)
	_, err := scanner.Inspect(context.Background(), "vol-listerr")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated list failure")
}
