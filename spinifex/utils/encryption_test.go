package utils

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mulgadc/predastore/pkg/masterkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMasterKeyFile writes a 32-byte master key at name under t.TempDir()
// with the given mode (chmod'd explicitly to bypass umask).
func writeMasterKeyFile(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, raw, mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// resetKeyCache clears the process-wide cache so cache-sensitive tests
// can run independently regardless of order.
func resetKeyCache() {
	viperblockKeyCacheMu.Lock()
	defer viperblockKeyCacheMu.Unlock()
	viperblockKeyCache = map[string]*masterkey.Key{}
}

func TestLoadViperblockMasterKey_EmptyPath(t *testing.T) {
	resetKeyCache()
	k, err := LoadViperblockMasterKey("")
	require.NoError(t, err)
	assert.Nil(t, k, "empty path must return (nil, nil) — encryption disabled")
}

func TestLoadViperblockMasterKey_LoadsAndCaches(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not enforced on Windows")
	}
	resetKeyCache()

	path := writeMasterKeyFile(t, "key", 0o640)

	k1, err := LoadViperblockMasterKey(path)
	require.NoError(t, err)
	require.NotNil(t, k1)
	assert.NotEmpty(t, k1.Fingerprint)

	// Second call must return the cached *Key (pointer-equal), proving
	// the path is memoised rather than re-stat'd + re-parsed.
	k2, err := LoadViperblockMasterKey(path)
	require.NoError(t, err)
	assert.Same(t, k1, k2, "second LoadViperblockMasterKey must return cached pointer")
}

func TestLoadViperblockMasterKey_LoadError(t *testing.T) {
	resetKeyCache()

	missing := filepath.Join(t.TempDir(), "nonexistent.key")
	k, err := LoadViperblockMasterKey(missing)
	require.Error(t, err)
	assert.Nil(t, k)
	assert.Contains(t, err.Error(), "load viperblock encryption key")
	assert.Contains(t, err.Error(), missing)
}

func TestLoadViperblockMasterKey_FailedLoadNotCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not enforced on Windows")
	}
	resetKeyCache()

	// 0o644 has the world-read bit set, which masterkey.LoadShared rejects.
	path := writeMasterKeyFile(t, "loose.key", 0o644)
	_, err := LoadViperblockMasterKey(path)
	require.Error(t, err)

	// Fix the permissions; a subsequent call must succeed (proving the
	// previous failure was not memoised).
	require.NoError(t, os.Chmod(path, 0o640))
	k, err := LoadViperblockMasterKey(path)
	require.NoError(t, err)
	require.NotNil(t, k)
}
