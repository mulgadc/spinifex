// Package predastore_test contains integration tests for predastore: start a
// real daemon (via testpredastore.Start) and verify S3 bucket, auth, and
// file operations using AWS SDK Go v1.
package predastore_test

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	testpredastore "github.com/mulgadc/spinifex/spinifex/testutil/predastore"
	"github.com/stretchr/testify/assert"
)

// Test credentials from config, mirrored from the shared fixture
// (testpredastore.AccessKey etc.) so this file has a single source of truth
// for the daemon's seeded auth and default buckets.
const (
	validAccessKey   = testpredastore.AccessKey
	validSecretKey   = testpredastore.SecretKey
	invalidAccessKey = "INVALIDACCESSKEY"
	invalidSecretKey = "InvalidSecretKey123"
	testBucket       = testpredastore.DefaultBucket
	publicBucket     = testpredastore.DefaultBucket2
	testRegion       = testpredastore.Region
)

// createS3Client creates an AWS S3 client for testing.
func createS3Client(accessKey, secretKey string) *s3.S3 {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	sess := session.Must(session.NewSession(&aws.Config{
		Region:           aws.String(testRegion),
		Endpoint:         aws.String("https://" + testpredastore.Host),
		Credentials:      credentials.NewStaticCredentials(accessKey, secretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(false),
		HTTPClient:       httpClient,
	}))

	return s3.New(sess)
}

// TestIntegration_BucketListing tests that both buckets from config exist.
func TestIntegration_BucketListing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Start server (only starts once)
	testpredastore.Start(t)

	// Create S3 client
	client := createS3Client(validAccessKey, validSecretKey)

	// List all buckets
	result, err := client.ListBuckets(&s3.ListBucketsInput{})
	assert.NoError(t, err, "ListBuckets should not return error")
	assert.NotNil(t, result, "ListBuckets result should not be nil")

	// Verify both buckets exist
	buckets := make(map[string]bool)
	for _, bucket := range result.Buckets {
		buckets[*bucket.Name] = true
		t.Logf("Found bucket: %s", *bucket.Name)
	}

	//assert.True(t, buckets[testBucket], "test-bucket should exist")
	//assert.True(t, buckets[publicBucket], "public-bucket should exist")
	assert.Len(t, buckets, 2, "Should have exactly 2 buckets")
}

// TestIntegration_Authentication_Valid tests authentication with valid credentials.
func TestIntegration_Authentication_Valid(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	// Create client with valid credentials
	client := createS3Client(validAccessKey, validSecretKey)

	// Try to list buckets
	result, err := client.ListBuckets(&s3.ListBucketsInput{})
	assert.NoError(t, err, "Valid credentials should authenticate successfully")
	if assert.NotNil(t, result, "Result should not be nil") {
		// Just verify authentication worked - bucket count is verified in BucketListing test
		t.Logf("Authentication successful, found %d buckets", len(result.Buckets))
	}
}

// TestIntegration_Authentication_Invalid tests authentication with invalid credentials.
func TestIntegration_Authentication_Invalid(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	// Create client with invalid credentials
	client := createS3Client(invalidAccessKey, invalidSecretKey)

	// Try to list buckets - should fail
	_, err := client.ListBuckets(&s3.ListBucketsInput{})
	assert.Error(t, err, "Invalid credentials should fail authentication")

	// Verify error contains authentication-related message
	errStr := err.Error()
	assert.True(t,
		strings.Contains(errStr, "InvalidAccessKeyId") ||
			strings.Contains(errStr, "SignatureDoesNotMatch") ||
			strings.Contains(errStr, "Forbidden") ||
			strings.Contains(errStr, "403"),
		"Error should indicate authentication failure, got: %s", errStr)
}

// TestIntegration_FileUpload_TestBucket tests file upload to test-bucket.
func TestIntegration_FileUpload_TestBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	// Upload test file
	key := "test1.txt"
	expectedContent := "hello-world-test-1-" + testBucket
	body := strings.NewReader(expectedContent)

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
		Body:   body,
	})
	assert.NoError(t, err, "File upload should succeed")

	// Download and verify content
	result, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
	})
	if !assert.NoError(t, err, "File download should succeed") {
		return // Skip rest if download failed
	}
	if !assert.NotNil(t, result, "Result should not be nil") {
		return
	}

	defer result.Body.Close()
	downloadedContent, err := io.ReadAll(result.Body)
	assert.NoError(t, err, "Reading file content should succeed")
	assert.Equal(t, expectedContent, string(downloadedContent), "File content should match")

	t.Logf("Successfully uploaded and verified file: s3://%s/%s", testBucket, key)
}

// TestIntegration_FileUpload_PublicBucket tests file upload to public-bucket.
func TestIntegration_FileUpload_PublicBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	// Upload test file
	key := "test1.txt"
	expectedContent := "hello-world-test-1-" + publicBucket
	body := strings.NewReader(expectedContent)

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(publicBucket),
		Key:    aws.String(key),
		Body:   body,
	})
	assert.NoError(t, err, "File upload should succeed")

	// Download and verify content
	result, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(publicBucket),
		Key:    aws.String(key),
	})
	if !assert.NoError(t, err, "File download should succeed") {
		return
	}
	if !assert.NotNil(t, result, "Result should not be nil") {
		return
	}

	defer result.Body.Close()
	downloadedContent, err := io.ReadAll(result.Body)
	assert.NoError(t, err, "Reading file content should succeed")
	assert.Equal(t, expectedContent, string(downloadedContent), "File content should match")

	t.Logf("Successfully uploaded and verified file: s3://%s/%s", publicBucket, key)
}

func TestIntegration_FileUpload_PublicBucket2(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	time.Sleep(1 * time.Second)

	// Upload test file
	key := "test1.txt"
	expectedContent := "hello-world-test-1-" + publicBucket
	body := strings.NewReader(expectedContent)

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(publicBucket),
		Key:    aws.String(key),
		Body:   body,
	})
	assert.NoError(t, err, "File upload should succeed")

	// Download and verify content
	result, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(publicBucket),
		Key:    aws.String(key),
	})
	if !assert.NoError(t, err, "File download should succeed") {
		return
	}
	if !assert.NotNil(t, result, "Result should not be nil") {
		return
	}

	time.Sleep(1 * time.Second)

	defer result.Body.Close()
	downloadedContent, err := io.ReadAll(result.Body)
	assert.NoError(t, err, "Reading file content should succeed")
	assert.Equal(t, expectedContent, string(downloadedContent), "File content should match")

	t.Logf("Successfully uploaded and verified file: s3://%s/%s", publicBucket, key)
}

// TestIntegration_FileOperations_Complete tests full file lifecycle.
func TestIntegration_FileOperations_Complete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	// Test with both buckets
	buckets := []string{testBucket, publicBucket}

	for _, bucket := range buckets {
		t.Run(bucket, func(t *testing.T) {
			key := "lifecycle-test.txt"
			content := fmt.Sprintf("hello-world-test-1-%s", bucket)

			// 1. Upload file
			_, err := client.PutObject(&s3.PutObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
				Body:   strings.NewReader(content),
			})
			assert.NoError(t, err, "Upload should succeed")

			// 2. Verify file exists in listing
			listResult, err := client.ListObjectsV2(&s3.ListObjectsV2Input{
				Bucket: aws.String(bucket),
			})
			assert.NoError(t, err, "List should succeed")

			found := false
			for _, obj := range listResult.Contents {
				if *obj.Key == key {
					found = true
					break
				}
			}
			assert.True(t, found, "File should appear in bucket listing")

			// 3. Download and verify content
			getResult, err := client.GetObject(&s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			if !assert.NoError(t, err, "Download should succeed") {
				return
			}
			if !assert.NotNil(t, getResult, "Get result should not be nil") {
				return
			}

			downloadedContent, err := io.ReadAll(getResult.Body)
			getResult.Body.Close()
			assert.NoError(t, err, "Reading content should succeed")
			assert.Equal(t, content, string(downloadedContent), "Content should match")

			// 4. Delete file
			_, err = client.DeleteObject(&s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			assert.NoError(t, err, "Delete should succeed")

			// 5. Verify file no longer exists
			listResult2, err := client.ListObjectsV2(&s3.ListObjectsV2Input{
				Bucket: aws.String(bucket),
			})
			assert.NoError(t, err, "List after delete should succeed")

			found = false
			for _, obj := range listResult2.Contents {
				if *obj.Key == key {
					found = true
					break
				}
			}
			assert.False(t, found, "File should not appear in listing after delete")

			// 6. Verify GET returns error for deleted file
			_, err = client.GetObject(&s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(key),
			})
			assert.Error(t, err, "GET on deleted file should return error")

			t.Logf("Complete lifecycle test passed for bucket: %s", bucket)
		})
	}
}

// TestIntegration_MultipleFiles tests uploading multiple files.
func TestIntegration_MultipleFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	// Upload multiple files to test-bucket
	fileCount := 5
	keys := make([]string, fileCount)

	for i := range fileCount {
		keys[i] = fmt.Sprintf("multi-test-%d.txt", i)
		content := fmt.Sprintf("content-file-%d", i)

		_, err := client.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(testBucket),
			Key:    aws.String(keys[i]),
			Body:   strings.NewReader(content),
		})
		assert.NoError(t, err, "Upload file %d should succeed", i)
	}

	// List and verify all files exist
	listResult, err := client.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(testBucket),
	})
	assert.NoError(t, err, "List should succeed")

	foundCount := 0
	for _, obj := range listResult.Contents {
		if slices.Contains(keys, *obj.Key) {
			foundCount++
		}
	}
	assert.Equal(t, fileCount, foundCount, "All uploaded files should be listed")

	// Cleanup - delete all test files
	for _, key := range keys {
		client.DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(testBucket),
			Key:    aws.String(key),
		})
	}
}

// TestIntegration_LargeFile tests uploading a larger file (1MB).
func TestIntegration_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testpredastore.Start(t)

	client := createS3Client(validAccessKey, validSecretKey)

	// Create 1MB file
	size := 1024 * 1024
	largeData := bytes.Repeat([]byte("A"), size)

	key := "large-file-test.bin"

	// Upload
	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(largeData),
	})
	assert.NoError(t, err, "Large file upload should succeed")

	// Download and verify
	result, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
	})
	if !assert.NoError(t, err, "Large file download should succeed") {
		return
	}
	if !assert.NotNil(t, result, "Result should not be nil") {
		return
	}

	downloadedData, err := io.ReadAll(result.Body)
	result.Body.Close()
	assert.NoError(t, err, "Reading large file should succeed")
	assert.Len(t, downloadedData, size, "Downloaded size should match")
	assert.Equal(t, largeData, downloadedData, "Downloaded content should match")

	// Cleanup
	client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(key),
	})

	t.Logf("Successfully uploaded and verified 1MB file")
}
