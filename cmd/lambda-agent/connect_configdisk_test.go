//test:in-package — package main cannot be imported by an external test package.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// An identity that does not load still seals the configuration disk: the
// guest key on it is erased before the agent refuses to start.
func TestConfigDiskStartup_RefusedLoadErasesDisk(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cfg")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "by_name"), 0o700))
	device := filepath.Join(dir, "vdb")
	require.NoError(t, os.WriteFile(device, bytes.Repeat([]byte("guest-key"), 512), 0o600))

	_, _, code, err := configDiskStartup(root, device).connect(t.Context())
	require.Error(t, err)
	require.Equal(t, exitRefused, code)
	got, rerr := os.ReadFile(device)
	require.NoError(t, rerr)
	require.Len(t, got, 9*512)
	require.Equal(t, make([]byte, len(got)), got, "configuration disk erased")
}

// A disk that cannot be erased fails closed: the agent refuses, and the
// extracted tree is kept so the seal check sees it.
func TestConfigDiskStartup_UnerasableDiskRefuses(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cfg")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "by_name"), 0o700))

	_, _, code, err := configDiskStartup(root, filepath.Join(dir, "missing")).connect(t.Context())
	require.ErrorContains(t, err, "erase configuration disk")
	require.Equal(t, exitRefused, code)
	require.DirExists(t, root)
}
