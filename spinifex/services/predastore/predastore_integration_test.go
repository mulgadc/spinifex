package predastore

// Integration tests for predastore service
//
// These tests start a real predastore daemon and test S3 operations against it.
// Tests use AWS SDK Go v1 to verify bucket operations, authentication, and file operations.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/assert"
)

// Test credentials from config
const (
	validAccessKey   = "AKIAIOSFODNN7EXAMPLE"
	validSecretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	invalidAccessKey = "INVALIDACCESSKEY"
	invalidSecretKey = "InvalidSecretKey123"
	testBucket       = "test-bucket"
	publicBucket     = "public-bucket"
	testRegion       = "us-east-1"
	testPort         = 18443
	testHost         = "127.0.0.1"
)

// Global server instance for integration tests
var (
	serverStarted    bool
	serverStartMutex sync.Mutex
	sharedTestDir    string
	sharedConfig     *Config
)

// generateTestCertificate generates a self-signed TLS certificate for testing
func generateTestCertificate(certPath, keyPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(24 * time.Hour)

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Predastore Test"},
			CommonName:   "localhost",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return err
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}

	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})
}

// startPredastoreServer starts the predastore server once for all tests
func startPredastoreServer(t *testing.T) *Config {
	serverStartMutex.Lock()
	defer serverStartMutex.Unlock()

	if serverStarted {
		// Server already running, return shared config
		return sharedConfig
	}

	// Create persistent test environment (not using t.TempDir() for shared server)
	testDir, err := os.MkdirTemp("", "predastore-integration-*") //nolint:usetesting // shared server outlives individual test
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	sharedTestDir = testDir

	// Create TLS cert directory
	certDir := filepath.Join(testDir, "certs")
	err = os.MkdirAll(certDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create cert dir: %v", err)
	}

	certPath := filepath.Join(certDir, "test.crt")
	keyPath := filepath.Join(certDir, "test.key")

	err = generateTestCertificate(certPath, keyPath)
	if err != nil {
		t.Fatalf("Failed to generate certificate: %v", err)
	}

	// Predastore mandates a 32-byte master key at mode 0600 (rejected otherwise
	// by internal/keyfile.Load).
	encryptionKeyPath := filepath.Join(testDir, "encryption.key")
	testEncryptionKey := make([]byte, 32)
	if _, err := rand.Read(testEncryptionKey); err != nil {
		t.Fatalf("Failed to generate test encryption key: %v", err)
	}
	if err := os.WriteFile(encryptionKeyPath, testEncryptionKey, 0600); err != nil {
		t.Fatalf("Failed to write test encryption key: %v", err)
	}

	// Create config file. Five storage nodes are declared so predastore's dev-mode
	// path launches all QUIC servers locally as goroutines (server.go:629). Buckets
	// are created via the S3 API after startup — config buckets are no longer
	// surfaced by ListBuckets (predastore commit 0711e7d, account-scoped auth).
	configPath := filepath.Join(testDir, "predastore_test.toml")
	configContent := `version = "1.0"
region = "us-east-1"
host = "127.0.0.1"
port = 18443
debug = false
disable_logging = false
base_path = "` + testDir + `/"

[rs]
data = 3
parity = 2

[[nodes]]
id = 1
host = "127.0.0.1"
port = 19991
path = "store/node-1/"

[[nodes]]
id = 2
host = "127.0.0.1"
port = 19992
path = "store/node-2/"

[[nodes]]
id = 3
host = "127.0.0.1"
port = 19993
path = "store/node-3/"

[[nodes]]
id = 4
host = "127.0.0.1"
port = 19994
path = "store/node-4/"

[[nodes]]
id = 5
host = "127.0.0.1"
port = 19995
path = "store/node-5/"

[[auth]]
access_key_id = "AKIAIOSFODNN7EXAMPLE"
secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
account_id = "123456789012"
`

	err = os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write config: %v", err)
	}

	cfg := &Config{
		ConfigPath:        configPath,
		Port:              18443,
		Host:              "127.0.0.1",
		Debug:             false,
		BasePath:          testDir,
		TlsCert:           certPath,
		TlsKey:            keyPath,
		EncryptionKeyFile: encryptionKeyPath,
		// Predastore rejects NodeID 0; -1 triggers dev mode, which launches
		// every configured QUIC node in-process. The test config defines
		// five nodes, so this is the only sane choice for a single-process
		// integration test.
		NodeID: -1,
	}
	sharedConfig = cfg

	// Start server in background
	go func() {
		svc, _ := New(cfg)
		svc.Start()
	}()

	// Wait for server to be ready
	if !waitForServer(10 * time.Second) {
		t.Fatal("Failed to start predastore server")
	}

	// Create the test buckets via the S3 API so they're registered in
	// distributed globalState (config buckets aren't visible to ListBuckets).
	setupClient := createS3Client(validAccessKey, validSecretKey)
	for _, bucket := range []string{testBucket, publicBucket} {
		if _, err := setupClient.CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("Failed to create bucket %s: %v", bucket, err)
		}
	}

	serverStarted = true
	t.Logf("Predastore server started successfully")
	t.Logf("Test directory: %s", testDir)
	t.Logf("Created buckets: %s, %s", testBucket, publicBucket)

	// NOTE: Cleanup will happen automatically when test process exits
	// Don't use t.Cleanup() as it runs after each test, not at the end

	return cfg
}

// waitForServer waits for the predastore server to be ready
func waitForServer(timeout time.Duration) bool {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 1 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + net.JoinHostPort(testHost, strconv.Itoa(testPort)) + "/")
		if err == nil {
			resp.Body.Close()
			// Give a bit more time for config to load
			time.Sleep(500 * time.Millisecond)
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// createS3Client creates an AWS S3 client for testing
func createS3Client(accessKey, secretKey string) *s3.S3 {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	sess := session.Must(session.NewSession(&aws.Config{
		Region:           aws.String(testRegion),
		Endpoint:         aws.String("https://" + net.JoinHostPort(testHost, strconv.Itoa(testPort))),
		Credentials:      credentials.NewStaticCredentials(accessKey, secretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
		DisableSSL:       aws.Bool(false),
		HTTPClient:       httpClient,
	}))

	return s3.New(sess)
}

// TestIntegration_BucketListing tests that both buckets from config exist
func TestIntegration_BucketListing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Start server (only starts once)
	startPredastoreServer(t)

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

// TestIntegration_Authentication_Valid tests authentication with valid credentials
func TestIntegration_Authentication_Valid(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

// TestIntegration_Authentication_Invalid tests authentication with invalid credentials
func TestIntegration_Authentication_Invalid(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

// TestIntegration_FileUpload_TestBucket tests file upload to test-bucket
func TestIntegration_FileUpload_TestBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

// TestIntegration_FileUpload_PublicBucket tests file upload to public-bucket
func TestIntegration_FileUpload_PublicBucket(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

	startPredastoreServer(t)

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

// TestIntegration_FileOperations_Complete tests full file lifecycle
func TestIntegration_FileOperations_Complete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

// TestIntegration_MultipleFiles tests uploading multiple files
func TestIntegration_MultipleFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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

// TestIntegration_LargeFile tests uploading a larger file (1MB)
func TestIntegration_LargeFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	startPredastoreServer(t)

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
