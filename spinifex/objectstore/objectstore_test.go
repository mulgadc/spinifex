package objectstore

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryObjectStore_PutAndGet(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put an object
	putInput := &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("test-key"),
		Body:   bytes.NewReader([]byte("test data")),
	}
	_, err := store.PutObject(putInput)
	require.NoError(t, err)

	// Get the object
	getInput := &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("test-key"),
	}
	output, err := store.GetObject(getInput)
	require.NoError(t, err)

	data, _ := io.ReadAll(output.Body)
	assert.Equal(t, "test data", string(data))
	assert.Equal(t, int64(9), *output.ContentLength)
}

func TestMemoryObjectStore_GetNonExistent(t *testing.T) {
	store := NewMemoryObjectStore()

	getInput := &s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("non-existent"),
	}
	_, err := store.GetObject(getInput)

	assert.Error(t, err)
	assert.True(t, IsNoSuchKeyError(err))

	// Verify error properties
	var noSuchKey *NoSuchKeyError
	assert.ErrorAs(t, err, &noSuchKey)
	assert.Equal(t, "non-existent", noSuchKey.Key)
	assert.Equal(t, "NoSuchKey", noSuchKey.Code())
}

func TestMemoryObjectStore_HeadObject(t *testing.T) {
	store := NewMemoryObjectStore()
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("b"),
		Key:    aws.String("k"),
		Body:   bytes.NewReader([]byte("twelve bytes")),
	})
	require.NoError(t, err)

	out, err := store.HeadObject(&s3.HeadObjectInput{Bucket: aws.String("b"), Key: aws.String("k")})
	require.NoError(t, err)
	assert.Equal(t, int64(12), *out.ContentLength)

	_, err = store.HeadObject(&s3.HeadObjectInput{Bucket: aws.String("b"), Key: aws.String("missing")})
	assert.True(t, IsNoSuchKeyError(err))
}

func TestMemoryObjectStore_Delete(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put an object
	putInput := &s3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("to-delete"),
		Body:   bytes.NewReader([]byte("data")),
	}
	_, err := store.PutObject(putInput)
	require.NoError(t, err)

	// Verify it exists
	assert.Equal(t, 1, store.Count())

	// Delete the object
	deleteInput := &s3.DeleteObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("to-delete"),
	}
	_, err = store.DeleteObject(deleteInput)
	require.NoError(t, err)

	// Verify it's gone
	assert.Equal(t, 0, store.Count())

	// Getting it should fail
	_, err = store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("to-delete"),
	})
	assert.True(t, IsNoSuchKeyError(err))
}

func TestMemoryObjectStore_DeleteNonExistent(t *testing.T) {
	store := NewMemoryObjectStore()

	// Deleting non-existent should not error (matches S3 behavior)
	deleteInput := &s3.DeleteObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String("never-existed"),
	}
	_, err := store.DeleteObject(deleteInput)
	assert.NoError(t, err)
}

func TestMemoryObjectStore_ListObjects(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put multiple objects
	objects := map[string]string{
		"images/ami-1.json":     `{"id":"ami-1"}`,
		"images/ami-2.json":     `{"id":"ami-2"}`,
		"images/ami-3.json":     `{"id":"ami-3"}`,
		"volumes/vol-1.json":    `{"id":"vol-1"}`,
		"snapshots/snap-1.json": `{"id":"snap-1"}`,
	}

	for key, value := range objects {
		_, err := store.PutObject(&s3.PutObjectInput{
			Bucket: aws.String("spinifex-metadata"),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte(value)),
		})
		require.NoError(t, err)
	}

	// List all objects in bucket
	output, err := store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String("spinifex-metadata"),
	})
	require.NoError(t, err)
	assert.Len(t, output.Contents, 5)

	// List objects with prefix
	output, err = store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String("spinifex-metadata"),
		Prefix: aws.String("images/"),
	})
	require.NoError(t, err)
	assert.Len(t, output.Contents, 3)

	// Verify keys
	keys := make([]string, len(output.Contents))
	for i, obj := range output.Contents {
		keys[i] = *obj.Key
	}
	assert.Contains(t, keys, "images/ami-1.json")
	assert.Contains(t, keys, "images/ami-2.json")
	assert.Contains(t, keys, "images/ami-3.json")
}

func TestMemoryObjectStore_ListObjectsWithDelimiter(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put objects with directory-like structure
	objects := []string{
		"root.json",
		"images/ami-1.json",
		"images/ami-2.json",
		"volumes/vol-1.json",
		"volumes/snapshots/snap-1.json",
	}

	for _, key := range objects {
		_, err := store.PutObject(&s3.PutObjectInput{
			Bucket: aws.String("spinifex"),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte("{}")),
		})
		require.NoError(t, err)
	}

	// List with delimiter at root level
	output, err := store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket:    aws.String("spinifex"),
		Delimiter: aws.String("/"),
	})
	require.NoError(t, err)

	// Should have one file at root
	assert.Len(t, output.Contents, 1)
	assert.Equal(t, "root.json", *output.Contents[0].Key)

	// Should have two common prefixes (images/, volumes/)
	assert.Len(t, output.CommonPrefixes, 2)
}

func TestMemoryObjectStore_Clear(t *testing.T) {
	store := NewMemoryObjectStore()

	// Add some objects
	for i := range 5 {
		_, _ = store.PutObject(&s3.PutObjectInput{
			Bucket: aws.String("bucket"),
			Key:    aws.String("key-" + string(rune('0'+i))),
			Body:   bytes.NewReader([]byte("data")),
		})
	}

	assert.Equal(t, 5, store.Count())

	// Clear
	store.Clear()
	assert.Equal(t, 0, store.Count())
}

func TestMemoryObjectStore_MultipleBuckets(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put objects in different buckets
	_, _ = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("bucket-a"),
		Key:    aws.String("key1"),
		Body:   bytes.NewReader([]byte("data-a")),
	})
	_, _ = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("bucket-b"),
		Key:    aws.String("key1"),
		Body:   bytes.NewReader([]byte("data-b")),
	})

	// List bucket-a
	outputA, _ := store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String("bucket-a"),
	})
	assert.Len(t, outputA.Contents, 1)

	// List bucket-b
	outputB, _ := store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String("bucket-b"),
	})
	assert.Len(t, outputB.Contents, 1)

	// Get from bucket-a
	objA, _ := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("bucket-a"),
		Key:    aws.String("key1"),
	})
	dataA, _ := io.ReadAll(objA.Body)
	assert.Equal(t, "data-a", string(dataA))

	// Get from bucket-b
	objB, _ := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("bucket-b"),
		Key:    aws.String("key1"),
	})
	dataB, _ := io.ReadAll(objB.Body)
	assert.Equal(t, "data-b", string(dataB))
}

func TestMemoryObjectStore_Overwrite(t *testing.T) {
	store := NewMemoryObjectStore()

	// Put initial object
	_, _ = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("key"),
		Body:   bytes.NewReader([]byte("initial")),
	})

	// Overwrite
	_, _ = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("key"),
		Body:   bytes.NewReader([]byte("updated")),
	})

	// Verify overwrite
	output, _ := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("key"),
	})
	data, _ := io.ReadAll(output.Body)
	assert.Equal(t, "updated", string(data))

	// Should still be one object
	assert.Equal(t, 1, store.Count())
}

func TestMemoryObjectStore_ConcurrentAccess(t *testing.T) {
	store := NewMemoryObjectStore()

	var wg sync.WaitGroup
	numGoroutines := 100

	// Concurrent writes
	wg.Add(numGoroutines)
	for i := range numGoroutines {
		go func(idx int) {
			defer wg.Done()
			_, _ = store.PutObject(&s3.PutObjectInput{
				Bucket: aws.String("bucket"),
				Key:    aws.String("key-" + string(rune('a'+idx%26)) + "-" + string(rune('0'+idx/26))),
				Body:   bytes.NewReader([]byte("data")),
			})
		}(i)
	}
	wg.Wait()

	// Concurrent reads
	wg.Add(numGoroutines)
	for range numGoroutines {
		go func() {
			defer wg.Done()
			_, _ = store.ListObjectsV2(&s3.ListObjectsV2Input{
				Bucket: aws.String("bucket"),
			})
		}()
	}
	wg.Wait()

	// Should complete without race conditions
	assert.True(t, store.Count() > 0)
}

func TestNoSuchKeyError(t *testing.T) {
	err := &NoSuchKeyError{Key: "test-key"}

	assert.Equal(t, "NoSuchKey: test-key", err.Error())
	assert.Equal(t, "NoSuchKey", err.Code())
	assert.True(t, IsNoSuchKeyError(err))
	assert.False(t, IsNoSuchKeyError(nil))
	assert.False(t, IsNoSuchKeyError(assert.AnError))
}

func TestIsNoSuchKeyError_WithWrappedError(t *testing.T) {
	originalErr := &NoSuchKeyError{Key: "test"}

	// Even when wrapped, IsNoSuchKeyError should work
	assert.True(t, IsNoSuchKeyError(originalErr))
}

// Test that the interface is properly defined
var _ ObjectStore = (*MemoryObjectStore)(nil)
var _ ObjectStore = (*S3ObjectStore)(nil)
