package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
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

func TestAttachVolume_InstanceNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.AttachVolume("i-missing", "vol-1", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestAttachVolume_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped, Instance: &ec2.Instance{}})

	_, err := m.AttachVolume("i-1", "vol-1", "")
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

	_, err := m.AttachVolume("i-1", "vol-1", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAttachmentLimitExceeded)
}

func TestDetachVolume_InstanceNotFound(t *testing.T) {
	m := NewManager()
	_, err := m.DetachVolume("i-missing", "vol-1", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestDetachVolume_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped})

	_, err := m.DetachVolume("i-1", "vol-1", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestDetachVolume_VolumeNotAttached(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateRunning, Instance: &ec2.Instance{}})

	_, err := m.DetachVolume("i-1", "vol-missing", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeNotAttached)
}

func TestDetachVolume_BootVolumeRejected(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{
		ID:       "i-1",
		Status:   StateRunning,
		Instance: &ec2.Instance{},
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-boot", Boot: true}},
		},
	})

	_, err := m.DetachVolume("i-1", "vol-boot", "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeNotDetachable)
}

func TestDetachVolume_DeviceMismatch(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{
		ID:       "i-1",
		Status:   StateRunning,
		Instance: &ec2.Instance{},
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	_, err := m.DetachVolume("i-1", "vol-1", "/dev/sdg", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeDeviceMismatch)
}

func TestReboot_InstanceNotFound(t *testing.T) {
	m := NewManager()
	err := m.Reboot("i-missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestReboot_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped})

	err := m.Reboot("i-1")
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

	require.NoError(t, m.Reboot("i-1"))

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

	require.NoError(t, m.Reboot("i-1"))

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

	err := m.Reboot("i-1")
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

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
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

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
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

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
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
		if cmd.Execute == "object-add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "iothread limit"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
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
		if cmd.Execute == "blockdev-add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "nbd connect failed"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blockdev-add")
	assert.Equal(t, []string{"object-add", "blockdev-add"}, recorder.executes())
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne)
}

// TestAttachVolume_DeviceAddFailure_BlockdevDelOK_Unmounts covers the
// device_add-fail-then-rollback path where blockdev-del succeeds. The
// rollback chain must run blockdev-del then UnmountOne in order.
func TestAttachVolume_DeviceAddFailure_BlockdevDelOK_Unmounts(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		if cmd.Execute == "device_add" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "no PCI slot"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{mountOneURI: "nbd:unix:/tmp/test.sock"}
	m, _ := attachVolumeRunningInstance(t, qmpClient, mounter)

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_add")
	assert.Equal(t, []string{"object-add", "blockdev-add", "device_add", "blockdev-del"}, recorder.executes())
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

	_, err := m.AttachVolume("i-1", "vol-1", "/dev/sdf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device_add")
	assert.Equal(t, []string{"object-add", "blockdev-add", "device_add", "blockdev-del"}, recorder.executes())
	assert.Empty(t, mounter.unmountedOne,
		"failed blockdev-del must skip UnmountOne — unmounting under a live block node crashes QEMU")
}
