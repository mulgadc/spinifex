package daemon

import (
	"testing"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmountNeverReleasesBootVolume locks the stop/crash attachment-persistence
// contract: Unmount seals the block map but never marks any attached volume
// "available", boot or data. Unmount is driven by stop (runtime/compute/vm/shutdown.go)
// and crash recovery (runtime/compute/vm/crash_recovery.go), where the instance keeps
// its attachments and restarts; as on AWS, EBS root and data volumes persist across
// stop. Only DetachVolume and terminate release a volume. Releasing one here while
// the instance still owns it splits the volume-state record (describe-instances
// "attached" vs describe-volumes "available").
func TestUnmountNeverReleasesBootVolume(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)
	volState := &recordingVolState{}
	adapter := newNATSMounter(daemon.natsConn, daemon.node, volState)

	// Stand in for the provider: every unpublish succeeds (sealed).
	serveFakeNodeProvider(t, daemon.natsConn, daemon.node)

	inst := &vm.VM{
		ID: "i-unmount-boot",
		EBSRequests: vm.EBSRequests{
			Requests: []viperblocklegacyv1.EBSRequest{
				{Name: "vol-bios-root", Boot: true, EFI: false},
				{Name: "vol-efi-root", Boot: true, EFI: true},
				{Name: "vol-data", Boot: false, EFI: false},
			},
		},
	}
	require.NoError(t, adapter.Unmount(t.Context(), inst))

	calls := volState.snapshot()
	assert.NotContains(t, calls, "vol-bios-root:available",
		"a BIOS-boot root volume (Boot && !EFI) must stay attached across stop/crash unmount, not be released to available")
	assert.NotContains(t, calls, "vol-efi-root:available",
		"an EFI-boot root volume must stay attached across stop/crash unmount")
	assert.NotContains(t, calls, "vol-data:available",
		"a data volume stays in-use across stop/crash unmount, not released to available")
}
