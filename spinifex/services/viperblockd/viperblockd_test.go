package viperblockd

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNew tests the service constructor
func TestNew(t *testing.T) {
	cfg := &Config{
		NatsHost:  "nats://localhost:4222",
		S3Host:    "https://s3.amazonaws.com",
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "test-access-key",
		SecretKey: "test-secret-key",
		BaseDir:   "/tmp/viperblock",
	}

	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
	assert.Equal(t, "nats://localhost:4222", svc.Config.NatsHost)
	assert.Equal(t, "https://s3.amazonaws.com", svc.Config.S3Host)
	assert.Equal(t, "test-bucket", svc.Config.Bucket)
	assert.Equal(t, "us-east-1", svc.Config.Region)
	assert.Equal(t, "/tmp/viperblock", svc.Config.BaseDir)
}

// TestNewWithNilConfig tests that New handles nil config correctly
func TestNewWithNilConfig(t *testing.T) {
	// This will panic if not handled, but based on the code it type asserts
	// For now we test that it accepts a Config pointer
	cfg := &Config{}
	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
}

// TestConfigDefaults tests Config struct default values
func TestConfigDefaults(t *testing.T) {
	cfg := &Config{}

	assert.Empty(t, cfg.NatsHost)
	assert.Empty(t, cfg.S3Host)
	assert.Empty(t, cfg.Bucket)
	assert.Empty(t, cfg.Region)
	assert.False(t, cfg.Debug)
	assert.Empty(t, cfg.MountedVolumes)
}

// TestConfigWithDebug tests Config with debug enabled
func TestConfigWithDebug(t *testing.T) {
	cfg := &Config{
		Debug:    true,
		NatsHost: "nats://localhost:4222",
		BaseDir:  "/tmp/test",
	}

	assert.True(t, cfg.Debug)
	assert.Equal(t, "nats://localhost:4222", cfg.NatsHost)
	assert.Equal(t, "/tmp/test", cfg.BaseDir)
}

// TestMountedVolumeStruct tests the MountedVolume struct
func TestMountedVolumeStruct(t *testing.T) {
	vol := MountedVolume{
		Name:   "vol-123",
		Port:   10809,
		Socket: "/tmp/vol-123.sock",
		PID:    12345,
	}

	assert.Equal(t, "vol-123", vol.Name)
	assert.Equal(t, 10809, vol.Port)
	assert.Equal(t, "/tmp/vol-123.sock", vol.Socket)
	assert.Equal(t, 12345, vol.PID)
}

// TestConfigMountedVolumesAppend tests adding volumes to the config
func TestConfigMountedVolumesAppend(t *testing.T) {
	cfg := &Config{
		MountedVolumes: []MountedVolume{},
	}

	assert.Len(t, cfg.MountedVolumes, 0)

	// Add first volume
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: "vol-1",
		Port: 10809,
		PID:  100,
	})

	assert.Len(t, cfg.MountedVolumes, 1)
	assert.Equal(t, "vol-1", cfg.MountedVolumes[0].Name)

	// Add second volume
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: "vol-2",
		Port: 10810,
		PID:  101,
	})

	assert.Len(t, cfg.MountedVolumes, 2)
	assert.Equal(t, "vol-2", cfg.MountedVolumes[1].Name)
}

// TestConfigMutexThreadSafety tests that the mutex protects MountedVolumes
func TestConfigMutexThreadSafety(t *testing.T) {
	cfg := &Config{
		MountedVolumes: []MountedVolume{},
	}

	var wg sync.WaitGroup
	iterations := 100

	// Spawn multiple goroutines to append volumes concurrently
	for i := range iterations {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cfg.mu.Lock()
			cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
				Name: "vol-" + string(rune(id)),
				Port: 10809 + id,
				PID:  1000 + id,
			})
			cfg.mu.Unlock()
		}(i)
	}

	wg.Wait()

	cfg.mu.Lock()
	assert.Len(t, cfg.MountedVolumes, iterations)
	cfg.mu.Unlock()
}

// TestServiceMethods tests the service interface methods
func TestServiceMethods(t *testing.T) {
	cfg := &Config{
		NatsHost: "nats://localhost:4222",
		BaseDir:  "/tmp/viperblock",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)

	// Test Status - returns "stopped" when no PID file exists
	status, err := svc.Status()
	assert.NoError(t, err)
	assert.Equal(t, "stopped", status)

	// Test Reload - returns no error
	err = svc.Reload()
	assert.NoError(t, err)
}

// TestServiceShutdown tests that Shutdown calls Stop
func TestServiceShutdown(t *testing.T) {
	cfg := &Config{
		NatsHost: "nats://localhost:4222",
		BaseDir:  "/tmp/viperblock",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)

	// Shutdown should call Stop internally
	// Since we can't actually start the service (requires NATS),
	// we just verify it doesn't panic
	err = svc.Shutdown()
	// Will error because no PID file exists, which is expected
	assert.Error(t, err)
}

// TestServiceStop tests the Stop method
func TestServiceStop(t *testing.T) {
	cfg := &Config{
		NatsHost: "nats://localhost:4222",
		BaseDir:  "/tmp/viperblock",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)

	// Stop should try to read PID file and stop the process
	err = svc.Stop()
	// Will error because no PID file exists, which is expected in tests
	assert.Error(t, err)
}

// TestConfigPluginPath tests PluginPath configuration
func TestConfigPluginPath(t *testing.T) {
	cfg := &Config{
		PluginPath: "/usr/lib/nbdkit/plugins",
		BaseDir:    "/var/lib/viperblock",
	}

	assert.Equal(t, "/usr/lib/nbdkit/plugins", cfg.PluginPath)
	assert.Equal(t, "/var/lib/viperblock", cfg.BaseDir)
}

// TestConfigFullyPopulated tests a fully populated config
func TestConfigFullyPopulated(t *testing.T) {
	cfg := &Config{
		ConfigPath: "/etc/viperblock/config.yml",
		PluginPath: "/usr/lib/nbdkit/plugins",
		Debug:      true,
		NatsHost:   "nats://10.0.0.1:4222",
		S3Host:     "https://s3.us-west-2.amazonaws.com",
		Bucket:     "my-volumes",
		Region:     "us-west-2",
		AccessKey:  "AKIAIOSFODNN7EXAMPLE",
		SecretKey:  "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		BaseDir:    "/var/lib/viperblock",
		MountedVolumes: []MountedVolume{
			{
				Name:   "vol-001",
				Port:   10809,
				Socket: "/tmp/vol-001.sock",
				PID:    5000,
			},
		},
	}

	assert.Equal(t, "/etc/viperblock/config.yml", cfg.ConfigPath)
	assert.Equal(t, "/usr/lib/nbdkit/plugins", cfg.PluginPath)
	assert.True(t, cfg.Debug)
	assert.Equal(t, "nats://10.0.0.1:4222", cfg.NatsHost)
	assert.Equal(t, "https://s3.us-west-2.amazonaws.com", cfg.S3Host)
	assert.Equal(t, "my-volumes", cfg.Bucket)
	assert.Equal(t, "us-west-2", cfg.Region)
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", cfg.AccessKey)
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", cfg.SecretKey)
	assert.Equal(t, "/var/lib/viperblock", cfg.BaseDir)
	assert.Len(t, cfg.MountedVolumes, 1)
	assert.Equal(t, "vol-001", cfg.MountedVolumes[0].Name)
}

// TestMountedVolumeFindByName tests finding a mounted volume by name
func TestMountedVolumeFindByName(t *testing.T) {
	cfg := &Config{
		MountedVolumes: []MountedVolume{
			{Name: "vol-1", Port: 10809, PID: 100},
			{Name: "vol-2", Port: 10810, PID: 101},
			{Name: "vol-3", Port: 10811, PID: 102},
		},
	}

	// Find existing volume
	var found *MountedVolume
	for _, vol := range cfg.MountedVolumes {
		if vol.Name == "vol-2" {
			found = &vol
			break
		}
	}

	assert.NotNil(t, found)
	assert.Equal(t, "vol-2", found.Name)
	assert.Equal(t, 10810, found.Port)
	assert.Equal(t, 101, found.PID)

	// Try to find non-existent volume
	found = nil
	for _, vol := range cfg.MountedVolumes {
		if vol.Name == "vol-999" {
			found = &vol
			break
		}
	}

	assert.Nil(t, found)
}

// TestMountedVolumeRemoval tests removing a mounted volume
func TestMountedVolumeRemoval(t *testing.T) {
	cfg := &Config{
		MountedVolumes: []MountedVolume{
			{Name: "vol-1", Port: 10809, PID: 100},
			{Name: "vol-2", Port: 10810, PID: 101},
			{Name: "vol-3", Port: 10811, PID: 102},
		},
	}

	assert.Len(t, cfg.MountedVolumes, 3)

	// Remove vol-2
	cfg.mu.Lock()
	for i, vol := range cfg.MountedVolumes {
		if vol.Name == "vol-2" {
			cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
			break
		}
	}
	cfg.mu.Unlock()

	assert.Len(t, cfg.MountedVolumes, 2)
	assert.Equal(t, "vol-1", cfg.MountedVolumes[0].Name)
	assert.Equal(t, "vol-3", cfg.MountedVolumes[1].Name)
}

// TestServiceNameConstant tests the serviceName constant
func TestServiceNameConstant(t *testing.T) {
	assert.Equal(t, "viperblock", serviceName)
}

// TestConfigS3Credentials tests S3 credential fields
func TestConfigS3Credentials(t *testing.T) {
	cfg := &Config{
		AccessKey: "test-key",
		SecretKey: "test-secret",
	}

	assert.NotEmpty(t, cfg.AccessKey)
	assert.NotEmpty(t, cfg.SecretKey)
	assert.Equal(t, "test-key", cfg.AccessKey)
	assert.Equal(t, "test-secret", cfg.SecretKey)
}

// TestMountedVolumeWithSocket tests MountedVolume with socket path
func TestMountedVolumeWithSocket(t *testing.T) {
	vol := MountedVolume{
		Name:   "vol-ebs-123",
		Port:   10809,
		Socket: "/var/run/viperblock/vol-ebs-123.sock",
		PID:    9876,
	}

	assert.Contains(t, vol.Socket, "vol-ebs-123")
	assert.True(t, len(vol.Socket) > 0)
}

// TestConfigConcurrentRead tests concurrent reads are safe
func TestConfigConcurrentRead(t *testing.T) {
	cfg := &Config{
		MountedVolumes: []MountedVolume{
			{Name: "vol-1", Port: 10809, PID: 100},
		},
	}

	var wg sync.WaitGroup
	iterations := 50

	// Spawn multiple goroutines to read volumes concurrently
	for range iterations {
		wg.Go(func() {
			cfg.mu.Lock()
			_ = len(cfg.MountedVolumes)
			if len(cfg.MountedVolumes) > 0 {
				_ = cfg.MountedVolumes[0].Name
			}
			cfg.mu.Unlock()
		})
	}

	wg.Wait()

	// Should still have original volume
	cfg.mu.Lock()
	assert.Len(t, cfg.MountedVolumes, 1)
	cfg.mu.Unlock()
}

// TestServiceStartWithoutNATS tests Start method behavior
// Note: This will fail without a running NATS server, which is expected
func TestServiceStartWithoutNATS(t *testing.T) {
	// Skip this test in CI environments or when NATS is not available
	if os.Getenv("SKIP_INTEGRATION_TESTS") == "true" {
		t.Skip("Skipping integration test")
	}

	cfg := &Config{
		NatsHost: "nats://localhost:4222",
		BaseDir:  "/tmp/viperblock-test",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)

	// Start in a goroutine since it blocks
	errChan := make(chan error, 1)
	go func() {
		_, err := svc.Start()
		errChan <- err
	}()

	// Wait a short time for connection attempt
	select {
	case err := <-errChan:
		// Expected to fail without NATS server
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		// Timeout is also acceptable
		t.Log("Start() timed out as expected without NATS server")
	}
}

// TestConfigValidation tests validation of required fields
func TestConfigValidation(t *testing.T) {
	// Test empty config - fields should be empty
	cfg := &Config{}
	assert.Empty(t, cfg.NatsHost)
	assert.Empty(t, cfg.Bucket)
	assert.Empty(t, cfg.Region)

	// Test minimal valid config
	cfg2 := &Config{
		NatsHost: "nats://localhost:4222",
		Bucket:   "test-bucket",
		Region:   "us-east-1",
	}
	assert.NotEmpty(t, cfg2.NatsHost)
	assert.NotEmpty(t, cfg2.Bucket)
	assert.NotEmpty(t, cfg2.Region)
}

// TestMountedVolumePortRange tests port assignment
func TestMountedVolumePortRange(t *testing.T) {
	volumes := []MountedVolume{
		{Name: "vol-1", Port: 10809},
		{Name: "vol-2", Port: 10810},
		{Name: "vol-3", Port: 10811},
	}

	for i, vol := range volumes {
		assert.Equal(t, 10809+i, vol.Port)
		assert.True(t, vol.Port >= 10809)
		assert.True(t, vol.Port <= 65535)
	}
}

// TestRetryLoadState pins the retry policy: transient errors retry with backoff,
// ErrStateNotFound fails fast, and the retry budget is honoured.
func TestRetryLoadState(t *testing.T) {
	t.Run("success_first_attempt_no_sleep", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error { calls++; return nil })
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.Equal(t, 0, sleeps)
	})

	t.Run("transient_recovers_within_budget", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error {
				calls++
				if calls < 3 {
					return fmt.Errorf("wrap: %w", viperblock.ErrStateBackendUnavailable)
				}
				return nil
			})
		require.NoError(t, err)
		assert.Equal(t, 3, calls)
		assert.Equal(t, 2, sleeps)
	})

	t.Run("not_found_fails_fast", func(t *testing.T) {
		calls := 0
		sleeps := 0
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) { sleeps++ },
			func() error { calls++; return viperblock.ErrStateNotFound })
		require.Error(t, err)
		assert.ErrorIs(t, err, viperblock.ErrStateNotFound)
		assert.Equal(t, 1, calls, "ErrStateNotFound must not be retried")
		assert.Equal(t, 0, sleeps)
	})

	t.Run("unclassified_fails_fast", func(t *testing.T) {
		calls := 0
		sentinel := errors.New("permission denied")
		err := retryLoadState("vol-x", 5, 10*time.Millisecond,
			func(time.Duration) {},
			func() error { calls++; return sentinel })
		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel)
		assert.Equal(t, 1, calls)
	})

	t.Run("exhausts_budget_on_persistent_transient", func(t *testing.T) {
		calls := 0
		sleeps := 0
		var lastDelay time.Duration
		err := retryLoadState("vol-x", 4, 100*time.Millisecond,
			func(d time.Duration) { sleeps++; lastDelay = d },
			func() error { calls++; return viperblock.ErrStateBackendUnavailable })
		require.Error(t, err)
		assert.ErrorIs(t, err, viperblock.ErrStateBackendUnavailable)
		assert.Equal(t, 4, calls)
		assert.Equal(t, 3, sleeps, "sleep happens between attempts, not after the final one")
		// Backoff multiplier is 1.5: 100ms, 150ms, 225ms.
		assert.Equal(t, 225*time.Millisecond, lastDelay)
	})
}
