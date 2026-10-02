package nbd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSocketFile(t *testing.T) {
	socketPath := fmt.Sprintf("%s/%s", t.TempDir(), "utilsunittest")

	name, err := GenerateSocketFile(socketPath)

	assert.NoError(t, err)

	assert.True(t, strings.HasSuffix(name, "utilsunittest.sock"))

	// Test empty socket path
	_, err = GenerateSocketFile("")

	assert.Error(t, err)
}

func TestWaitForNBDReady(t *testing.T) {
	t.Run("unix socket ready", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nbd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		require.NoError(t, WaitForNBDReady(FormatNBDSocketURI(path), time.Second))
	})

	t.Run("unix socket times out", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.sock")
		err := WaitForNBDReady(FormatNBDSocketURI(path), 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("tcp listener ready", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		_, portStr, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)
		port, err := strconv.Atoi(portStr)
		require.NoError(t, err)

		require.NoError(t, WaitForNBDReady(FormatNBDTCPURI("127.0.0.1", port), time.Second))
	})

	t.Run("tcp listener times out", func(t *testing.T) {
		// Port 1 is unprivileged-bind reserved; nothing will be listening.
		err := WaitForNBDReady(FormatNBDTCPURI("127.0.0.1", 1), 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("rejects malformed uri", func(t *testing.T) {
		err := WaitForNBDReady("garbage://nope", time.Second)
		require.Error(t, err)
	})
}

func TestParseNBDURI(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		wantType string
		wantPath string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "Unix socket URI",
			uri:      "nbd+unix:///?socket=/run/user/1000/nbd-vol-123.sock",
			wantType: "unix",
			wantPath: "/run/user/1000/nbd-vol-123.sock",
		},
		{
			name:     "Unix socket URI without the empty export path",
			uri:      "nbd+unix://?socket=/run/nbd-vol.sock",
			wantType: "unix",
			wantPath: "/run/nbd-vol.sock",
		},
		{
			name:     "legacy QEMU nbd:unix: filename form",
			uri:      "nbd:unix:/run/user/1000/nbd-vol-123.sock",
			wantType: "unix",
			wantPath: "/run/user/1000/nbd-vol-123.sock",
		},
		{
			name:     "TCP address",
			uri:      "nbd://127.0.0.1:34305",
			wantType: "inet",
			wantHost: "127.0.0.1",
			wantPort: 34305,
		},
		{
			name:     "TCP with hostname",
			uri:      "nbd://storage.local:9000",
			wantType: "inet",
			wantHost: "storage.local",
			wantPort: 9000,
		},
		{
			name:    "Empty socket path",
			uri:     "nbd:unix:",
			wantErr: true,
		},
		{
			name:    "Unix socket URI with no socket parameter",
			uri:     "nbd+unix:///",
			wantErr: true,
		},
		{
			name:    "Missing port in TCP",
			uri:     "nbd://127.0.0.1",
			wantErr: true,
		},
		{
			name:    "Invalid port",
			uri:     "nbd://127.0.0.1:notaport",
			wantErr: true,
		},
		{
			name:    "Unsupported format",
			uri:     "http://example.com",
			wantErr: true,
		},
		{
			name:    "Empty string",
			uri:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverType, path, host, port, err := ParseNBDURI(tt.uri)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.wantType, serverType)
			assert.Equal(t, tt.wantPath, path)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantPort, port)
		})
	}
}

func TestIsSocketURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{"Socket suffix", "/run/nbd-vol.sock", true},
		{"Unix prefix", "unix:/run/nbd-vol", true},
		{"Both", "unix:/run/nbd-vol.sock", true},
		{"TCP URI", "nbd://127.0.0.1:9000", false},
		{"Empty", "", false},
		{"Random path", "/tmp/somefile.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSocketURI(tt.uri))
		})
	}
}

func TestFormatNBDSocketURI(t *testing.T) {
	assert.Equal(t, "nbd+unix:///?socket=/run/nbd-vol.sock", FormatNBDSocketURI("/run/nbd-vol.sock"))
	assert.Equal(t, "nbd+unix:///?socket=/tmp/test.sock", FormatNBDSocketURI("/tmp/test.sock"))
}

// TestFormatNBDSocketURIRoundTrips guards the pairing that a third-party NBD
// client depends on: what we publish must be what we can parse back.
func TestFormatNBDSocketURIRoundTrips(t *testing.T) {
	const path = "/run/spinifex/nbd/nbd-vol-abc123.sock"
	serverType, parsed, _, _, err := ParseNBDURI(FormatNBDSocketURI(path))
	require.NoError(t, err)
	assert.Equal(t, "unix", serverType)
	assert.Equal(t, path, parsed)
}

func TestFormatNBDTCPURI(t *testing.T) {
	assert.Equal(t, "nbd://127.0.0.1:9000", FormatNBDTCPURI("127.0.0.1", 9000))
	assert.Equal(t, "nbd://storage.local:34305", FormatNBDTCPURI("storage.local", 34305))
}

func TestGenerateUniqueSocketFile(t *testing.T) {
	path1, err := GenerateUniqueSocketFile("vol-123")
	require.NoError(t, err)
	assert.Contains(t, path1, "nbd-vol-123-")
	assert.True(t, strings.HasSuffix(path1, ".sock"))

	// Two calls should produce different paths (different timestamps)
	time.Sleep(time.Nanosecond)
	path2, err := GenerateUniqueSocketFile("vol-123")
	require.NoError(t, err)
	assert.NotEqual(t, path1, path2)

	// Empty volume name
	_, err = GenerateUniqueSocketFile("")
	assert.Error(t, err)
}

func TestDirExists(t *testing.T) {
	// Existing directory
	assert.True(t, dirExists(os.TempDir()))

	// Non-existent path
	assert.False(t, dirExists("/nonexistent/path/should/not/exist"))

	// File (not a directory)
	tmpFile, err := os.CreateTemp(t.TempDir(), "direxists-test-*")
	require.NoError(t, err)
	tmpFile.Close()
	assert.False(t, dirExists(tmpFile.Name()))
}

func TestGenerateSocketFile_EmptyName(t *testing.T) {
	_, err := GenerateSocketFile("")
	assert.Error(t, err)
}
