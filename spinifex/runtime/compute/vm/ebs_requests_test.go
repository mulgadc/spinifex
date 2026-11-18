package vm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nbdRequestJSON = `{
		"Name":"vol-123", "VolType":"gp3", "Boot":true, "EFI":true,
		"DeleteOnTermination":true,
		"NBDURI":"nbd:unix:/run/spinifex/vol-123.sock",
		"DeviceName":"/dev/sdf", "HotplugPort":3
	}`
	blockRequestJSON = `{
		"Name":"vol-456", "VolType":"", "Boot":false, "EFI":false,
		"DeleteOnTermination":false, "NBDURI":"", "DeviceName":"",
		"BlockDevice":"/dev/sdx", "BlockSizeBytes":10737418240
	}`
)

// TestEBSRequestJSON pins the persisted attachment shape against literals, so
// instance records and local state written before a change still decode.
func TestEBSRequestJSON(t *testing.T) {
	cases := []struct {
		name    string
		golden  string
		request EBSRequest
	}{
		{"nbd export", nbdRequestJSON, EBSRequest{
			Name:                "vol-123",
			VolType:             "gp3",
			Boot:                true,
			EFI:                 true,
			DeleteOnTermination: true,
			NBDURI:              "nbd:unix:/run/spinifex/vol-123.sock",
			DeviceName:          "/dev/sdf",
			HotplugPort:         3,
		}},
		{"block export", blockRequestJSON, EBSRequest{
			Name:           "vol-456",
			BlockDevice:    "/dev/sdx",
			BlockSizeBytes: 10 << 30,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.request)
			require.NoError(t, err)
			assert.JSONEq(t, tc.golden, string(data))

			var decoded EBSRequest
			require.NoError(t, json.Unmarshal([]byte(tc.golden), &decoded))
			assert.Equal(t, tc.request, decoded)
		})
	}
}

// TestEBSRequestsJSON pins the collection's key and that its mutex is never
// written, so a stored record round-trips through the list.
func TestEBSRequestsJSON(t *testing.T) {
	golden := `{"Requests":[` + nbdRequestJSON + `]}`

	var decoded EBSRequests
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	require.Len(t, decoded.Requests, 1)
	assert.Equal(t, "vol-123", decoded.Requests[0].Name)

	data, err := json.Marshal(&decoded)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))
}
