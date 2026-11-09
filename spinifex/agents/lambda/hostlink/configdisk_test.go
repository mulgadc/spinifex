//test:in-package reaches the unexported configDisk and fwCfgTree readers to seal and read a real archive

package hostlink

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// configDisk writes a fake disk holding secret and an extracted tree; the
// stand-in unmount reports the tree is not a mount, as on a plain directory.
func configDisk(t *testing.T, secret []byte) (device, root string) {
	t.Helper()
	dir := t.TempDir()
	device = filepath.Join(dir, "vdb")
	require.NoError(t, os.WriteFile(device, append(bytes.Repeat([]byte{0xa5}, 3*zeroChunk+7), secret...), 0o600))
	root = fwCfgTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "by_name", "key"), secret, 0o600))
	old := unmount
	unmount = func(string, int) error { return unix.EINVAL }
	t.Cleanup(func() { unmount = old })
	return device, root
}

func TestSealConfigDisk(t *testing.T) {
	secret := []byte("guest-key-material")

	t.Run("erases the disk and removes the tree", func(t *testing.T) {
		device, root := configDisk(t, secret)
		before, err := os.Stat(device)
		require.NoError(t, err)
		require.NoError(t, SealFwCfg(root, func() error { return SealConfigDisk(device, root) }))

		disk, err := os.ReadFile(device)
		require.NoError(t, err)
		assert.Len(t, disk, int(before.Size()))
		assert.Equal(t, make([]byte, len(disk)), disk, "configuration disk not fully erased")
		_, err = os.Stat(root)
		assert.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("an unwritable disk keeps the tree and fails closed", func(t *testing.T) {
		device, root := configDisk(t, secret)
		require.NoError(t, os.Chmod(device, 0o400))
		err := SealFwCfg(root, func() error { return SealConfigDisk(device, root) })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still readable")
		_, err = os.Stat(root)
		assert.NoError(t, err)
	})
	t.Run("a failed unmount keeps the tree", func(t *testing.T) {
		device, root := configDisk(t, secret)
		unmount = func(string, int) error { return unix.EBUSY }
		require.Error(t, SealFwCfg(root, func() error { return SealConfigDisk(device, root) }))
		_, err := os.Stat(root)
		assert.NoError(t, err)
	})
}
