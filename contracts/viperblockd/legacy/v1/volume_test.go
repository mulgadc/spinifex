package viperblocklegacyv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVolumeSubjects(t *testing.T) {
	require.Equal(t, "ebs.delete", DeleteSubject)
	require.Equal(t, "ebs.mount", MountSubject(""))
	require.Equal(t, "ebs.unmount", UnmountSubject(""))
	require.Equal(t, "ebs.node-a.mount", MountSubject("node-a"))
	require.Equal(t, "ebs.node-a.unmount", UnmountSubject("node-a"))
	require.Equal(t, "ebs.mount.response", MountResponseSubject)
	require.Equal(t, "ebs.unmount.response", UnmountResponseSubject)
}

func TestEBSRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(EBSRequest{
		Name:                "vol-123",
		VolType:             "gp3",
		Boot:                true,
		EFI:                 true,
		DeleteOnTermination: true,
		NBDURI:              "nbd:unix:/run/spinifex/vol-123.sock",
		DeviceName:          "/dev/sdf",
		HotplugPort:         3,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"Name":"vol-123", "VolType":"gp3", "Boot":true, "EFI":true,
		"DeleteOnTermination":true,
		"NBDURI":"nbd:unix:/run/spinifex/vol-123.sock",
		"DeviceName":"/dev/sdf", "HotplugPort":3
	}`, string(data))
}

func TestVolumeResponseJSONContracts(t *testing.T) {
	mount, err := json.Marshal(EBSMountResponse{
		URI: "nbd:unix:/run/spinifex/vol-123.sock", Mounted: true, Retryable: true,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"URI":"nbd:unix:/run/spinifex/vol-123.sock", "Mounted":true,
		"Error":"", "Retryable":true
	}`, string(mount))

	unmount, err := json.Marshal(EBSUnMountResponse{
		Volume: "vol-123", NotFound: true, Reaped: true,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"Volume":"vol-123", "Mounted":false, "Error":"",
		"NotFound":true, "Reaped":true
	}`, string(unmount))

	deleteRequest, err := json.Marshal(EBSDeleteRequest{Volume: "vol-123"})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123"}`, string(deleteRequest))

	deleteResponse, err := json.Marshal(EBSDeleteResponse{Volume: "vol-123", Success: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"Volume":"vol-123", "Success":true, "Error":""}`, string(deleteResponse))
}
