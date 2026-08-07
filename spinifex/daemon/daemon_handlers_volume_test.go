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
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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
func seedVolumeConfig(t *testing.T, daemon *Daemon, store *objectstore.MemoryObjectStore, volumeID string, meta viperblock.VolumeMetadata) {
	t.Helper()
	seedProviderVolume(t, daemon, volumeID, int64(meta.SizeGiB))
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

			seedVolumeConfig(t, daemon, store, volumeID, viperblock.VolumeMetadata{
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

	seedVolumeConfig(t, daemon, store, volumeID, viperblock.VolumeMetadata{
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

	seedVolumeConfig(t, daemon, store, volumeID, viperblock.VolumeMetadata{
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

// drainCommandFor builds the drain command a snapshot addresses to the node
// hosting instanceID.
func drainCommandFor(t *testing.T, instanceID, volumeID string) []byte {
	t.Helper()
	command := types.EC2InstanceCommand{
		ID:              instanceID,
		Attributes:      types.EC2CommandAttributes{DrainVolume: true},
		DrainVolumeData: &types.DrainVolumeData{VolumeID: volumeID},
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)
	return data
}

// drainTestDaemon returns a daemon owning a running instance, with a data dir
// short enough to hold the drain socket path.
func drainTestDaemon(t *testing.T, instanceID string) *Daemon {
	t.Helper()
	return drainTestDaemonInState(t, instanceID, vm.StateRunning)
}

// drainTestDaemonInState is drainTestDaemon for an instance the node still
// holds in some other state.
func drainTestDaemonInState(t *testing.T, instanceID string, status vm.InstanceState) *Daemon {
	t.Helper()
	daemon := createTestDaemon(t, sharedNATSURL)
	daemon.config.DataDir = testutil.SocketTempDir(t)
	daemon.vmMgr.Insert(&vm.VM{
		ID:           instanceID,
		Status:       status,
		AccountID:    testAccountID,
		InstanceType: getTestInstanceType(t),
		Instance:     &ec2.Instance{},
	})
	return daemon
}

// drainRequest issues a drain on the instance's command subject, returning the
// raw reply.
func drainRequest(t *testing.T, daemon *Daemon, instanceID, volumeID string) *nats.Msg {
	t.Helper()
	reply, err := sendDrainCommand(daemon, instanceID, drainCommandFor(t, instanceID, volumeID))
	require.NoError(t, err)
	return reply
}

// sendDrainCommand is drainRequest without the assertions, so a test can issue
// one from a goroutine and assert on the result back on the test goroutine.
func sendDrainCommand(daemon *Daemon, instanceID string, command []byte) (*nats.Msg, error) {
	msg := nats.NewMsg("ec2.cmd." + instanceID)
	msg.Data = command
	msg.Header.Set(utils.AccountIDHeader, testAccountID)
	return daemon.natsConn.RequestMsg(msg, 5*time.Second)
}

// subscribeEC2Commands wires the daemon's real dispatcher onto the per-instance
// command subject, so tests exercise the same serial delivery production has.
func subscribeEC2Commands(t *testing.T, daemon *Daemon, instanceID string) {
	t.Helper()
	sub, err := daemon.natsConn.Subscribe("ec2.cmd."+instanceID, daemon.handleEC2Events)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// A drain routed to the node hosting the instance reaches the local socket and
// acks, which is what lets a snapshot answered elsewhere read a current
// checkpoint.
func TestDrainVolume_HostNodeAcksLocalSocket(t *testing.T) {
	const instanceID, volumeID = "i-drain-ok", "vol-drain-ok"
	daemon := drainTestDaemon(t, instanceID)
	testutil.StartDrainSocket(t, daemon.config.DataDir, volumeID, "OK\n")

	resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
		testAccountID, drainCommandFor(t, instanceID, volumeID))

	var ack types.DrainVolumeResponse
	require.NoError(t, json.Unmarshal(resp.Data, &ack))
	assert.Equal(t, volumeID, ack.VolumeID)
	assert.Equal(t, types.DrainVolumeStatusDrained, ack.Status)
}

// The owning node having no socket for the volume means the writes cannot be
// flushed. That must reach the caller as an error, never as a silent success:
// the caller would otherwise snapshot a stale checkpoint.
func TestDrainVolume_HostNodeWithoutSocketFails(t *testing.T) {
	const instanceID, volumeID = "i-drain-nosock", "vol-drain-nosock"
	daemon := drainTestDaemon(t, instanceID)

	resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
		testAccountID, drainCommandFor(t, instanceID, volumeID))

	assert.Equal(t, awserrors.ErrorServerInternal, replyErrCode(t, resp.Data))
}

// A plugin that refuses the drain (ERR rather than OK) is a failure, not an ack.
func TestDrainVolume_HostNodeRelaysSocketFailure(t *testing.T) {
	const instanceID, volumeID = "i-drain-err", "vol-drain-err"
	daemon := drainTestDaemon(t, instanceID)
	testutil.StartDrainSocket(t, daemon.config.DataDir, volumeID, "ERR\n")

	resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
		testAccountID, drainCommandFor(t, instanceID, volumeID))

	assert.Equal(t, awserrors.ErrorServerInternal, replyErrCode(t, resp.Data))
}

// A drain command with no volume is a caller error, not a server fault.
func TestDrainVolume_MissingVolumeDataIsInvalidParameter(t *testing.T) {
	const instanceID = "i-drain-novol"
	daemon := drainTestDaemon(t, instanceID)

	command := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{DrainVolume: true},
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)

	resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
		testAccountID, data)

	assert.Equal(t, awserrors.ErrorInvalidParameterValue, replyErrCode(t, resp.Data))
}

// Only the instance's owner may drain its volume: the command carries the same
// ownership gate as every other per-instance command.
func TestDrainVolume_ForeignAccountRejected(t *testing.T) {
	const instanceID, volumeID = "i-drain-foreign", "vol-drain-foreign"
	daemon := drainTestDaemon(t, instanceID)
	testutil.StartDrainSocket(t, daemon.config.DataDir, volumeID, "OK\n")

	resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
		"999988887777", drainCommandFor(t, instanceID, volumeID))

	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, replyErrCode(t, resp.Data))
}

// Only stable post-teardown states prove the volume is sealed. These states
// acknowledge not-running without touching the absent plugin socket.
func TestDrainVolume_CompletedTeardownAcksNotRunning(t *testing.T) {
	for _, status := range []vm.InstanceState{vm.StateStopped, vm.StateTerminated} {
		t.Run(string(status), func(t *testing.T) {
			instanceID := "i-drain-" + string(status)
			volumeID := "vol-drain-" + string(status)
			daemon := drainTestDaemonInState(t, instanceID, status)

			resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
				testAccountID, drainCommandFor(t, instanceID, volumeID))

			var ack types.DrainVolumeResponse
			require.NoError(t, json.Unmarshal(resp.Data, &ack))
			assert.Equal(t, volumeID, ack.VolumeID)
			assert.Equal(t, types.DrainVolumeStatusNotRunning, ack.Status)
		})
	}
}

// Transitional and uncertain states are not proof of a sealed checkpoint. An
// absent socket in any of them must fail rather than silently snapshot stale
// data while launch or teardown may still own the volume.
func TestDrainVolume_UnsealedStatesRequireDrain(t *testing.T) {
	statuses := []vm.InstanceState{
		vm.StateRunning,
		vm.StateStopping,
		vm.StateShuttingDown,
		vm.StatePending,
		vm.StateProvisioning,
		vm.StateError,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			instanceID := "i-drain-" + string(status)
			volumeID := "vol-drain-" + string(status)
			daemon := drainTestDaemonInState(t, instanceID, status)

			resp := requestHandler(t, daemon.natsConn, "ec2.cmd."+instanceID, daemon.handleEC2Events,
				testAccountID, drainCommandFor(t, instanceID, volumeID))

			assert.Equal(t, awserrors.ErrorServerInternal, replyErrCode(t, resp.Data))
		})
	}
}

// nats.go delivers a subscription's messages serially, so a drain run inline
// would hold ec2.cmd.{instanceID} for the length of the flush — stalling stop,
// terminate and hot-plug for that instance. A drain still flushing must not
// keep the next command waiting.
func TestDrainVolume_SlowDrainDoesNotBlockTheCommandSubject(t *testing.T) {
	const instanceID = "i-drain-parallel"
	daemon := drainTestDaemon(t, instanceID)
	accepted, release := testutil.StartBlockingDrainSocket(t, daemon.config.DataDir, "vol-drain-slow", "OK\n")
	testutil.StartDrainSocket(t, daemon.config.DataDir, "vol-drain-fast", "OK\n")
	subscribeEC2Commands(t, daemon, instanceID)

	type reply struct {
		msg *nats.Msg
		err error
	}
	slowCommand := drainCommandFor(t, instanceID, "vol-drain-slow")
	slow := make(chan reply, 1)
	go func() {
		msg, err := sendDrainCommand(daemon, instanceID, slowCommand)
		slow <- reply{msg, err}
	}()

	// Only assert once the slow drain is genuinely mid-flush.
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow drain never reached its socket")
	}

	fast := drainRequest(t, daemon, instanceID, "vol-drain-fast")
	var ack types.DrainVolumeResponse
	require.NoError(t, json.Unmarshal(fast.Data, &ack))
	assert.Equal(t, types.DrainVolumeStatusDrained, ack.Status)

	release()
	select {
	case got := <-slow:
		require.NoError(t, got.err)
		require.NoError(t, json.Unmarshal(got.msg.Data, &ack))
		assert.Equal(t, types.DrainVolumeStatusDrained, ack.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("the released drain never replied")
	}
}

// Running the drain off the delivery goroutine must not leak it: the goroutine
// has to be gone once the ack is on the wire, or a snapshot loop against a
// wedged plugin accumulates them.
func TestDrainVolume_DispatchedGoroutineDoesNotLeak(t *testing.T) {
	const instanceID, volumeID = "i-drain-leak", "vol-drain-leak"
	daemon := drainTestDaemon(t, instanceID)
	testutil.StartDrainSocket(t, daemon.config.DataDir, volumeID, "OK\n")
	subscribeEC2Commands(t, daemon, instanceID)

	// Take the baseline after one full round trip: the first request is what
	// makes nats.go stand up its long-lived response-inbox goroutine, which
	// would otherwise read as the leak.
	var ack types.DrainVolumeResponse
	require.NoError(t, json.Unmarshal(drainRequest(t, daemon, instanceID, volumeID).Data, &ack))
	require.Equal(t, types.DrainVolumeStatusDrained, ack.Status)
	ignoreExisting := goleak.IgnoreCurrent()

	require.NoError(t, json.Unmarshal(drainRequest(t, daemon, instanceID, volumeID).Data, &ack))
	require.Equal(t, types.DrainVolumeStatusDrained, ack.Status)

	goleak.VerifyNone(t, ignoreExisting)
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
