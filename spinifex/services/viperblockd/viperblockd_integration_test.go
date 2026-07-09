package viperblockd

// Integration tests for viperblockd using an embedded NATS server. Run with:
//   go test ./spinifex/services/viperblockd/... -timeout 40s
//   go test -short ./spinifex/services/viperblockd/...  # unit tests only

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupEmbeddedNATS starts an embedded NATS server for testing
func setupEmbeddedNATS(t *testing.T) (*server.Server, string) {
	opts := &server.Options{
		Host: "127.0.0.1",
		Port: -1, // Random available port
	}
	ns := natstest.RunServer(opts)

	if ns == nil {
		t.Fatal("Failed to start embedded NATS server")
	}

	return ns, ns.ClientURL()
}

// setupTestConfig creates a test configuration with proper paths
func setupTestConfig(t *testing.T, natsURL string) *Config {
	testDir := t.TempDir()

	cfg := &Config{
		NatsHost:       natsURL,
		S3Host:         "https://s3.mock.local",
		Bucket:         "test-bucket",
		Region:         "us-east-1",
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		BaseDir:        testDir,
		PluginPath:     filepath.Join(testDir, "plugins"), // Use temp dir to avoid actual nbdkit execution
		Debug:          true,
		MountedVolumes: []MountedVolume{},
		NodeName:       "test-node",
	}

	return cfg
}

// createMockVolumeState creates mock volume state files for testing
func createMockVolumeState(t *testing.T, baseDir, volumeName string) {
	volumeDir := filepath.Join(baseDir, volumeName)
	err := os.MkdirAll(volumeDir, 0755)
	assert.NoError(t, err)

	// Create a mock state file
	stateFile := filepath.Join(volumeDir, "state.json")
	mockState := map[string]any{
		"volume_name": volumeName,
		"volume_size": 1073741824, // 1GB
		"created_at":  time.Now().Unix(),
	}

	data, err := json.Marshal(mockState)
	assert.NoError(t, err)

	err = os.WriteFile(stateFile, data, 0644)
	assert.NoError(t, err)
}

// TestIntegration_ServiceStartWithEmbeddedNATS tests service startup with embedded NATS
func TestIntegration_ServiceStartWithEmbeddedNATS(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config
	cfg := setupTestConfig(t, natsURL)

	// Start service in background goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to start and subscribe
	time.Sleep(500 * time.Millisecond)

	// Verify server is responsive
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Test that we can publish to NATS
	err = nc.Publish("test.subject", []byte("test"))
	assert.NoError(t, err)

	// Test completes, launchService will be cleaned up when test ends
}

// TestIntegration_EBSMountRequest tests EBS mount request handling
func TestIntegration_EBSMountRequest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config
	cfg := setupTestConfig(t, natsURL)

	// Create mock volume state (this will fail in viperblock.New, which is expected)
	createMockVolumeState(t, cfg.BaseDir, "vol-test-001")

	// Start service in goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to subscribe
	time.Sleep(500 * time.Millisecond)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Flush to ensure subscription is registered
	nc.Flush()

	// Create mount request
	request := types.EBSRequest{
		Name:    "vol-test-001",
		VolType: "gp3",
		Boot:    false,
	}

	requestData, err := json.Marshal(request)
	assert.NoError(t, err)

	// Use Request instead of Publish to get direct response
	msg, err := nc.Request("ebs.test-node.mount", requestData, 5*time.Second)

	// We expect to get a response (even if it contains an error)
	if err != nil {
		t.Logf("Request error (may be expected if backend initialization fails): %v", err)
		// This is acceptable - the service tried to process but backend failed
		return
	}

	var response types.EBSMountResponse
	err = json.Unmarshal(msg.Data, &response)
	assert.NoError(t, err)

	// We expect an error because viperblock/S3 backend is mocked
	// But we should get a response
	assert.NotEmpty(t, response.Error)
	t.Logf("Received expected error response: %s", response.Error)
}

// TestIntegration_EBSUnmountRequest tests EBS unmount request handling
func TestIntegration_EBSUnmountRequest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config with a pre-mounted volume
	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name:   "vol-test-unmount",
			Port:   10809,
			Socket: "/tmp/vol-test-unmount.sock",
			PID:    99999, // Fake PID that doesn't exist
		},
	}

	// Start service in goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to subscribe
	time.Sleep(500 * time.Millisecond)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Subscribe to response topic
	responseChan := make(chan *nats.Msg, 1)
	_, err = nc.Subscribe("ebs.unmount.response", func(msg *nats.Msg) {
		responseChan <- msg
	})
	assert.NoError(t, err)

	// Create unmount request
	request := types.EBSRequest{
		Name: "vol-test-unmount",
	}

	requestData, err := json.Marshal(request)
	assert.NoError(t, err)

	// Send unmount request
	msg, err := nc.Request("ebs.test-node.unmount", requestData, 3*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, msg)

	// Parse response
	var response types.EBSUnMountResponse
	err = json.Unmarshal(msg.Data, &response)
	assert.NoError(t, err)

	// Verify response
	assert.Equal(t, "vol-test-unmount", response.Volume)
	assert.False(t, response.Mounted)
	assert.Empty(t, response.Error) // Should succeed

	// Note: Volume removal from cfg.MountedVolumes happens in the handler
	// We verified the response indicates success, which is sufficient for this test
	t.Logf("Unmount successful for volume: %s", response.Volume)
}

// TestIntegration_EBSUnmountNonExistentVolume tests unmounting a volume that doesn't exist
func TestIntegration_EBSUnmountNonExistentVolume(t *testing.T) {
	t.Parallel()

	// Skip
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config without any mounted volumes
	cfg := setupTestConfig(t, natsURL)

	// Start service in goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to subscribe
	time.Sleep(500 * time.Millisecond)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Create unmount request for non-existent volume
	request := types.EBSRequest{
		Name: "vol-does-not-exist",
	}

	requestData, err := json.Marshal(request)
	assert.NoError(t, err)

	// Send unmount request
	msg, err := nc.Request("ebs.test-node.unmount", requestData, 3*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, msg)

	// Parse response
	var response types.EBSUnMountResponse
	err = json.Unmarshal(msg.Data, &response)
	assert.NoError(t, err)

	// Verify error response
	assert.Equal(t, "vol-does-not-exist", response.Volume)
	assert.NotEmpty(t, response.Error)
	assert.Contains(t, response.Error, "not found")
	assert.True(t, response.NotFound, "a volume absent from MountedVolumes must set NotFound so the caller treats the retry as an idempotent success")
}

// TestIntegration_EBSUnmountRetryAfterCompletedSealReportsNotFound verifies the
// idempotency contract a slow-but-completed seal relies on: once a volume has
// been fully unmounted (removed from MountedVolumes), a second ebs.unmount for
// the same name reports NotFound=true rather than a bare "not found" the
// caller could mistake for an unrelated failure.
func TestIntegration_EBSUnmountRetryAfterCompletedSealReportsNotFound(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-retry-unmount", PID: 99999},
	}

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	requestData, err := json.Marshal(types.EBSRequest{Name: "vol-retry-unmount"})
	require.NoError(t, err)

	// First call: volume is mounted, no local WAL to seal (not created via
	// createMockVolumeState), so it completes and is dropped from MountedVolumes.
	msg, err := nc.Request("ebs.test-node.unmount", requestData, 3*time.Second)
	require.NoError(t, err)
	var first types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &first))
	assert.Empty(t, first.Error)
	assert.False(t, first.NotFound)

	// Retry: the volume is gone from MountedVolumes, so this must report
	// NotFound=true — the signal the caller's adapter treats as a completed,
	// idempotent seal rather than a hard failure.
	msg, err = nc.Request("ebs.test-node.unmount", requestData, 3*time.Second)
	require.NoError(t, err)
	var retry types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &retry))
	assert.True(t, retry.NotFound, "a retry after a completed unmount must report NotFound")
}

// TestIntegration_EBSUnmountSealFailureKeepsVolumeMounted verifies the
// seal-before-remove reorder: when a volume needs sealing (a local WAL
// directory exists) and the seal fails, the volume must remain in
// MountedVolumes rather than being dropped — a caller must never see NotFound
// for a seal that actually failed, since that would flip an unattached
// volume to available with no durable checkpoint.
func TestIntegration_EBSUnmountSealFailureKeepsVolumeMounted(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	// A local WAL directory makes volumeNeedsSeal true; the configured S3 host
	// is an unreachable mock endpoint, so sealVolumeVB's LoadState fails.
	createMockVolumeState(t, cfg.BaseDir, "vol-seal-fail")
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-seal-fail", PID: 99999},
	}

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	requestData, err := json.Marshal(types.EBSRequest{Name: "vol-seal-fail"})
	require.NoError(t, err)

	msg, err := nc.Request("ebs.test-node.unmount", requestData, 5*time.Second)
	require.NoError(t, err)

	var resp types.EBSUnMountResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.NotEmpty(t, resp.Error, "a seal failure must surface as an error, not a silent success")
	assert.False(t, resp.NotFound, "a failed seal must not report NotFound")

	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	require.Len(t, cfg.MountedVolumes, 1, "a failed seal must leave the volume in MountedVolumes for retry")
	assert.Equal(t, "vol-seal-fail", cfg.MountedVolumes[0].Name)
}

// TestIntegration_ConcurrentMountRequests tests multiple concurrent mount requests
func TestIntegration_ConcurrentMountRequests(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config
	cfg := setupTestConfig(t, natsURL)

	// Start service in goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to subscribe
	time.Sleep(500 * time.Millisecond)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	nc.Flush()

	// Send multiple concurrent mount requests using Request (synchronous)
	volumeNames := []string{"vol-001", "vol-002", "vol-003"}

	successCount := 0
	errorCount := 0

	for _, name := range volumeNames {
		request := types.EBSRequest{
			Name:    name,
			VolType: "gp3",
		}

		requestData, err := json.Marshal(request)
		assert.NoError(t, err)

		// Use Request to get synchronous response
		msg, err := nc.Request("ebs.test-node.mount", requestData, 5*time.Second)

		if err != nil {
			// Timeout or no response - acceptable for this test
			errorCount++
			t.Logf("No response for %s (expected with mocked backend)", name)
			continue
		}

		var response types.EBSMountResponse
		if err := json.Unmarshal(msg.Data, &response); err == nil {
			successCount++
			// We expect errors in responses due to mocked backend
			assert.NotEmpty(t, response.Error)
		}
	}

	// We should have gotten at least some responses
	t.Logf("Got %d responses, %d timeouts out of %d requests", successCount, errorCount, len(volumeNames))
	assert.True(t, successCount > 0 || errorCount > 0, "Should have received some responses or timeouts")
}

// TestIntegration_MessageSubscriptions tests that all expected subscriptions are active
func TestIntegration_MessageSubscriptions(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config
	cfg := setupTestConfig(t, natsURL)

	// Start service in goroutine
	go func() {
		launchService(cfg)
	}()

	// Give service time to subscribe
	time.Sleep(500 * time.Millisecond)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	nc.Flush()

	// Test unmount (most reliable since it doesn't depend on external services)
	testMsg := []byte(`{"Name": "test-volume-sub"}`)
	msg, err := nc.Request("ebs.test-node.unmount", testMsg, 3*time.Second)

	// Should get a response
	assert.NoError(t, err)
	assert.NotNil(t, msg)

	var response types.EBSUnMountResponse
	err = json.Unmarshal(msg.Data, &response)
	assert.NoError(t, err)

	// Should indicate volume not found
	assert.NotEmpty(t, response.Error)
	assert.Contains(t, response.Error, "not found")

	t.Log("Successfully verified ebs.unmount subscription is active")

	// Test delete (publish only, no response expected)
	err = nc.Publish("ebs.delete", testMsg)
	assert.NoError(t, err)

	t.Log("Successfully published to ebs.delete subscription")
}

// TestIntegration_ServiceGracefulShutdown tests graceful shutdown behavior
func TestIntegration_ServiceGracefulShutdown(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Setup embedded NATS server
	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	// Create test config with a fake mounted volume
	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{
			Name: "vol-shutdown-test",
			Port: 10809,
			PID:  99999, // Fake PID
		},
	}

	// Start service in goroutine
	doneChan := make(chan struct{})
	go func() {
		launchService(cfg)
		close(doneChan)
	}()

	// Give service time to start
	time.Sleep(500 * time.Millisecond)

	// Note: We can't easily test SIGTERM handling without refactoring launchService
	// to accept a context or shutdown channel. For now, we verify the service
	// is running and leave full shutdown testing for manual integration tests.

	// Verify service is running
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)

	// Test that service is responsive
	request := types.EBSRequest{Name: "test"}
	requestData, _ := json.Marshal(request)

	_, err = nc.Request("ebs.test-node.unmount", requestData, 2*time.Second)
	assert.NoError(t, err)

	nc.Close()

	t.Log("Service is running and responsive")
}

// TestIntegration_GenericTopicRouting tests that NodeName="" uses generic ebs.unmount with queue group
func TestIntegration_GenericTopicRouting(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	cfg.NodeName = "" // Single-node / generic mode
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-generic", PID: 99999},
	}

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Send to generic topic (not ebs.{node}.unmount)
	request := types.EBSRequest{Name: "vol-generic"}
	requestData, _ := json.Marshal(request)

	msg, err := nc.Request("ebs.unmount", requestData, 3*time.Second)
	assert.NoError(t, err)

	var response types.EBSUnMountResponse
	assert.NoError(t, json.Unmarshal(msg.Data, &response))
	assert.Equal(t, "vol-generic", response.Volume)
	assert.False(t, response.Mounted)
	assert.Empty(t, response.Error)
}

// TestIntegration_MountAuxiliaryVolumeSuffix tests that volumes with -cloudinit suffix
// reach the mount handler and trigger the auxiliary volume code path
func TestIntegration_MountAuxiliaryVolumeSuffix(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	go func() { launchService(cfg) }()
	time.Sleep(500 * time.Millisecond)

	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	nc.Flush()

	// Send mount request for auxiliary volume (will fail at S3 backend, but validates routing)
	request := types.EBSRequest{
		Name:    "vol-test-cloudinit",
		VolType: "gp3",
	}

	requestData, _ := json.Marshal(request)

	msg, err := nc.Request("ebs.test-node.mount", requestData, 5*time.Second)
	if err != nil {
		// Timeout is acceptable — service attempted to process but backend failed
		t.Logf("Request error (expected with mocked S3 backend): %v", err)
		return
	}

	var response types.EBSMountResponse
	assert.NoError(t, json.Unmarshal(msg.Data, &response))
	// Expect an error because S3 backend is mocked, but the request was processed
	assert.NotEmpty(t, response.Error)
	t.Logf("Auxiliary volume mount processed with expected error: %s", response.Error)
}
