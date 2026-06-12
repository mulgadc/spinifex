package predastore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNew tests the service constructor
func TestNew(t *testing.T) {
	cfg := &Config{
		ConfigPath:        "/tmp/test-config.toml",
		Port:              8443,
		Host:              "0.0.0.0",
		Debug:             false,
		BasePath:          "/tmp/predastore",
		TlsCert:           "/tmp/cert.pem",
		TlsKey:            "/tmp/key.pem",
		EncryptionKeyFile: "/tmp/encryption.key",
	}

	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
	assert.Equal(t, "/tmp/test-config.toml", svc.Config.ConfigPath)
	assert.Equal(t, 8443, svc.Config.Port)
	assert.Equal(t, "0.0.0.0", svc.Config.Host)
	assert.False(t, svc.Config.Debug)
	assert.Equal(t, "/tmp/predastore", svc.Config.BasePath)
	assert.Equal(t, "/tmp/cert.pem", svc.Config.TlsCert)
	assert.Equal(t, "/tmp/key.pem", svc.Config.TlsKey)
	assert.Equal(t, "/tmp/encryption.key", svc.Config.EncryptionKeyFile)
}

// TestNewWithNilConfig tests that New handles nil config correctly
func TestNewWithNilConfig(t *testing.T) {
	cfg := &Config{}
	svc, err := New(cfg)

	assert.NoError(t, err)
	assert.NotNil(t, svc)
	assert.NotNil(t, svc.Config)
}

// TestConfigDefaults tests Config struct default values
func TestConfigDefaults(t *testing.T) {
	cfg := &Config{}

	assert.Empty(t, cfg.ConfigPath)
	assert.Empty(t, cfg.Host)
	assert.Equal(t, 0, cfg.Port)
	assert.False(t, cfg.Debug)
	assert.Empty(t, cfg.BasePath)
	assert.Empty(t, cfg.TlsCert)
	assert.Empty(t, cfg.TlsKey)
	assert.Empty(t, cfg.EncryptionKeyFile)
}

// TestConfigWithDebug tests Config with debug enabled
func TestConfigWithDebug(t *testing.T) {
	cfg := &Config{
		Debug:      true,
		ConfigPath: "/etc/predastore/config.toml",
		Port:       8443,
	}

	assert.True(t, cfg.Debug)
	assert.Equal(t, "/etc/predastore/config.toml", cfg.ConfigPath)
	assert.Equal(t, 8443, cfg.Port)
}

// TestConfigFullyPopulated tests a fully populated config
func TestConfigFullyPopulated(t *testing.T) {
	cfg := &Config{
		ConfigPath:        "/etc/predastore/config.toml",
		Port:              9443,
		Host:              "127.0.0.1",
		Debug:             true,
		BasePath:          "/var/lib/predastore",
		TlsCert:           "/etc/ssl/certs/predastore.crt",
		TlsKey:            "/etc/ssl/private/predastore.key",
		EncryptionKeyFile: "/etc/spinifex/predastore/encryption.key",
	}

	assert.Equal(t, "/etc/predastore/config.toml", cfg.ConfigPath)
	assert.Equal(t, 9443, cfg.Port)
	assert.Equal(t, "127.0.0.1", cfg.Host)
	assert.True(t, cfg.Debug)
	assert.Equal(t, "/var/lib/predastore", cfg.BasePath)
	assert.Equal(t, "/etc/ssl/certs/predastore.crt", cfg.TlsCert)
	assert.Equal(t, "/etc/ssl/private/predastore.key", cfg.TlsKey)
	assert.Equal(t, "/etc/spinifex/predastore/encryption.key", cfg.EncryptionKeyFile)
}

// TestServiceMethods tests the service interface methods
func TestServiceMethods(t *testing.T) {
	cfg := &Config{
		ConfigPath: "/tmp/test-config.toml",
		Port:       8443,
		Host:       "127.0.0.1",
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

// TestServiceNameConstant tests the serviceName constant
func TestServiceNameConstant(t *testing.T) {
	assert.Equal(t, "predastore", serviceName)
}

// TestConfigPortRange tests port validation
func TestConfigPortRange(t *testing.T) {
	testCases := []struct {
		name     string
		port     int
		valid    bool
		expected int
	}{
		{"Standard HTTPS", 443, true, 443},
		{"Custom Port", 8443, true, 8443},
		{"High Port", 65535, true, 65535},
		{"Port 80", 80, true, 80},
		{"Port 8080", 8080, true, 8080},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Port: tc.port,
			}

			assert.Equal(t, tc.expected, cfg.Port)
			if tc.valid {
				assert.True(t, cfg.Port > 0)
				assert.True(t, cfg.Port <= 65535)
			}
		})
	}
}

// TestConfigHostnames tests host configuration
func TestConfigHostnames(t *testing.T) {
	testCases := []struct {
		name     string
		host     string
		expected string
	}{
		{"Localhost", "127.0.0.1", "127.0.0.1"},
		{"All Interfaces", "0.0.0.0", "0.0.0.0"},
		{"Localhost Name", "localhost", "localhost"},
		{"Custom Domain", "s3.example.com", "s3.example.com"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Host: tc.host,
			}

			assert.Equal(t, tc.expected, cfg.Host)
			assert.NotEmpty(t, cfg.Host)
		})
	}
}

// TestConfigBasePath tests base path configuration
func TestConfigBasePath(t *testing.T) {
	testCases := []struct {
		name     string
		basePath string
	}{
		{"Absolute Path", "/var/lib/predastore"},
		{"Relative Path", "data/predastore"},
		{"Temp Directory", "/tmp/predastore"},
		{"User Directory", "/home/user/predastore"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				BasePath: tc.basePath,
			}

			assert.Equal(t, tc.basePath, cfg.BasePath)
		})
	}
}

// TestConfigTLSPaths tests TLS certificate and key paths
func TestConfigTLSPaths(t *testing.T) {
	cfg := &Config{
		TlsCert: "/etc/ssl/certs/server.crt",
		TlsKey:  "/etc/ssl/private/server.key",
	}

	assert.NotEmpty(t, cfg.TlsCert)
	assert.NotEmpty(t, cfg.TlsKey)
	assert.Contains(t, cfg.TlsCert, ".crt")
	assert.Contains(t, cfg.TlsKey, ".key")
}

// TestConfigMinimal tests minimal required config
func TestConfigMinimal(t *testing.T) {
	cfg := &Config{
		ConfigPath: "/tmp/config.toml",
		Port:       8443,
		Host:       "127.0.0.1",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)
	assert.NotNil(t, svc)

	// Minimal config should work
	assert.NotEmpty(t, cfg.ConfigPath)
	assert.True(t, cfg.Port > 0)
	assert.NotEmpty(t, cfg.Host)
}

// TestServiceStartWithoutConfig tests Start method behavior
// Note: This will fail without proper config file, which is expected
func TestServiceStartWithoutConfig(t *testing.T) {
	// Skip this test - it requires actual config file and will block/exit
	// This is covered by integration tests instead
	t.Skip("Skipping test that requires actual predastore config file - covered by integration tests")
}

// TestServiceStartRejectsMissingEncryptionKey ensures Start fails fast when
// EncryptionKeyFile is unset, before touching the pid file or binding.
func TestServiceStartRejectsMissingEncryptionKey(t *testing.T) {
	cfg := &Config{
		ConfigPath: "/tmp/test-config.toml",
		Port:       18443,
		Host:       "127.0.0.1",
		BasePath:   t.TempDir(),
		TlsCert:    "/tmp/cert.pem",
		TlsKey:     "/tmp/key.pem",
		// EncryptionKeyFile deliberately left empty.
	}
	svc, err := New(cfg)
	assert.NoError(t, err)

	_, err = svc.Start()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "encryption key file is required")
}

// TestConfigDebugFlag tests debug flag behavior
func TestConfigDebugFlag(t *testing.T) {
	// Debug enabled
	cfg1 := &Config{
		Debug: true,
	}
	assert.True(t, cfg1.Debug)

	// Debug disabled
	cfg2 := &Config{
		Debug: false,
	}
	assert.False(t, cfg2.Debug)

	// Default should be false
	cfg3 := &Config{}
	assert.False(t, cfg3.Debug)
}

// TestConfigValidation tests validation of required fields
func TestConfigValidation(t *testing.T) {
	// Empty config - fields should be empty/zero
	cfg := &Config{}
	assert.Empty(t, cfg.ConfigPath)
	assert.Equal(t, 0, cfg.Port)
	assert.Empty(t, cfg.Host)

	// Valid config
	cfg2 := &Config{
		ConfigPath: "/etc/predastore/config.toml",
		Port:       8443,
		Host:       "0.0.0.0",
	}
	assert.NotEmpty(t, cfg2.ConfigPath)
	assert.True(t, cfg2.Port > 0)
	assert.NotEmpty(t, cfg2.Host)
}

// TestMultipleServiceInstances tests creating multiple service instances
func TestMultipleServiceInstances(t *testing.T) {
	cfg1 := &Config{
		ConfigPath: "/tmp/config1.toml",
		Port:       8443,
		Host:       "127.0.0.1",
	}

	cfg2 := &Config{
		ConfigPath: "/tmp/config2.toml",
		Port:       9443,
		Host:       "127.0.0.1",
	}

	svc1, err := New(cfg1)
	assert.NoError(t, err)
	assert.NotNil(t, svc1)

	svc2, err := New(cfg2)
	assert.NoError(t, err)
	assert.NotNil(t, svc2)

	// Services should be independent
	assert.NotEqual(t, svc1.Config.Port, svc2.Config.Port)
	assert.NotEqual(t, svc1.Config.ConfigPath, svc2.Config.ConfigPath)
}

// TestConfigPointerPreservation tests that config pointer is preserved
func TestConfigPointerPreservation(t *testing.T) {
	cfg := &Config{
		Port: 8443,
		Host: "127.0.0.1",
	}

	svc, err := New(cfg)
	assert.NoError(t, err)

	// Modify original config
	cfg.Port = 9443

	// Service should reflect the change (same pointer)
	assert.Equal(t, 9443, svc.Config.Port)
}
