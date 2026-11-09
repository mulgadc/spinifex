package daemon

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stopped or crashed instance still owns its data volumes, so Unmount seals
// them and releases the node-local mount without touching their state.
func TestUnmount_LeavesDataVolumesAttached(t *testing.T) {
	t.Parallel()
	daemon := createTestDaemon(t, sharedNATSURL)
	adapter := newNATSMounter(daemon.natsConn, daemon.node)
	fake := serveFakeNodeProvider(t, daemon.natsConn, daemon.node)

	inst := &vm.VM{
		ID:        "i-stop-keep",
		AccountID: "000000000042",
		EBSRequests: vm.EBSRequests{
			Requests: []vm.EBSRequest{
				{Name: "vol-root", Boot: true},
				{Name: "vol-efi", Boot: true, EFI: true},
				{Name: "vol-data-a", DeviceName: "/dev/sdf"},
				{Name: "vol-data-b", DeviceName: "/dev/sdg"},
			},
		},
	}
	require.NoError(t, adapter.Unmount(t.Context(), inst))

	assert.ElementsMatch(t, []string{"vol-root", "vol-efi", "vol-data-a", "vol-data-b"}, fake.unpublished(),
		"every volume is still sealed on stop")
}

// The point of keeping the attachment: after the stopped instance's unmount,
// another instance cannot take the volume, which is what AWS does.
func TestStoppedInstanceVolume_CannotBeAttachedElsewhere(t *testing.T) {
	t.Parallel()
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	const (
		stoppedID = "i-stopped-holder"
		otherID   = "i-wants-the-volume"
		volumeID  = "vol-held-by-stopped"
	)
	seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "in-use",
		TenantID:         testAccountID,
		AttachedInstance: stoppedID,
		DeviceName:       "/dev/sdf",
	})

	serveFakeNodeProvider(t, daemon.natsConn, daemon.node)

	adapter := newNATSMounter(daemon.natsConn, daemon.node)
	stopped := &vm.VM{
		ID:        stoppedID,
		AccountID: testAccountID,
		EBSRequests: vm.EBSRequests{
			Requests: []vm.EBSRequest{{Name: volumeID, DeviceName: "/dev/sdf"}},
		},
	}
	require.NoError(t, adapter.Unmount(t.Context(), stopped))

	other := &vm.VM{
		ID:           otherID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.vmMgr.Insert(other)
	sub, err := daemon.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", otherID), daemon.handleEC2Events)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmdData, err := json.Marshal(ec2v1.EC2InstanceCommand{
		ID:               otherID,
		Attributes:       ec2v1.EC2CommandAttributes{AttachVolume: true},
		AttachVolumeData: &ec2v1.AttachVolumeData{VolumeID: volumeID},
	})
	require.NoError(t, err)
	resp, err := natsRequest(daemon.natsConn, fmt.Sprintf("ec2.cmd.%s", otherID), cmdData, 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "VolumeInUse")
}
