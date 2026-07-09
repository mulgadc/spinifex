package vm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMockQMPClient creates a QMPClient backed by an in-memory pipe. The
// responder is invoked for every command and returns the JSON object the
// server should reply with (e.g. {"return": {}} or {"error": {...}}). A
// nil responder reply defaults to an empty success.
func newMockQMPClient(t *testing.T, responder func(qmp.QMPCommand) map[string]any) (*qmp.QMPClient, func()) {
	t.Helper()
	clientConn, serverConn := net.Pipe()

	client := &qmp.QMPClient{
		Conn:    clientConn,
		Decoder: json.NewDecoder(clientConn),
		Encoder: json.NewEncoder(clientConn),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		dec := json.NewDecoder(serverConn)
		enc := json.NewEncoder(serverConn)
		for {
			var cmd qmp.QMPCommand
			if err := dec.Decode(&cmd); err != nil {
				return
			}
			var resp map[string]any
			if responder != nil {
				resp = responder(cmd)
			}
			if resp == nil {
				resp = map[string]any{"return": map[string]any{}}
			}
			if err := enc.Encode(resp); err != nil {
				return
			}
		}
	}()

	cancel := func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		<-done
	}
	return client, cancel
}

// qmpRecorder records the order of QMP commands for assertion in rollback
// tests. Safe for concurrent use because sendQMPCommand serializes via the
// QMPClient mutex, but we lock anyway for clarity.
type qmpRecorder struct {
	mu       sync.Mutex
	commands []qmp.QMPCommand
}

func (r *qmpRecorder) record(cmd qmp.QMPCommand) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, cmd)
}

func (r *qmpRecorder) executes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.commands))
	for i, c := range r.commands {
		out[i] = c.Execute
	}
	return out
}

func TestNextAvailableDevice(t *testing.T) {
	tests := []struct {
		name       string
		instance   *VM
		wantDevice string
	}{
		{
			name: "empty instance returns first device",
			instance: &VM{
				Instance: &ec2.Instance{},
			},
			wantDevice: "/dev/sdf",
		},
		{
			name:       "nil Instance returns first device",
			instance:   &VM{},
			wantDevice: "/dev/sdf",
		},
		{
			name: "existing BlockDeviceMappings skipped",
			instance: &VM{
				Instance: &ec2.Instance{
					BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
						{DeviceName: aws.String("/dev/sdf")},
						{DeviceName: aws.String("/dev/sdg")},
					},
				},
			},
			wantDevice: "/dev/sdh",
		},
		{
			name: "existing EBSRequests skipped",
			instance: &VM{
				Instance: &ec2.Instance{},
				EBSRequests: types.EBSRequests{
					Requests: []types.EBSRequest{
						{Name: "vol-1", DeviceName: "/dev/sdf"},
					},
				},
			},
			wantDevice: "/dev/sdg",
		},
		{
			name: "mixed sources all skipped",
			instance: &VM{
				Instance: &ec2.Instance{
					BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
						{DeviceName: aws.String("/dev/sdf")},
					},
				},
				EBSRequests: types.EBSRequests{
					Requests: []types.EBSRequest{
						{Name: "vol-1", DeviceName: "/dev/sdg"},
					},
				},
			},
			wantDevice: "/dev/sdh",
		},
		{
			name: "all devices f-p used returns empty",
			instance: func() *VM {
				var bdms []*ec2.InstanceBlockDeviceMapping
				for c := 'f'; c <= 'p'; c++ {
					dev := fmt.Sprintf("/dev/sd%c", c)
					bdms = append(bdms, &ec2.InstanceBlockDeviceMapping{
						DeviceName: aws.String(dev),
					})
				}
				return &VM{
					Instance: &ec2.Instance{BlockDeviceMappings: bdms},
				}
			}(),
			wantDevice: "",
		},
		{
			name: "nil DeviceName in BlockDeviceMappings ignored",
			instance: &VM{
				Instance: &ec2.Instance{
					BlockDeviceMappings: []*ec2.InstanceBlockDeviceMapping{
						{DeviceName: nil},
						{DeviceName: aws.String("/dev/sdf")},
					},
				},
			},
			wantDevice: "/dev/sdg",
		},
		{
			name: "empty DeviceName in EBSRequests ignored",
			instance: &VM{
				EBSRequests: types.EBSRequests{
					Requests: []types.EBSRequest{
						{DeviceName: ""},
						{DeviceName: "/dev/sdf"},
					},
				},
			},
			wantDevice: "/dev/sdg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextAvailableDevice(tt.instance)
			assert.Equal(t, tt.wantDevice, got)
		})
	}
}

func TestIsQMPDeviceNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "matches DeviceNotFound", err: errors.New("QMP error: DeviceNotFound: device 'vdisk-x' not found"), want: true},
		{name: "other QMP error", err: errors.New("QMP error: GenericError: bad command"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isQMPDeviceNotFound(tt.err))
		})
	}
}

func TestIsQMPNodeInUse(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "matches 'is in use'", err: errors.New("QMP error: GenericError: Node 'nbd-x' is in use"), want: true},
		{name: "matches 'still in use'", err: errors.New("QMP error: GenericError: Node 'nbd-x' is still in use"), want: true},
		{name: "other QMP error", err: errors.New("QMP error: GenericError: not found"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isQMPNodeInUse(tt.err))
		})
	}
}

func TestIsQMPNodeNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "matches 'Failed to find node'", err: errors.New("QMP error: GenericError: Failed to find node with node-name='nbd-x'"), want: true},
		{name: "matches 'Cannot find device'", err: errors.New("QMP error: GenericError: Cannot find device='' nor node-name='nbd-x'"), want: true},
		{name: "in-use is not not-found", err: errors.New("QMP error: GenericError: Node 'nbd-x' is in use"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isQMPNodeNotFound(tt.err))
		})
	}
}

func TestAttachVolume_InstanceNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.AttachVolume(t.Context(), "i-missing", "vol-1", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestAttachVolume_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped, Instance: &ec2.Instance{}})

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestAttachVolume_AttachmentLimitExceeded(t *testing.T) {
	bdms := make([]*ec2.InstanceBlockDeviceMapping, 0, 11)
	for c := 'f'; c <= 'p'; c++ {
		dev := fmt.Sprintf("/dev/sd%c", c)
		bdms = append(bdms, &ec2.InstanceBlockDeviceMapping{DeviceName: aws.String(dev)})
	}
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{BlockDeviceMappings: bdms}})

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAttachmentLimitExceeded)
}

func TestDetachVolume_InstanceNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.DetachVolume(t.Context(), "i-missing", "vol-1", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestDetachVolume_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped})

	_, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestDetachVolume_VolumeNotAttached(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{}})

	_, err := m.DetachVolume(t.Context(), "i-1", "vol-missing", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeNotAttached)
}

// TestDetachVolume_DeviceGuards covers the four cross-check and undetachable
// guards at the head of DetachVolume. Bypassing any of these is destructive:
// detaching a boot/EFI volume kills the instance, and a device-name
// mismatch silently rips out the wrong disk. The mismatch case additionally
// pins the error text so an operator-visible diagnostic stays intact.
func TestDetachVolume_DeviceGuards(t *testing.T) {
	tests := []struct {
		name           string
		req            types.EBSRequest
		requestDevice  string
		expectQMP      bool
		wantErr        error
		wantErrSubstrs []string
	}{
		{
			name:          "boot volume rejected",
			req:           types.EBSRequest{Name: "vol-boot", Boot: true, DeviceName: "/dev/sdf"},
			requestDevice: "",
			wantErr:       ErrVolumeNotDetachable,
		},
		{
			name:          "EFI volume rejected",
			req:           types.EBSRequest{Name: "vol-efi", EFI: true, DeviceName: "/dev/sdf"},
			requestDevice: "",
			wantErr:       ErrVolumeNotDetachable,
		},
		{
			name:           "device mismatch surfaces expected and actual",
			req:            types.EBSRequest{Name: "vol-1", DeviceName: "/dev/sdf"},
			requestDevice:  "/dev/sdg",
			wantErr:        ErrVolumeDeviceMismatch,
			wantErrSubstrs: []string{"/dev/sdg", "/dev/sdf"},
		},
		{
			name:          "empty device skips cross-check and proceeds",
			req:           types.EBSRequest{Name: "vol-1", DeviceName: "/dev/sdf"},
			requestDevice: "",
			expectQMP:     true,
		},
		{
			name:          "matching device passes cross-check and proceeds",
			req:           types.EBSRequest{Name: "vol-1", DeviceName: "/dev/sdf"},
			requestDevice: "/dev/sdf",
			expectQMP:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &qmpRecorder{}
			qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
				recorder.record(cmd)
				return nil
			})
			defer cancel()

			mounter := &fakeVolumeMounter{}
			m := NewManagerWithDeps(Deps{VolumeMounter: mounter})
			m.Insert(&VM{
				ID:        "i-1",
				Status:    StateRunning,
				Instance:  &ec2.Instance{},
				QMPClient: qmpClient,
				EBSRequests: types.EBSRequests{
					Requests: []types.EBSRequest{tt.req},
				},
			})

			device, err := m.DetachVolume(t.Context(), "i-1", tt.req.Name, tt.requestDevice, false)

			if tt.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tt.wantErr)
				for _, s := range tt.wantErrSubstrs {
					assert.Contains(t, err.Error(), s,
						"error must surface %q so an operator sees expected vs actual", s)
				}
				assert.Empty(t, recorder.executes(),
					"guard rejection must short-circuit before any QMP traffic")
				assert.Empty(t, mounter.unmountedOne,
					"guard rejection must not invoke UnmountOne")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.req.DeviceName, device,
				"DetachVolume must echo the recorded API device name")
			executed := recorder.executes()
			require.NotEmpty(t, executed, "proceed path must issue QMP commands")
			assert.Equal(t, "device_del", executed[0],
				"proceed path begins with device_del")
			assert.Contains(t, executed, "blockdev-del",
				"proceed path must issue blockdev-del")
			assert.Equal(t, []string{tt.req.Name}, mounter.unmountedOne,
				"proceed path must invoke UnmountOne and, on success, complete the detach")
		})
	}
}

// TestDetachVolume_SealFailureGatesAvailable covers the durability gate in the
// detach-seal fix: when ebs.unmount (the synchronous block-map seal) fails,
// DetachVolume must propagate the error, must NOT flip the volume to "available",
// and must leave it in EBSRequests so the volume stays attached/retryable and its
// local WAL is retained. A regression back to fire-and-forget unmount would let a
// volume go available with an unsealed block map — the bad-superblock bug.
func TestDetachVolume_SealFailureGatesAvailable(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		return nil
	})
	defer cancel()

	sealErr := errors.New("seal volume: predastore unreachable")
	mounter := &fakeVolumeMounter{unmountOneErr: sealErr}
	stateUpdater := &fakeVolumeStateUpdater{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, VolumeStateUpdater: stateUpdater})
	m.Insert(&VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	_, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)

	require.Error(t, err)
	assert.ErrorIs(t, err, sealErr,
		"seal failure must propagate so the caller keeps the volume attached/retryable")
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne,
		"DetachVolume must still attempt the unmount/seal before gating")
	assert.Empty(t, stateUpdater.snapshot(),
		"a failed seal must not drive any volume-state update — least of all the available transition")

	instance, ok := m.Get("i-1")
	require.True(t, ok)
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()
	var retained bool
	for _, r := range instance.EBSRequests.Requests {
		if r.Name == "vol-1" {
			retained = true
		}
	}
	assert.True(t, retained,
		"volume must stay in EBSRequests after a seal failure so reattach/retry can recover it")
}

func TestReboot_InstanceNotFound(t *testing.T) {
	m := NewManager()
	err := m.Reboot(t.Context(), "i-missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestReboot_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped})

	err := m.Reboot(t.Context(), "i-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

// TestReboot_DoesNotFireHooks asserts that Manager.Reboot fires neither
// OnInstanceUp nor OnInstanceDown. The plan's "Hook firing contract" requires
// hooks to fire only on Pending→Running and terminal transitions; reboot keeps
// the VM in StateRunning across system_reset, so firing OnInstanceDown +
// OnInstanceUp would tear down per-instance NATS subs on every reboot.
func TestReboot_DoesNotFireHooks(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		return nil // success
	})
	defer cancel()

	var upCalls, downCalls int
	hooks := ManagerHooks{
		OnInstanceUp:   func(*VM) error { upCalls++; return nil },
		OnInstanceDown: func(string) { downCalls++ },
	}
	m := NewManagerWithDeps(Deps{Hooks: hooks})
	m.Insert(&VM{ID: "i-1", Status: StateRunning, QMPClient: qmpClient})

	require.NoError(t, m.Reboot(t.Context(), "i-1"))

	assert.Equal(t, 0, upCalls, "Reboot must not fire OnInstanceUp")
	assert.Equal(t, 0, downCalls, "Reboot must not fire OnInstanceDown")
	assert.Equal(t, []string{"system_reset"}, recorder.executes(),
		"Reboot must issue exactly one system_reset QMP command")
}

// TestReboot_DoesNotChangeStatus asserts the VM stays in StateRunning across
// reboot. Pairs with TestReboot_DoesNotFireHooks to lock down the hook
// contract: status doesn't transition, so hooks shouldn't fire.
func TestReboot_DoesNotChangeStatus(t *testing.T) {
	qmpClient, cancel := newMockQMPClient(t, nil)
	defer cancel()

	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateRunning, QMPClient: qmpClient})

	require.NoError(t, m.Reboot(t.Context(), "i-1"))

	v, ok := m.Get("i-1")
	require.True(t, ok)
	assert.Equal(t, StateRunning, m.Status(v))
}

func TestReboot_QMPFailureSurfacesError(t *testing.T) {
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		return map[string]any{
			"error": map[string]any{
				"class": "GenericError",
				"desc":  "guest is not running",
			},
		}
	})
	defer cancel()

	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateRunning, QMPClient: qmpClient})

	err := m.Reboot(t.Context(), "i-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "QMP system_reset")
}

// attachVolumeRunningInstance returns a manager with a single running VM
// wired with the supplied QMP client. Used by AttachVolume rollback tests.
func attachVolumeRunningInstance(t *testing.T, qmpClient *qmp.QMPClient, mounter VolumeMounter) (*Manager, *VM) {
	t.Helper()
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter})
	v := &VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
	}
	m.Insert(v)
	return m, v
}

// TestAttachVolume_MountAmbiguous_TriggersUnmountOne asserts that an
// ebs.mount response with an empty URI is treated as ambiguous: AttachVolume
// returns ErrMountAmbiguous and invokes UnmountOne so a half-started
// backend mount cannot be orphaned.
func TestAttachVolume_MountAmbiguous_TriggersUnmountOne(t *testing.T) {
	mounter := &fakeVolumeMounter{mountOneErr: ErrMountAmbiguous}
	m, _ := attachVolumeRunningInstance(t, nil, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMountAmbiguous)
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne,
		"AttachVolume must call UnmountOne to clean up the ambiguous backend mount")
}

// TestAttachVolume_GenericMountError_DoesNotUnmount confirms that other
// MountOne failures (NATS timeout, backend explicit Error, marshal errors)
// do NOT trigger UnmountOne — only the empty-URI ambiguous case rolls back.
func TestAttachVolume_GenericMountError_DoesNotUnmount(t *testing.T) {
	mounter := &fakeVolumeMounter{mountOneErr: errors.New("ebs.mount NATS request: timeout")}
	m, _ := attachVolumeRunningInstance(t, nil, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrMountAmbiguous)
	assert.Empty(t, mounter.unmountedOne,
		"non-ambiguous MountOne errors must not trigger UnmountOne")
}

// TestAttachVolume_NBDURIParseFailure_TriggersUnmountOne covers the rollback
// after a successful mount that returns a malformed NBDURI. AttachVolume must
// unmount the backend before bailing.
func TestAttachVolume_NBDURIParseFailure_TriggersUnmountOne(t *testing.T) {
	mounter := &fakeVolumeMounter{mountOneURI: "not-a-valid-nbd-uri"}
	m, _ := attachVolumeRunningInstance(t, nil, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse NBDURI")
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne)
}

// TestAttachVolume_ObjectAddFailure_TriggersUnmountOne covers QMP iothread
// object-add failure: AttachVolume must unmount and bail before issuing
// blockdev-add (no further QMP traffic past the failed object-add).
func TestAttachVolume_ObjectAddFailure_TriggersUnmountOne(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		if cmd.Execute == "query-block" {
			return map[string]any{"return": []qmp.BlockDevice{}}
		}
		if cmd.Execute == "object-add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "iothread limit"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "object-add")
	assert.Equal(t, []string{"object-add"}, recorder.executes(),
		"object-add failure must short-circuit before blockdev-add")
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne)
}

// TestAttachVolume_BlockdevAddFailure_TriggersUnmountOne covers QMP
// blockdev-add failure: AttachVolume must unmount and bail before issuing
// device_add.
func TestAttachVolume_BlockdevAddFailure_TriggersUnmountOne(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		if cmd.Execute == "query-block" {
			return map[string]any{"return": []qmp.BlockDevice{}}
		}
		if cmd.Execute == "blockdev-add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "nbd connect failed"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blockdev-add")
	assert.Equal(t, []string{"object-add", "blockdev-add", "object-del"}, recorder.executes())
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne)
}

// TestAttachVolume_DeviceAddFailure_BlockdevDelOK_Unmounts covers the
// device_add-fail-then-rollback path where blockdev-del succeeds. The
// rollback chain must run blockdev-del then UnmountOne in order.
func TestAttachVolume_DeviceAddFailure_BlockdevDelOK_Unmounts(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		if cmd.Execute == "query-block" {
			return map[string]any{"return": []qmp.BlockDevice{}}
		}
		if cmd.Execute == "device_add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "no PCI slot"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_add")
	assert.Equal(t, []string{"object-add", "blockdev-add", "device_add", "blockdev-del", "object-del"}, recorder.executes())
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne,
		"successful blockdev-del rollback must be followed by UnmountOne")
}

// TestAttachVolume_DeviceAddFailure_BlockdevDelFails_SkipsUnmountOne
// preserves the load-bearing invariant in the rollback chain: when
// blockdev-del fails, AttachVolume must NOT call UnmountOne. Tearing down
// the NBD server while QEMU still holds the block node would crash the VM.
// A future refactor that "fixes" the apparent bug by always unmounting
// would crash production VMs — this test is the regression guard.
func TestAttachVolume_DeviceAddFailure_BlockdevDelFails_SkipsUnmountOne(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		switch cmd.Execute {
		case "query-block":
			return map[string]any{"return": []qmp.BlockDevice{}}
		case "device_add":
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "no PCI slot"}}
		case "blockdev-del":
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "node still in use"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_add")
	assert.Equal(t, []string{"object-add", "blockdev-add", "device_add", "blockdev-del"}, recorder.executes())
	assert.Empty(t, mounter.unmountedOne,
		"failed blockdev-del must skip UnmountOne — unmounting under a live block node crashes QEMU")
}

// captureSlog redirects slog.Default to a text handler over the returned
// buffer for the duration of t. The "succeeded after retry" log line is the
// only operator-visible signal that blockdev-del needed to wait, so retry
// tests must be able to assert on its presence/absence.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestAttachVolume_PersistsAPIDeviceNameInVolumeMetadata locks down the
// AWS-spec contract that Volume.Attachments[].Device carries the API-form
// name (/dev/sd[f-p]), not the in-guest virtio path (/dev/vd*). The
// Terraform AWS provider's aws_volume_attachment polls DescribeVolumes
// with attachment.device=/dev/sdf — storing the guest path makes that
// filter reject every attached volume and the wait loop times out with
// "couldn't find resource".
//
// The QMP responder returns a query-block payload where vdisk-vol-1
// maps to /dev/vdc (the third virtio slot after os + cloudinit), so the
// test can distinguish the API name from the guest name in every assertion.
// BlockDeviceMappings still carries the guest device name per mulga-599;
// only the volume-metadata path uses the API name.
func TestAttachVolume_PersistsAPIDeviceNameInVolumeMetadata(t *testing.T) {
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		if cmd.Execute == "query-block" {
			return map[string]any{
				"return": []qmp.BlockDevice{
					{
						Device:   "os",
						Inserted: &qmp.BlockInserted{},
						QDev:     "/machine/peripheral-anon/device[0]/virtio-backend",
					},
					{
						Device:   "cloudinit",
						Inserted: &qmp.BlockInserted{},
						QDev:     "/machine/peripheral-anon/device[1]/virtio-backend",
					},
					{
						Device:   "",
						Inserted: &qmp.BlockInserted{},
						QDev:     "/machine/peripheral/vdisk-vol-1/hotplug-ebs1/virtio-backend",
					},
				},
			}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	stateUpdater := &fakeVolumeStateUpdater{}

	m := NewManagerWithDeps(Deps{
		VolumeMounter:      mounter,
		VolumeStateUpdater: stateUpdater,
	})
	m.Insert(&VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
	})

	device, err := m.AttachVolume(t.Context(), "i-1", "vol-1", "/dev/sdf")
	require.NoError(t, err)

	assert.Equal(t, "/dev/sdf", device,
		"AttachVolume must return the API-form name so the daemon's AttachVolume response and aws_volume_attachment's post-attach wait both round-trip the AWS-spec contract")

	calls := stateUpdater.snapshot()
	require.Len(t, calls, 1,
		"AttachVolume must persist exactly one volume-metadata update")
	assert.Equal(t, "vol-1", calls[0].VolumeID)
	assert.Equal(t, "in-use", calls[0].State)
	assert.Equal(t, "i-1", calls[0].InstanceID)
	assert.Equal(t, "/dev/sdf", calls[0].AttachmentDevice,
		"UpdateVolumeState must persist the API-form name (/dev/sdf), not the in-guest path (/dev/vdc) — the attachment.device filter on DescribeVolumes only matches the API form")

	v, ok := m.Get("i-1")
	require.True(t, ok)
	require.Len(t, v.Instance.BlockDeviceMappings, 1,
		"AttachVolume must append exactly one BlockDeviceMapping")
	bdm := v.Instance.BlockDeviceMappings[0]
	require.NotNil(t, bdm.DeviceName)
	assert.Equal(t, "/dev/vdc", *bdm.DeviceName,
		"BlockDeviceMappings[].DeviceName must carry the guest virtio path so `lsblk` inside the VM matches DescribeInstances (mulga-599 / PR #55)")
	require.NotNil(t, bdm.Ebs)
	require.NotNil(t, bdm.Ebs.VolumeId)
	assert.Equal(t, "vol-1", *bdm.Ebs.VolumeId)
}

// TestTryBlockdevDel covers the bounded retry loop that handles QEMU's
// transient "node is in use" GenericError after device_del. Each sub-test
// configures a QMP responder that fails the first N attempts with the
// chosen error class, then succeeds (or keeps failing) so the loop can
// exercise the success-on-retry, non-retryable, and exhaustion branches.
//
// DetachDelay is zero throughout so the 20-attempt exhaustion case stays
// well under a second; the production 1s polling delay is not under test.
// Sub-tests run sequentially because captureSlog mutates the process-wide
// slog.Default — parallel sub-tests would race on the shared logger.
func TestTryBlockdevDel(t *testing.T) {
	const inUseDesc = "Node 'nbd-vol-1' is in use"

	t.Run("success on first attempt logs no retry message", func(t *testing.T) {
		buf := captureSlog(t)

		var attempts atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			attempts.Add(1)
			return nil
		})
		defer cancel()

		m := NewManagerWithDeps(Deps{})
		err := m.tryBlockdevDel(t.Context(), &VM{ID: "i-1", QMPClient: qmpClient}, "nbd-vol-1")
		require.NoError(t, err)
		assert.Equal(t, int32(1), attempts.Load())
		assert.NotContains(t, buf.String(), "succeeded after retry",
			"first-attempt success must not log the retry-success line")
	})

	t.Run("success on third attempt logs succeeded after retry", func(t *testing.T) {
		buf := captureSlog(t)

		var attempts atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			n := attempts.Add(1)
			if n < 3 {
				return map[string]any{"error": map[string]any{"class": "GenericError", "desc": inUseDesc}}
			}
			return nil
		})
		defer cancel()

		m := NewManagerWithDeps(Deps{})
		err := m.tryBlockdevDel(t.Context(), &VM{ID: "i-1", QMPClient: qmpClient}, "nbd-vol-1")
		require.NoError(t, err)
		assert.Equal(t, int32(3), attempts.Load())
		logs := buf.String()
		assert.Contains(t, logs, "succeeded after retry")
		assert.Contains(t, logs, "attempts=3")
	})

	t.Run("non-in-use error on second attempt returns immediately", func(t *testing.T) {
		var attempts atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			n := attempts.Add(1)
			switch n {
			case 1:
				return map[string]any{"error": map[string]any{"class": "GenericError", "desc": inUseDesc}}
			default:
				return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "permission denied"}}
			}
		})
		defer cancel()

		m := NewManagerWithDeps(Deps{})
		err := m.tryBlockdevDel(t.Context(), &VM{ID: "i-1", QMPClient: qmpClient}, "nbd-vol-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
		assert.Equal(t, int32(2), attempts.Load(),
			"non-retryable error must stop the loop on the attempt that produced it")
	})

	t.Run("node-not-found returns nil so a detach retry resumes to the seal", func(t *testing.T) {
		var attempts atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			attempts.Add(1)
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "Failed to find node with node-name='nbd-vol-1'"}}
		})
		defer cancel()

		m := NewManagerWithDeps(Deps{})
		err := m.tryBlockdevDel(t.Context(), &VM{ID: "i-1", QMPClient: qmpClient}, "nbd-vol-1")
		require.NoError(t, err,
			"an already-removed block node must be idempotent so a detach retry can re-drive the unmount seal")
		assert.Equal(t, int32(1), attempts.Load(),
			"node-not-found must not be retried")
	})

	t.Run("twenty in-use errors return the last error", func(t *testing.T) {
		var attempts atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			attempts.Add(1)
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": inUseDesc}}
		})
		defer cancel()

		m := NewManagerWithDeps(Deps{})
		err := m.tryBlockdevDel(t.Context(), &VM{ID: "i-1", QMPClient: qmpClient}, "nbd-vol-1")
		require.Error(t, err)
		assert.True(t, isQMPNodeInUse(err),
			"exhaustion must return the last 'in use' error, not a wrapped or sentinel value")
		assert.Equal(t, int32(blockdevDelMaxAttempts), attempts.Load(),
			"retry must cap at blockdevDelMaxAttempts attempts")
	})
}
