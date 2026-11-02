package hostlink

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fwCfgTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "qemu_fw_cfg")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "by_name"), 0o700))
	return root
}

func TestSealFwCfg(t *testing.T) {
	t.Run("already absent needs no unload", func(t *testing.T) {
		called := false
		require.NoError(t, SealFwCfg(filepath.Join(t.TempDir(), "absent"), func() error { called = true; return nil }))
		assert.False(t, called)
	})
	t.Run("unload removes the tree", func(t *testing.T) {
		root := fwCfgTree(t)
		require.NoError(t, SealFwCfg(root, func() error { return os.RemoveAll(root) }))
	})
	t.Run("unload fails and the tree remains", func(t *testing.T) {
		builtIn := errors.New("module is built in")
		err := SealFwCfg(fwCfgTree(t), func() error { return builtIn })
		assert.ErrorIs(t, err, builtIn)
		assert.ErrorContains(t, err, "still readable")
	})
	t.Run("unload succeeds but the tree remains", func(t *testing.T) {
		assert.ErrorContains(t, SealFwCfg(fwCfgTree(t), func() error { return nil }), "unload reported success")
	})
	t.Run("unload errors after removing the tree", func(t *testing.T) {
		root := fwCfgTree(t)
		require.NoError(t, SealFwCfg(root, func() error { _ = os.RemoveAll(root); return errors.New("late") }))
	})
}
