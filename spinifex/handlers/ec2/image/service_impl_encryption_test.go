// Copyright 2026 Mulga Defense Corporation (MDC). All rights reserved.
// Use of this source code is governed by an Apache 2.0 license
// that can be found in the LICENSE file.

package handlers_ec2_image

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMasterKey writes a 32-byte AES-256 master key at mode and returns the path.
func writeMasterKey(t *testing.T, mode os.FileMode) string {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "master.key")
	require.NoError(t, os.WriteFile(path, raw, mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// imageServiceWithConfig returns an ImageServiceImpl wired to the given
// ViperblockConfig — used to exercise the cluster-encryption posture path
// which reads s.config.Viperblock.EncryptionKeyFile.
func imageServiceWithConfig(vb config.ViperblockConfig) *ImageServiceImpl {
	return &ImageServiceImpl{
		config: &config.Config{Viperblock: vb},
		store:  objectstore.NewMemoryObjectStore(),
	}
}

func TestClusterEncryptionEnabled_NilConfig(t *testing.T) {
	svc := &ImageServiceImpl{store: objectstore.NewMemoryObjectStore()}
	assert.False(t, svc.clusterEncryptionEnabled(),
		"nil config must report encryption disabled")
}

func TestClusterEncryptionEnabled_EmptyKeyFile(t *testing.T) {
	svc := imageServiceWithConfig(config.ViperblockConfig{})
	assert.False(t, svc.clusterEncryptionEnabled(),
		"empty EncryptionKeyFile must report encryption disabled")
}

func TestClusterEncryptionEnabled_LoadError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent.key")
	svc := imageServiceWithConfig(config.ViperblockConfig{EncryptionKeyFile: missing})
	assert.False(t, svc.clusterEncryptionEnabled(),
		"failed master key load must report encryption disabled (warn + fall back)")
}

func TestClusterEncryptionEnabled_KeyLoaded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not enforced on Windows")
	}
	path := writeMasterKey(t, 0o640)
	svc := imageServiceWithConfig(config.ViperblockConfig{EncryptionKeyFile: path})
	assert.True(t, svc.clusterEncryptionEnabled(),
		"valid master key must report cluster encryption enabled")
}
