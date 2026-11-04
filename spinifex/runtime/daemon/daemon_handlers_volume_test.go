package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAttachDetachErrorCode locks down the manager-error → AWS-API-code
// mapping that handleAttachVolume and handleDetachVolume both call. Wrong
// mapping silently breaks AWS-SDK retry semantics: clients expect 4xx
// codes for caller-fixable problems and 5xx for server faults. A future
// edit that drops a sentinel branch would otherwise pass the mechanical
// "tests still compile" bar with no signal.
func TestAttachDetachErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ErrInstanceNotFound maps to InvalidInstanceID.NotFound",
			err:  vm.ErrInstanceNotFound,
			want: awserrors.ErrorInvalidInstanceIDNotFound,
		},
		{
			name: "wrapped ErrInstanceNotFound still matches via errors.Is",
			err:  fmt.Errorf("manager: %w", vm.ErrInstanceNotFound),
			want: awserrors.ErrorInvalidInstanceIDNotFound,
		},
		{
			name: "ErrInvalidTransition maps to IncorrectInstanceState",
			err:  vm.ErrInvalidTransition,
			want: awserrors.ErrorIncorrectInstanceState,
		},
		{
			name: "wrapped ErrInvalidTransition still matches",
			err:  fmt.Errorf("cannot attach in state stopped: %w", vm.ErrInvalidTransition),
			want: awserrors.ErrorIncorrectInstanceState,
		},
		{
			name: "ErrHotAttachUnsupported maps to UnsupportedOperation",
			err:  vm.ErrHotAttachUnsupported,
			want: awserrors.ErrorUnsupportedOperation,
		},
		{
			name: "wrapped ErrHotAttachUnsupported still matches",
			err:  fmt.Errorf("%w: instance i-db", vm.ErrHotAttachUnsupported),
			want: awserrors.ErrorUnsupportedOperation,
		},
		{
			name: "ErrAttachmentLimitExceeded maps to AttachmentLimitExceeded",
			err:  vm.ErrAttachmentLimitExceeded,
			want: awserrors.ErrorAttachmentLimitExceeded,
		},
		{
			name: "ErrVolumeNotAttached maps to IncorrectState",
			err:  vm.ErrVolumeNotAttached,
			want: awserrors.ErrorIncorrectState,
		},
		{
			name: "wrapped ErrVolumeNotAttached still matches",
			err:  fmt.Errorf("%w: vol-1", vm.ErrVolumeNotAttached),
			want: awserrors.ErrorIncorrectState,
		},
		{
			name: "ErrVolumeNotDetachable maps to OperationNotPermitted",
			err:  vm.ErrVolumeNotDetachable,
			want: awserrors.ErrorOperationNotPermitted,
		},
		{
			name: "ErrVolumeDeviceMismatch maps to InvalidParameterValue",
			err:  vm.ErrVolumeDeviceMismatch,
			want: awserrors.ErrorInvalidParameterValue,
		},
		{
			name: "a mount refused behind a settling seal maps to IncorrectState",
			err:  fmt.Errorf("mount volume vol-1: publish volume vol-1: %w: %w", vm.ErrVolumeSettling, vm.ErrMountRetryable),
			want: awserrors.ErrorIncorrectState,
		},
		{
			name: "unknown error falls through to ServerInternal",
			err:  errors.New("QMP blockdev-add: connection refused"),
			want: awserrors.ErrorServerInternal,
		},
		{
			name: "wrapped unknown error falls through to ServerInternal",
			err:  fmt.Errorf("manager: %w", errors.New("nbdkit timeout")),
			want: awserrors.ErrorServerInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := attachDetachErrorCode(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// seedVolumeConfig makes a volume exist on both sides at once: the provider
// holds the blocks, the ebsmetadata document is what the control plane reads.
func seedVolumeConfig(t *testing.T, daemon *Daemon, store *objectstore.MemoryObjectStore, volume ebsmetadata.Volume) {
	t.Helper()
	seedProviderVolume(t, daemon, volume.VolumeID, safecast.Uint64ToInt64(volume.CapacityGiB))
	seedVolumeDocument(t, store, volume)
}

// TestAttachVolume_IdempotentSameInstance verifies that re-attaching a
// volume already attached to the requesting instance (e.g. a CSI
// ControllerPublishVolume retry after a slow first attach) short-circuits
// to an idempotent "attached" success instead of VolumeInUse, and does not
// invoke vm.Manager.AttachVolume (the fake QMPClient on the seeded instance
// has no live socket, so a real AttachVolume call would fail rather than
// silently succeed with the pre-existing device).
func TestAttachVolume_IdempotentSameInstance(t *testing.T) {
	tests := []struct {
		name            string
		requestedDevice string
	}{
		{name: "no device specified", requestedDevice: ""},
		{name: "device matches existing attachment", requestedDevice: "/dev/sdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

			instanceID := "i-attach-idempotent-" + strings.ReplaceAll(tt.name, " ", "-")
			volumeID := "vol-idempotent-" + strings.ReplaceAll(tt.name, " ", "-")

			instance := &vm.VM{
				ID:           instanceID,
				Status:       vm.StateRunning,
				AccountID:    testAccountID,
				InstanceType: getTestInstanceType(t),
				Instance:     &ec2.Instance{},
				QMPClient:    &qmp.QMPClient{},
			}
			daemon.vmMgr.Insert(instance)

			seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
				VolumeID:         volumeID,
				CapacityGiB:      10,
				State:            "in-use",
				TenantID:         testAccountID,
				AttachedInstance: instanceID,
				DeviceName:       "/dev/sdf",
			})

			sub, err := daemon.natsConn.Subscribe(
				fmt.Sprintf("ec2.cmd.%s", instanceID),
				daemon.handleEC2Events,
			)
			require.NoError(t, err)
			defer sub.Unsubscribe()

			command := ec2v1.EC2InstanceCommand{
				ID: instanceID,
				Attributes: ec2v1.EC2CommandAttributes{
					AttachVolume: true,
				},
				AttachVolumeData: &ec2v1.AttachVolumeData{
					VolumeID: volumeID,
					Device:   tt.requestedDevice,
				},
			}
			cmdData, err := json.Marshal(command)
			require.NoError(t, err)

			resp, err := natsRequest(daemon.natsConn,
				fmt.Sprintf("ec2.cmd.%s", instanceID),
				cmdData,
				5*time.Second,
			)
			require.NoError(t, err)

			var attachment ec2.VolumeAttachment
			require.NoError(t, json.Unmarshal(resp.Data, &attachment))

			assert.Equal(t, volumeID, *attachment.VolumeId)
			assert.Equal(t, instanceID, *attachment.InstanceId)
			assert.Equal(t, "/dev/sdf", *attachment.Device)
			assert.Equal(t, "attached", *attachment.State)
		})
	}
}

// TestAttachVolume_InUseDifferentInstance is a regression test locking down
// that a volume attached to a DIFFERENT instance than the requester still
// returns VolumeInUse unchanged, distinguishing it from the same-instance
// idempotent short-circuit.
func TestAttachVolume_InUseDifferentInstance(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-vol-other-instance"
	otherInstanceID := "i-already-attached-elsewhere"
	volumeID := "vol-in-use-other-instance"

	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.vmMgr.Insert(instance)

	seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "in-use",
		TenantID:         testAccountID,
		AttachedInstance: otherInstanceID,
		DeviceName:       "/dev/sdf",
	})

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := ec2v1.EC2InstanceCommand{
		ID: instanceID,
		Attributes: ec2v1.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &ec2v1.AttachVolumeData{
			VolumeID: volumeID,
		},
	}
	cmdData, err := json.Marshal(command)
	require.NoError(t, err)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "VolumeInUse")
}

// TestAttachVolume_DeletingVolumeRefused pins the half of the delete contract
// that is not about the delete itself: a volume whose teardown is already owed
// must take no new dependant, because the objects behind it are being removed.
// DeleteVolume records the obligation and returns, so between the call and the
// reclaimer finishing it the volume is detached and would otherwise look
// attachable.
func TestAttachVolume_DeletingVolumeRefused(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-vol-deleting"
	volumeID := "vol-deleting-attach"

	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.vmMgr.Insert(instance)

	seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
		VolumeID:    volumeID,
		CapacityGiB: 10,
		State:       "deleting",
		TenantID:    testAccountID,
	})

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := ec2v1.EC2InstanceCommand{
		ID:               instanceID,
		Attributes:       ec2v1.EC2CommandAttributes{AttachVolume: true},
		AttachVolumeData: &ec2v1.AttachVolumeData{VolumeID: volumeID},
	}
	cmdData, err := json.Marshal(command)
	require.NoError(t, err)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), "VolumeInUse",
		"a volume already recorded for deletion must not take a new attachment")
}

// TestAttachVolume_IdempotentSameInstance_DeviceMismatch verifies that a
// same-instance re-attach requesting a DIFFERENT device than the one
// already attached is treated as a real CSI conflict — AWS returns
// VolumeInUse for this case — not silently echoed back or accepted.
func TestAttachVolume_IdempotentSameInstance_DeviceMismatch(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-device-mismatch"
	volumeID := "vol-device-mismatch"

	instance := &vm.VM{
		ID:           instanceID,
		Status:       vm.StateRunning,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
		QMPClient:    &qmp.QMPClient{},
	}
	daemon.vmMgr.Insert(instance)

	seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
		VolumeID:         volumeID,
		CapacityGiB:      10,
		State:            "in-use",
		TenantID:         testAccountID,
		AttachedInstance: instanceID,
		DeviceName:       "/dev/sdf",
	})

	sub, err := daemon.natsConn.Subscribe(
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		daemon.handleEC2Events,
	)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	command := ec2v1.EC2InstanceCommand{
		ID: instanceID,
		Attributes: ec2v1.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &ec2v1.AttachVolumeData{
			VolumeID: volumeID,
			Device:   "/dev/sdg",
		},
	}
	cmdData, err := json.Marshal(command)
	require.NoError(t, err)

	resp, err := natsRequest(daemon.natsConn,
		fmt.Sprintf("ec2.cmd.%s", instanceID),
		cmdData,
		5*time.Second,
	)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), awserrors.ErrorVolumeInUse)
}

// seedProviderVolume allocates volumeID in the daemon's EBS provider so
// operations that reach the provider (expand, delete) find it there.
func seedProviderVolume(t *testing.T, daemon *Daemon, volumeID string, sizeGiB int64) {
	t.Helper()
	if daemon == nil || daemon.ebsProvider == nil || volumeID == "" || sizeGiB <= 0 {
		return
	}
	_, err := daemon.ebsProvider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: sizeGiB * 1024 * 1024 * 1024},
	})
	require.NoError(t, err)
}

// A volume still being prepared is never attachable: the request is refused
// before any mount and the record keeps no attachment.
func TestAttachVolume_RefusesACreatingVolume(t *testing.T) {
	daemon, store := createFullTestDaemonWithStore(t, sharedNATSURL)

	instanceID := "i-attach-creating"
	volumeID := "vol-attach-creating"
	daemon.vmMgr.Insert(&vm.VM{
		ID: instanceID, Status: vm.StateRunning, AccountID: testAccountID,
		InstanceType: getTestInstanceType(t), Instance: &ec2.Instance{}, QMPClient: &qmp.QMPClient{},
	})
	seedVolumeConfig(t, daemon, store, ebsmetadata.Volume{
		VolumeID: volumeID, CapacityGiB: 10, State: "creating", TenantID: testAccountID,
	})

	sub, err := daemon.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instanceID), daemon.handleEC2Events)
	require.NoError(t, err)
	defer sub.Unsubscribe()

	cmdData, err := json.Marshal(ec2v1.EC2InstanceCommand{
		ID:               instanceID,
		Attributes:       ec2v1.EC2CommandAttributes{AttachVolume: true},
		AttachVolumeData: &ec2v1.AttachVolumeData{VolumeID: volumeID, Device: "/dev/sdf"},
	})
	require.NoError(t, err)
	resp, err := natsRequest(daemon.natsConn, fmt.Sprintf("ec2.cmd.%s", instanceID), cmdData, 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Data), awserrors.ErrorVolumeInUse)

	meta, err := ebsmetadata.NewStore(store, daemon.config.Predastore.Bucket).GetVolume(t.Context(), testAccountID, volumeID)
	require.NoError(t, err)
	assert.Equal(t, "creating", meta.State)
	assert.Empty(t, meta.AttachedInstance)
}
