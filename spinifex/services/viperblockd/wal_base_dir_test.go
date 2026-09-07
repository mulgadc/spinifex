package viperblockd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestVolumeVBConfigCarriesWALBaseDir pins that the short-lived VBs the
// service opens honour the node's WAL device choice, so a volume's WAL is in
// one place regardless of which code path opened it.
func TestVolumeVBConfigCarriesWALBaseDir(t *testing.T) {
	cfg := &Config{BaseDir: "/tmp/vb-base", WALBaseDir: "/wal"}

	vbconfig, _ := volumeVBConfig(cfg, "vol-1")
	assert.Equal(t, "/tmp/vb-base", vbconfig.BaseDir)
	assert.Equal(t, "/wal", vbconfig.WALBaseDir)
}

// TestVolumeVBConfigLeavesWALBaseDirUnsetByDefault pins the no-op default:
// viperblock reads an empty WALBaseDir as "same as BaseDir".
func TestVolumeVBConfigLeavesWALBaseDirUnsetByDefault(t *testing.T) {
	vbconfig, _ := volumeVBConfig(&Config{BaseDir: "/tmp/vb-base"}, "vol-1")
	assert.Empty(t, vbconfig.WALBaseDir)
}
