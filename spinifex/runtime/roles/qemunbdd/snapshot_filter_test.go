package qemunbdd_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/qemunbdd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A filtered read is answered from the snapshot's file, so an ID that is not a
// single path element must name nothing rather than a file elsewhere on disk.
func TestListSnapshotsFilterOnlyNamesSnapshotFiles(t *testing.T) {
	baseDir := shortTempDir(t)
	provider, err := qemunbdd.NewProvider(baseDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "volumes", "vol-1.qcow2"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "snapshots", "snap-1.qcow2"), nil, 0o600))

	for _, tt := range []struct {
		snapshotID string
		want       []ebsprovider.SnapshotRef
	}{
		{snapshotID: "snap-1", want: []ebsprovider.SnapshotRef{{ID: "snap-1", Handle: filepath.Join(baseDir, "snapshots", "snap-1.qcow2"), State: ebsprovider.SnapshotStateCompleted}}},
		{snapshotID: "snap-absent"},
		{snapshotID: "../volumes/vol-1"},
	} {
		t.Run(tt.snapshotID, func(t *testing.T) {
			resp, err := provider.ListSnapshots(t.Context(), ebsprovider.ListSnapshotsRequest{Versioned: ebsprovider.NewLatestVersioned(), SnapshotID: tt.snapshotID})
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Snapshots)
		})
	}
}
