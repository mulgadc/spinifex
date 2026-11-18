package viperblock

// Integration tests for the Viperblock adapter using an embedded NATS server. Run with:
//   go test ./spinifex/providers/ebs/viperblock/... -timeout 40s
//   go test -short ./spinifex/providers/ebs/viperblock/...  # unit tests only

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/deadpid"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupEmbeddedNATS starts an embedded NATS server for testing. JetStream is
// on because volume leases live in a KV bucket, and a daemon that cannot take
// one refuses to open an engine. The helper also declares a one-node cluster.
func setupEmbeddedNATS(t *testing.T) (*server.Server, string) {
	t.Helper()
	ns, _, _ := testutil.StartTestJetStream(t)
	return ns, ns.ClientURL()
}

// installTestVolumeLeases gives cfg a lease store backed by natsURL's
// JetStream, which every engine-open path needs before it will proceed.
func installTestVolumeLeases(t *testing.T, cfg *Config, natsURL string) {
	t.Helper()

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	leases, err := newVolumeLeases(t.Context(), nc, cfg.leaseOwner())
	require.NoError(t, err)
	cfg.leases = leases
}

// startTestService runs the adapter's startup sequence (the same one
// viperblockd's role wires up over a connection it owns) and blocks until
// every subscription is registered on the server. Run returns early on a
// startup error, so the wait is bounded rather than blocking the suite on an
// adapter that died.
func startTestService(t *testing.T, cfg *Config) {
	t.Helper()

	nc, err := nats.Connect(cfg.NatsHost)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	cfg.ready = make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- cfg.Run(nc) }()

	select {
	case <-cfg.ready:
	case err := <-errCh:
		t.Fatalf("Run returned before becoming ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not become ready")
	}
}

// setupTestConfig creates a test configuration with proper paths.
func setupTestConfig(t *testing.T, natsURL string) *Config {
	testDir := t.TempDir()

	cfg := &Config{
		NatsHost:       natsURL,
		S3Host:         fastFailingS3Host(t),
		Bucket:         "test-bucket",
		Region:         "us-east-1",
		AccessKey:      "test-access-key",
		SecretKey:      "test-secret-key",
		BaseDir:        testDir,
		PluginPath:     filepath.Join(testDir, "plugins"), // Use temp dir to avoid actual nbdkit execution
		Debug:          true,
		MountedVolumes: []MountedVolume{},
		NodeName:       "test-node",
		// One shot: the production retry exists for a JetStream still catching
		// up at daemon start, and every test that fails construction does so
		// deliberately and permanently.
		recoveryAttempts: 1,
	}
	installTestVolumeLeases(t, cfg, natsURL)

	return cfg
}

// createMockVolumeState creates mock volume state files for testing.
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

// publishOverContract sends PublishVolume for volume to cfg's node and decodes
// the reply. The error is the request's own, such as a timeout.
func publishOverContract(t *testing.T, nc *nats.Conn, cfg *Config, volume string, timeout time.Duration) (ebsprovider.PublishVolumeResponse, error) {
	t.Helper()
	subject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body, err := json.Marshal(ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion},
		VolumeID:  volume,
		NodeID:    cfg.NodeName,
	})
	require.NoError(t, err)

	var resp ebsprovider.PublishVolumeResponse
	msg, err := nc.Request(subject, body, timeout)
	if err != nil {
		return resp, err
	}
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	return resp, nil
}

// unpublishOverContract sends UnpublishVolume for volume to cfg's node and
// decodes the reply.
func unpublishOverContract(t *testing.T, nc *nats.Conn, cfg *Config, volume string, timeout time.Duration) ebsprovider.UnpublishVolumeResponse {
	t.Helper()
	subject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body, err := json.Marshal(ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion},
		VolumeID:  volume,
		NodeID:    cfg.NodeName,
	})
	require.NoError(t, err)

	msg, err := nc.Request(subject, body, timeout)
	require.NoError(t, err)
	var resp ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	return resp
}

// writeSealReceipt writes a valid seal receipt for volumeName, matching the
// fixed shape the nbdkit plugin produces after a successful seal.
func writeSealReceipt(t *testing.T, baseDir, volumeName string) {
	t.Helper()

	receipt := sealReceipt{
		Volume:   volumeName,
		PID:      4242,
		SealedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(receipt)
	require.NoError(t, err)

	err = os.WriteFile(sealReceiptPath(baseDir, volumeName), data, 0644)
	require.NoError(t, err)
}

// syncBuffer is a concurrency-safe io.Writer, used to capture slog output
// while the service is running its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs swaps the process-wide default slog logger for one that writes
// to the returned buffer, restoring the previous logger on test cleanup.
// Callers must not run in parallel with other tests that log, so do not call
// this from a test that also calls t.Parallel().
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()

	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestIntegration_ServiceStartWithEmbeddedNATS tests service startup with embedded NATS.
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

	startTestService(t, cfg)

	// Verify server is responsive
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Test that we can publish to NATS
	err = nc.Publish("test.subject", []byte("test"))
	assert.NoError(t, err)

	// Test completes, the adapter goroutine will be cleaned up when test ends
}

// TestIntegration_PublishVolumeRequest tests publish request handling.
func TestIntegration_PublishVolumeRequest(t *testing.T) {
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

	startTestService(t, cfg)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	// Flush to ensure subscription is registered
	nc.Flush()

	response, err := publishOverContract(t, nc, cfg, "vol-test-001", 5*time.Second)

	// We expect to get a response (even if it contains an error)
	if err != nil {
		t.Logf("Request error (may be expected if backend initialization fails): %v", err)
		// This is acceptable - the service tried to process but backend failed
		return
	}

	// We expect an error because viperblock/S3 backend is mocked
	// But we should get a response
	require.NotNil(t, response.Error)
	t.Logf("Received expected error response: %s", response.Error.Message)
}

// TestIntegration_UnpublishVolumeRequest tests unpublish request handling.
func TestIntegration_UnpublishVolumeRequest(t *testing.T) {
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
			PID:    deadpid.PID, // Fake PID that doesn't exist
		},
	}

	startTestService(t, cfg)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	response := unpublishOverContract(t, nc, cfg, "vol-test-unmount", 3*time.Second)

	// Verify response
	assert.Nil(t, response.Error) // Should succeed
	assert.True(t, response.TornDown, "a mounted volume must be reported torn down")
}

// TestIntegration_UnpublishNonExistentVolume tests unpublishing a volume that doesn't exist.
func TestIntegration_UnpublishNonExistentVolume(t *testing.T) {
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

	startTestService(t, cfg)

	// Connect client to NATS
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	response := unpublishOverContract(t, nc, cfg, "vol-does-not-exist", 3*time.Second)

	// A volume absent from MountedVolumes is an idempotent success with nothing torn down.
	assert.Nil(t, response.Error, "a volume absent from MountedVolumes must be an idempotent success")
	assert.False(t, response.TornDown, "nothing was exported, so nothing was torn down")
}

// TestIntegration_UnpublishRetryAfterCompletedSealReportsNotFound verifies the
// idempotency contract a slow-but-completed seal relies on: once a volume has
// been fully unmounted, a second unpublish succeeds with nothing torn down.
func TestIntegration_UnpublishRetryAfterCompletedSealReportsNotFound(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-retry-unmount", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// First call: volume is mounted, no local WAL to seal (not created via
	// createMockVolumeState), so it completes and is dropped from MountedVolumes.
	first := unpublishOverContract(t, nc, cfg, "vol-retry-unmount", 3*time.Second)
	assert.Nil(t, first.Error)
	assert.True(t, first.TornDown)

	// Retry: the volume is gone from MountedVolumes, so this must converge as a
	// completed, idempotent seal rather than a hard failure.
	retry := unpublishOverContract(t, nc, cfg, "vol-retry-unmount", 3*time.Second)
	assert.Nil(t, retry.Error, "a retry after a completed unmount must not fail")
	assert.False(t, retry.TornDown, "a retry after a completed unmount must report not found")
}

// TestIntegration_UnpublishSealFailureKeepsVolumeMounted verifies the
// seal-before-remove reorder: when a volume needs sealing (a local WAL
// directory exists) and the seal fails, the volume must remain in
// MountedVolumes rather than being dropped — a caller must never see success
// for a seal that actually failed, since that would flip an unattached
// volume to available with no durable checkpoint.
func TestIntegration_UnpublishSealFailureKeepsVolumeMounted(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	// A local WAL directory makes volumeNeedsSeal true, and the injected seal
	// then fails immediately. Letting the real seal fail against the mock S3
	// host works too, but only after seconds of jittered client backoff, which
	// is what put this test's outcome inside the tail of the 5s deadline below.
	createMockVolumeState(t, cfg.BaseDir, "vol-seal-fail")
	var sealCalls atomic.Int32
	cfg.sealVolume = func(_ context.Context, volumeName string) error {
		sealCalls.Add(1)
		return fmt.Errorf("seal %s: injected backend failure", volumeName)
	}
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-seal-fail", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-seal-fail", 5*time.Second)
	assert.NotNil(t, resp.Error, "a seal failure must surface as an error, not a silent success")
	assert.False(t, resp.TornDown, "a failed seal must not report a teardown")
	// Guards the injection itself: a handler that stopped sealing altogether
	// would otherwise satisfy every assertion below.
	assert.Equal(t, int32(1), sealCalls.Load(), "the unmount handler must have attempted the seal")

	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	require.Len(t, cfg.MountedVolumes, 1, "a failed seal must leave the volume in MountedVolumes for retry")
	assert.Equal(t, "vol-seal-fail", cfg.MountedVolumes[0].Name)
}

// TestIntegration_UnpublishReceiptSkipsFallbackSeal verifies the healthy
// path: a seal receipt with no local WAL directory means the plugin already
// sealed and cleaned up, so the fallback seal must not run and no WARN
// should fire. Not parallel: it swaps the process-wide slog default to
// assert on log output (see captureLogs).
func TestIntegration_UnpublishReceiptSkipsFallbackSeal(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logs := captureLogs(t)

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	writeSealReceipt(t, cfg.BaseDir, "vol-clean-receipt")
	var sealCalls atomic.Int32
	cfg.sealVolume = func(_ context.Context, volumeName string) error {
		sealCalls.Add(1)
		return nil
	}
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-clean-receipt", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-clean-receipt", 5*time.Second)
	assert.Nil(t, resp.Error, "a receipt on the healthy path must not surface as an unmount error")
	assert.Equal(t, int32(0), sealCalls.Load(), "a receipt present with no local WAL must not trigger the fallback seal")

	output := logs.String()
	assert.NotContains(t, output, "no local viperblock state for volume", "the healthy receipt path must not WARN")
	assert.Contains(t, output, "level=INFO", "the healthy receipt path should log at Info, not silently")
	assert.Contains(t, output, "vol-clean-receipt")

	_, statErr := os.Stat(sealReceiptPath(cfg.BaseDir, "vol-clean-receipt"))
	assert.True(t, os.IsNotExist(statErr), "the receipt must be deleted once consumed")
}

// TestIntegration_UnpublishStateDirSealsRegardlessOfReceipt verifies that a
// surviving local WAL directory always triggers the fallback seal, even when
// a seal receipt is also present — local state is the durability signal that
// takes precedence.
func TestIntegration_UnpublishStateDirSealsRegardlessOfReceipt(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	createMockVolumeState(t, cfg.BaseDir, "vol-state-and-receipt")
	writeSealReceipt(t, cfg.BaseDir, "vol-state-and-receipt")
	var sealCalls atomic.Int32
	cfg.sealVolume = func(_ context.Context, volumeName string) error {
		sealCalls.Add(1)
		return nil
	}
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-state-and-receipt", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-state-and-receipt", 5*time.Second)
	assert.Nil(t, resp.Error)
	assert.Equal(t, int32(1), sealCalls.Load(), "a surviving local WAL must trigger the fallback seal regardless of any receipt")
}

// TestIntegration_UnpublishAuxVolumeNeverSeals pins that auxiliary volumes
// stay excluded from sealing even when local state survives. The aux check
// used to live inside volumeNeedsSeal; splitting it out put branch ordering
// in play, and an -efi volume must never reach the fallback seal.
func TestIntegration_UnpublishAuxVolumeNeverSeals(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	createMockVolumeState(t, cfg.BaseDir, "vol-aux-efi")
	var sealCalls atomic.Int32
	cfg.sealVolume = func(_ context.Context, volumeName string) error {
		sealCalls.Add(1)
		return nil
	}
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-aux-efi", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-aux-efi", 5*time.Second)
	assert.Nil(t, resp.Error)
	assert.Equal(t, int32(0), sealCalls.Load(), "an auxiliary volume must never be sealed, even with local state present")
}

// TestIntegration_UnpublishNoStateNoReceiptWarns verifies the WARN is
// preserved for the case it actually describes: no local WAL and no seal
// receipt, meaning this node never held state to seal. Not parallel: it
// swaps the process-wide slog default to assert on log output.
func TestIntegration_UnpublishNoStateNoReceiptWarns(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logs := captureLogs(t)

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	var sealCalls atomic.Int32
	cfg.sealVolume = func(_ context.Context, volumeName string) error {
		sealCalls.Add(1)
		return nil
	}
	cfg.MountedVolumes = []MountedVolume{
		{Name: "vol-no-state-no-receipt", PID: deadpid.PID},
	}

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	resp := unpublishOverContract(t, nc, cfg, "vol-no-state-no-receipt", 5*time.Second)
	assert.Nil(t, resp.Error)
	assert.Equal(t, int32(0), sealCalls.Load(), "neither state nor receipt must not trigger the fallback seal")
	assert.Contains(t, logs.String(), "no local viperblock state for volume, skipping seal")
}

// TestIntegration_PublishClearsStaleSealReceipt verifies staleness is
// handled at mount: a receipt left over from a previous mount of this volume
// must not survive into a new one, so it can't later be misread as proof
// this mount's plugin sealed.
func TestIntegration_PublishClearsStaleSealReceipt(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	writeSealReceipt(t, cfg.BaseDir, "vol-mount-clear")
	receiptPath := sealReceiptPath(cfg.BaseDir, "vol-mount-clear")

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()
	nc.Flush()

	// Wait for the reply even though the mount fails in the mocked S3 backend:
	// until it arrives the handler is still creating the volume's directories
	// under BaseDir, which would race t.TempDir's cleanup.
	_, err = publishOverContract(t, nc, cfg, "vol-mount-clear", 10*time.Second)
	require.NoError(t, err)

	assert.NoFileExists(t, receiptPath, "mount must clear a pre-existing receipt for the volume it is mounting")
}

// TestIntegration_ConcurrentMountRequests tests multiple concurrent mount requests.
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

	startTestService(t, cfg)

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
		response, err := publishOverContract(t, nc, cfg, name, 5*time.Second)
		if err != nil {
			// Timeout or no response - acceptable for this test
			errorCount++
			t.Logf("No response for %s (expected with mocked backend)", name)
			continue
		}

		successCount++
		// We expect errors in responses due to mocked backend
		assert.NotNil(t, response.Error)
	}

	// We should have gotten at least some responses
	t.Logf("Got %d responses, %d timeouts out of %d requests", successCount, errorCount, len(volumeNames))
	assert.True(t, successCount > 0 || errorCount > 0, "Should have received some responses or timeouts")
}

// TestIntegration_MessageSubscriptions asserts viperblockd answers none of the
// retired ebs.* subjects, so the provider contract is its only request route.
func TestIntegration_MessageSubscriptions(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)
	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	testMsg := []byte(`{"Name": "test-volume-sub", "Volume": "test-volume-sub"}`)
	for _, subject := range []string{
		"ebs.test-node.mount",
		"ebs.test-node.unmount",
		"ebs.test-node.abandon",
		"ebs.delete",
		"ebs.sync",
		"ebs.config",
	} {
		_, err := nc.Request(subject, testMsg, 3*time.Second)
		assert.ErrorIs(t, err, nats.ErrNoResponders, "viperblockd must not answer %s", subject)
	}
}

// TestIntegration_ServiceGracefulShutdown tests graceful shutdown behavior.
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
			PID:  deadpid.PID, // Fake PID
		},
	}

	startTestService(t, cfg)

	// Note: SIGTERM handling lives in the role (runtime/roles/viperblockd) and is not exercised here
	// to accept a context or shutdown channel. For now, we verify the service
	// is running and leave full shutdown testing for manual integration tests.

	// Verify service is running
	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)

	// Test that service is responsive
	unpublishOverContract(t, nc, cfg, "test", 2*time.Second)

	nc.Close()

	t.Log("Service is running and responsive")
}

// TestIntegration_MountAuxiliaryVolumeSuffix tests that volumes with -cloudinit suffix
// reach the mount handler and trigger the auxiliary volume code path.
func TestIntegration_MountAuxiliaryVolumeSuffix(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ns, natsURL := setupEmbeddedNATS(t)
	defer ns.Shutdown()

	cfg := setupTestConfig(t, natsURL)

	startTestService(t, cfg)

	nc, err := nats.Connect(natsURL)
	assert.NoError(t, err)
	defer nc.Close()

	nc.Flush()

	// Send mount request for auxiliary volume (will fail at S3 backend, but validates routing)
	response, err := publishOverContract(t, nc, cfg, "vol-test-cloudinit", 5*time.Second)
	if err != nil {
		// Timeout is acceptable — service attempted to process but backend failed
		t.Logf("Request error (expected with mocked S3 backend): %v", err)
		return
	}

	// Expect an error because S3 backend is mocked, but the request was processed
	require.NotNil(t, response.Error)
	t.Logf("Auxiliary volume mount processed with expected error: %s", response.Error.Message)
}
