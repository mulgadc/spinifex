package viperblocklegacyv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSyncSubject(t *testing.T) {
	require.Equal(t, "ebs.sync", SyncSubject)
}

func TestSyncRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(EBSSyncRequest{Volume: "vol-123"})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123"}`, string(data))
}

func TestSyncResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(EBSSyncResponse{
		Volume: "vol-123",
		Synced: true,
		Error:  "",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123","Synced":true,"Error":""}`, string(data))
}
