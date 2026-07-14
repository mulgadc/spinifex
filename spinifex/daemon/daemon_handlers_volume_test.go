package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
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

// seedVolumeConfig writes a minimal VolumeConfig to config.json, matching
// the seeding pattern used by the handleAttachVolume tests in
// daemon_handlers_test.go.
func seedVolumeConfig(t *testing.T, store *objectstore.MemoryObjectStore, volumeID string, meta viperblock.VolumeMetadata) {
	t.Helper()
	wrapper := struct {
		VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
	}{
		VolumeConfig: viperblock.VolumeConfig{VolumeMetadata: meta},
	}
	data, err := json.Marshal(wrapper)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &awss3.PutObjectInput{
		Bucket: aws.String("test-bucket"),
		Key:    aws.String(volumeID + "/config.json"),
		Body:   strings.NewReader(string(data)),
	})
	require.NoError(t, err)
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

			seedVolumeConfig(t, store, volumeID, viperblock.VolumeMetadata{
				VolumeID:         volumeID,
				SizeGiB:          10,
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

			command := types.EC2InstanceCommand{
				ID: instanceID,
				Attributes: types.EC2CommandAttributes{
					AttachVolume: true,
				},
				AttachVolumeData: &types.AttachVolumeData{
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

	seedVolumeConfig(t, store, volumeID, viperblock.VolumeMetadata{
		VolumeID:         volumeID,
		SizeGiB:          10,
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

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
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

	seedVolumeConfig(t, store, volumeID, viperblock.VolumeMetadata{
		VolumeID:         volumeID,
		SizeGiB:          10,
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

	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			AttachVolume: true,
		},
		AttachVolumeData: &types.AttachVolumeData{
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
