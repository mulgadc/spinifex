package daemon

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmountNeverReleasesBootVolume locks the stop/crash attachment-persistence
// contract: Unmount seals the block map but must NOT mark a boot/root volume
// "available". Unmount is driven by stop (vm/shutdown.go) and crash recovery
// (vm/crash_recovery.go), where the instance keeps its attachment and restarts —
// only DetachVolume and terminate release a boot volume. Releasing it here while
// the instance still owns it splits the volume-state record (describe-instances
// "attached" vs describe-volumes "available"). EFI is not a
// sufficient proxy for "boot": BIOS-boot roots are Boot && !EFI, so the gate must
// also exclude Boot. Only non-boot data volumes may be released by Unmount.
func TestUnmountNeverReleasesBootVolume(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	volState := &recordingVolState{}
	adapter := newVolumeMounterAdapter(daemon.natsConn, daemon.node, volState)

	// Stand in for the ebs daemon: every ebs.<node>.unmount succeeds (sealed).
	sub, err := daemon.natsConn.Subscribe(adapter.topic("unmount"), func(msg *nats.Msg) {
		var req types.EBSRequest
		require.NoError(t, json.Unmarshal(msg.Data, &req))
		data, err := json.Marshal(types.EBSUnMountResponse{Volume: req.Name, Mounted: false})
		require.NoError(t, err)
		require.NoError(t, msg.Respond(data))
	})
	require.NoError(t, err)
	defer sub.Unsubscribe()

	inst := &vm.VM{
		ID: "i-unmount-boot",
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{
				{Name: "vol-bios-root", Boot: true, EFI: false},
				{Name: "vol-efi-root", Boot: true, EFI: true},
				{Name: "vol-data", Boot: false, EFI: false},
			},
		},
	}
	require.NoError(t, adapter.Unmount(inst))

	calls := volState.snapshot()
	assert.NotContains(t, calls, "vol-bios-root:available",
		"a BIOS-boot root volume (Boot && !EFI) must stay attached across stop/crash unmount, not be released to available")
	assert.NotContains(t, calls, "vol-efi-root:available",
		"an EFI-boot root volume must stay attached across stop/crash unmount")
	assert.Contains(t, calls, "vol-data:available",
		"a non-boot data volume is released to available by Unmount")
}
