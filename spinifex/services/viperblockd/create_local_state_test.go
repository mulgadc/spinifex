package viperblockd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderHandlers_CreateVolume_LeavesNoLocalState pins that a create
// leaves nothing behind on the node that served it. A state read prefers a
// local copy over the backend, so a leftover here answers describe as if the
// volume still existed after another node deleted it.
func TestProviderHandlers_CreateVolume_LeavesNoLocalState(t *testing.T) {
	fixture := testpredastore.Start(t)
	_, natsURL := setupEmbeddedNATS(t)

	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = "https://" + fixture.Host
	cfg.Bucket = testpredastore.DefaultBucket
	cfg.Region = fixture.Region
	cfg.AccessKey = fixture.AccessKey
	cfg.SecretKey = fixture.SecretKey
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-createlocalstate01"
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(1) << 30},
	})
	msg := requestProvider(t, nc, ebsprovider.CreateVolumeSubject, body)

	var resp ebsprovider.CreateVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Nil(t, resp.Error)

	localPath, err := localVolumeDir(cfg.BaseDir, volumeName)
	require.NoError(t, err)
	_, err = os.Stat(localPath)
	assert.True(t, os.IsNotExist(err), "create must leave no local state at %s, got err=%v", localPath, err)
}
