package viperblocklegacyv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigUpdateSubjects(t *testing.T) {
	require.Equal(t, "ebs.config", ConfigUpdateSubject)
	require.Equal(t, "ebs.config.vol-123", VolumeConfigUpdateSubject("vol-123"))
	// The deployed route builder did not validate its final token. Validation
	// remains with the handler and is not silently introduced by this move.
	require.Equal(t, "ebs.config.", VolumeConfigUpdateSubject(""))
}

func TestConfigUpdateRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(EBSConfigUpdateRequest{
		Volume:       "vol-123",
		VolumeConfig: json.RawMessage(`{"size_gib":8}`),
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123","VolumeConfig":{"size_gib":8}}`, string(data))
}

func TestConfigUpdateResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(EBSConfigUpdateResponse{
		Volume:  "vol-123",
		Success: true,
		Error:   "",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123","Success":true,"Error":""}`, string(data))
}
