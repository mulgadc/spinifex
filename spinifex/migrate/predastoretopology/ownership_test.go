package predastoretopology

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nobodyUID is an unprivileged id the root-only assertions move data to, so
// "inherited the owner" cannot be confused with "happened to already match".
const nobodyUID, nobodyGID = 65534, 65534

// TestPredastoreDataDirInheritsNodeOwnership pins the ownership of the data
// dir the migration creates. Predastore runs as its own service user while the
// migration runs as root, and cannot traverse a root-owned parent.
func TestPredastoreDataDirInheritsNodeOwnership(t *testing.T) {
	nodeDirs := []string{
		"distributed/db/node-1", "distributed/db/node-2", "distributed/db/node-3",
		"distributed/nodes/node-1", "distributed/nodes/node-2", "distributed/nodes/node-3",
	}
	configDir, dataDir := predastoreFixture(t, "predastore_v4_singlenode.toml", nodeDirs)

	wantUID, wantGID := os.Geteuid(), os.Getegid()
	if os.Geteuid() == 0 {
		// Stand the fixture up as an unprivileged user owns it, which is the
		// shape of a real install the root migration has to preserve.
		for _, dir := range nodeDirs {
			require.NoError(t, chownTree(filepath.Join(dataDir, "predastore", dir), nobodyUID, nobodyGID))
		}
		wantUID, wantGID = nobodyUID, nobodyGID
	}

	runPredastoreMigration(t, configDir, dataDir)

	cluster := filepath.Join(dataDir, "predastore", "cluster")
	uid, gid, err := dirOwner(cluster)
	require.NoError(t, err)
	assert.Equal(t, wantUID, uid, "data dir owner")
	assert.Equal(t, wantGID, gid, "data dir group")

	// The node dirs arrive by rename, so they keep their owner rather than
	// gaining one; assert it anyway, since recovery writes into them.
	for id := 1; id <= 6; id++ {
		uid, gid, err := dirOwner(filepath.Join(cluster, fmt.Sprintf("node-%d", id)))
		require.NoError(t, err)
		assert.Equal(t, wantUID, uid, "node-%d owner", id)
		assert.Equal(t, wantGID, gid, "node-%d group", id)
	}
}

// TestChownIsNoopWhenUnprivileged covers the guard that lets the migration run
// as an ordinary user, where there is no ownership to correct and every chown
// would fail.
func TestChownIsNoopWhenUnprivileged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("asserts the unprivileged path")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0640))

	require.NoError(t, chown(dir, nobodyUID, nobodyGID))
	require.NoError(t, chownTree(dir, nobodyUID, nobodyGID))

	uid, _, err := dirOwner(dir)
	require.NoError(t, err)
	assert.Equal(t, os.Geteuid(), uid, "ownership must be left alone")
}

// TestChownTreeCoversNestedContents guards the recursive case: raft recovery
// writes a snapshot directory and badger segments below the node dir.
func TestChownTreeCoversNestedContents(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another user requires root")
	}

	dir := t.TempDir()
	nested := filepath.Join(dir, "snapshots", "1-2-3")
	require.NoError(t, os.MkdirAll(nested, 0750))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "state.bin"), []byte("x"), 0640))

	require.NoError(t, chownTree(dir, nobodyUID, nobodyGID))

	for _, path := range []string{dir, nested, filepath.Join(nested, "state.bin")} {
		uid, gid, err := dirOwner(path)
		require.NoError(t, err)
		assert.Equal(t, nobodyUID, uid, path)
		assert.Equal(t, nobodyGID, gid, path)
	}
}
