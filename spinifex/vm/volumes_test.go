package vm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

// mockQMPServer lets a responder push asynchronous QMP events (e.g.
// DEVICE_DELETED) onto the same stream as command replies. Writes are
// serialized so a delayed event push never interleaves with a reply.
type mockQMPServer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (s *mockQMPServer) pushEvent(ev map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(ev)
}

// newMockQMPClientWithEvents is newMockQMPClient plus a *mockQMPServer handle
// so the responder can schedule out-of-band events (typically from a
// goroutine) in addition to its synchronous command reply.
//
// This uses a real unix socket rather than net.Pipe: net.Pipe's deadline
// implementation does not reliably support "timeout, then clear the deadline,
// then read again" on the same conn (verified against Go's net.Pipe — a real
// unix socket, matching production QMP transport, does not have this
// limitation), and DEVICE_DELETED-wait tests need exactly that sequence.
//
// The listener keeps accepting connections for the life of the test (each
// served on its own goroutine, greeting first) so a client-side reconnect —
// e.g. reconnectQMP redialing after a genuine decode error marks the client
// Dead — is served exactly like the first connection.
func newMockQMPClientWithEvents(t *testing.T, responder func(qmp.QMPCommand, *mockQMPServer) map[string]any) (*qmp.QMPClient, func()) {
	t.Helper()

	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	var wg sync.WaitGroup
	serveConn := func(conn net.Conn) {
		defer wg.Done()
		defer conn.Close()

		srv := &mockQMPServer{enc: json.NewEncoder(conn)}
		if err := srv.pushEvent(map[string]any{
			"QMP": map[string]any{
				"version":      map[string]any{"qemu": map[string]any{"major": 8, "minor": 0}},
				"capabilities": []string{},
			},
		}); err != nil {
			return
		}

		dec := json.NewDecoder(conn)
		for {
			var cmd qmp.QMPCommand
			if err := dec.Decode(&cmd); err != nil {
				return
			}
			if cmd.Execute == "qmp_capabilities" {
				if err := srv.pushEvent(map[string]any{"return": map[string]any{}}); err != nil {
					return
				}
				continue
			}
			var reply map[string]any
			if responder != nil {
				reply = responder(cmd, srv)
			}
			if reply == nil {
				reply = map[string]any{"return": map[string]any{}}
			}
			if err := srv.pushEvent(reply); err != nil {
				return
			}
		}
	}

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go serveConn(conn)
		}
	}()

	clientConn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)

	dec := json.NewDecoder(clientConn)
	var greeting qmp.QMPGreeting
	require.NoError(t, dec.Decode(&greeting), "mock QMP server greeting")

	client := &qmp.QMPClient{
		Conn:    clientConn,
		Decoder: dec,
		Encoder: json.NewEncoder(clientConn),
		Path:    sockPath,
	}

	cancel := func() {
		_ = ln.Close()
		// client.Conn may have been swapped by reconnectQMP after a Dead
		// redial, so close the current field value, not the initial dial.
		_ = client.Conn.Close()
		<-acceptDone
		wg.Wait()
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

// newMockQMPServerConn wires a QMPClient over net.Pipe with no command-reply
// loop at all — the test drives the server side directly via the returned
// json.Encoder, which is what waitForDeviceDeletedEvent tests need: an event
// pushed independently of any command/response exchange.
func newMockQMPServerConn(t *testing.T) (*qmp.QMPClient, *json.Encoder, func()) {
	t.Helper()
	client, enc, _, cancel := newMockQMPServerConnRaw(t)
	return client, enc, cancel
}

// newMockQMPServerConnRaw is newMockQMPServerConn plus the raw server-side
// net.Conn, needed by tests that close the server side independently (e.g. to
// force a genuine EOF decode error rather than a benign read-deadline
// timeout).
func newMockQMPServerConnRaw(t *testing.T) (*qmp.QMPClient, *json.Encoder, net.Conn, func()) {
	t.Helper()

	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	clientConn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)

	serverConn := <-accepted
	_ = ln.Close()
	require.NotNil(t, serverConn, "mock QMP server failed to accept")

	client := &qmp.QMPClient{
		Conn:    clientConn,
		Decoder: json.NewDecoder(clientConn),
		Encoder: json.NewEncoder(clientConn),
		Path:    sockPath,
	}
	enc := json.NewEncoder(serverConn)

	cancel := func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	}
	return client, enc, serverConn, cancel
}

// TestWaitForDeviceDeletedEvent covers waitForDeviceDeletedEvent directly:
// matching event, non-matching event followed by a match, and timeout. Uses
// a short timeout so the timeout sub-test stays fast.
func TestWaitForDeviceDeletedEvent(t *testing.T) {
	t.Run("matching event returns nil", func(t *testing.T) {
		qmpClient, enc, cancel := newMockQMPServerConn(t)
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- waitForDeviceDeletedEvent(t.Context(), qmpClient, "vdisk-vol-1", 2*time.Second, "i-1")
		}()

		require.NoError(t, enc.Encode(map[string]any{
			"event": "DEVICE_DELETED",
			"data":  map[string]any{"device": "vdisk-vol-1", "path": "/machine/peripheral/vdisk-vol-1"},
		}))
		require.NoError(t, <-errCh)
	})

	t.Run("non-matching event is skipped, later match returns nil", func(t *testing.T) {
		qmpClient, enc, cancel := newMockQMPServerConn(t)
		defer cancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- waitForDeviceDeletedEvent(t.Context(), qmpClient, "vdisk-vol-2", 2*time.Second, "i-1")
		}()

		require.NoError(t, enc.Encode(map[string]any{
			"event": "DEVICE_DELETED",
			"data":  map[string]any{"device": "vdisk-vol-1", "path": "/machine/peripheral/vdisk-vol-1"},
		}))
		require.NoError(t, enc.Encode(map[string]any{
			"event": "DEVICE_DELETED",
			"data":  map[string]any{"device": "vdisk-vol-2", "path": "/machine/peripheral/vdisk-vol-2"},
		}))
		require.NoError(t, <-errCh)
	})

	t.Run("no event before timeout returns error but leaves the client alive", func(t *testing.T) {
		qmpClient, _, cancel := newMockQMPServerConn(t)
		defer cancel()

		err := waitForDeviceDeletedEvent(t.Context(), qmpClient, "vdisk-vol-1", 50*time.Millisecond, "i-1")
		require.Error(t, err)
		// A read-deadline timeout fires at a message boundary — nothing was
		// partially consumed — and the deferred SetReadDeadline reset restores
		// the connection, so this is benign: DEVICE_DELETED is frequently
		// pre-swallowed by device_del's own reply loop, and poisoning the
		// connection on every such miss was the wedge regression.
		assert.False(t, qmpClient.Dead,
			"a benign read-deadline timeout must not mark the client dead")
	})

	t.Run("genuine decode error marks the client dead", func(t *testing.T) {
		qmpClient, _, serverConn, cancel := newMockQMPServerConnRaw(t)
		defer cancel()

		// Close only the server side: the client's next Decode fails with EOF,
		// not a deadline timeout, so the stream position is genuinely unknown.
		_ = serverConn.Close()

		err := waitForDeviceDeletedEvent(t.Context(), qmpClient, "vdisk-vol-1", 5*time.Second, "i-1")
		require.Error(t, err)
		assert.True(t, qmpClient.Dead,
			"a non-timeout decode error (EOF/connection close) must mark the client dead")
	})

	t.Run("nil client returns error", func(t *testing.T) {
		err := waitForDeviceDeletedEvent(t.Context(), nil, "vdisk-vol-1", time.Second, "i-1")
		require.Error(t, err)
	})
}

// TestDetachVolume_WaitsForDeviceDeletedBeforeBlockdevDel is the regression
// guard for the "node in use" wedge: blockdev-del only ever succeeds once
// DEVICE_DELETED has been observed, so if DetachVolume attempted it before
// waiting for the event, this test would exhaust blockdevDelMaxAttempts and
// fail exactly like the reported bug. Release of the event is gated on an
// explicit signal (not a race against wall-clock timing) so the test is
// deterministic; a minimum-elapsed-time assertion confirms DetachVolume
// actually blocked on the event rather than skipping the wait.
func TestDetachVolume_WaitsForDeviceDeletedBeforeBlockdevDel(t *testing.T) {
	var blockdevAttempts atomic.Int32
	releaseEvent := make(chan struct{})

	qmpClient, cancel := newMockQMPClientWithEvents(t, func(cmd qmp.QMPCommand, srv *mockQMPServer) map[string]any {
		switch cmd.Execute {
		case "device_del":
			go func() {
				<-releaseEvent
				_ = srv.pushEvent(map[string]any{
					"event": "DEVICE_DELETED",
					"data":  map[string]any{"device": "vdisk-vol-1", "path": "/machine/peripheral/vdisk-vol-1"},
				})
			}()
			return nil
		case "blockdev-del":
			blockdevAttempts.Add(1)
			return nil
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, DeviceDeletedTimeout: 5 * time.Second})
	m.Insert(&VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	const releaseDelay = 100 * time.Millisecond
	go func() {
		time.Sleep(releaseDelay)
		close(releaseEvent)
	}()

	start := time.Now()
	device, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, "/dev/sdf", device)
	assert.Equal(t, int32(1), blockdevAttempts.Load(),
		"waiting for DEVICE_DELETED before the first blockdev-del attempt must avoid any 'node in use' retry")
	assert.GreaterOrEqual(t, elapsed, releaseDelay/2,
		"DetachVolume must actually block on the DEVICE_DELETED event rather than proceeding immediately")
}

// TestDetachVolume_DeviceDeletedTimeout_FallsBackToRetry covers the case
// where DEVICE_DELETED is never observed within DeviceDeletedTimeout (e.g. a
// lost event, or a QEMU version quirk): DetachVolume must still fall back to
// the bounded blockdev-del retry and succeed once the node frees up, rather
// than surfacing the wait timeout as a hard failure.
func TestDetachVolume_DeviceDeletedTimeout_FallsBackToRetry(t *testing.T) {
	var blockdevAttempts atomic.Int32

	qmpClient, cancel := newMockQMPClientWithEvents(t, func(cmd qmp.QMPCommand, srv *mockQMPServer) map[string]any {
		if cmd.Execute == "blockdev-del" {
			n := blockdevAttempts.Add(1)
			if n < 2 {
				return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "Node 'nbd-vol-1' is in use"}}
			}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, DeviceDeletedTimeout: 30 * time.Millisecond})
	m.Insert(&VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	device, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdf", device)
	assert.Equal(t, int32(2), blockdevAttempts.Load(),
		"a missed DEVICE_DELETED event must still resolve via the bounded blockdev-del retry")
}

// TestDetachVolume_ConfirmedDeadQEMU_SkipsQMPAndSeals proves the
// detach-wedges-when-QEMU-dies fix: when QEMU is provably gone (PID file
// present, process dead), DetachVolume issues no QMP command — those steps
// would only hang on the wedged process — and proceeds straight to the
// ebs.unmount seal and the attachment clear.
func TestDetachVolume_ConfirmedDeadQEMU_SkipsQMPAndSeals(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var qmpCalled atomic.Bool
	qmpClient, cancel := newMockQMPClientWithEvents(t, func(cmd qmp.QMPCommand, srv *mockQMPServer) map[string]any {
		qmpCalled.Store(true)
		return nil
	})
	defer cancel()

	const id = "i-1"
	require.NoError(t, utils.WritePidFile(id, 999999)) // PID file present but process dead

	mounter := &fakeVolumeMounter{}
	updater := &fakeVolumeStateUpdater{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, VolumeStateUpdater: updater})
	m.Insert(&VM{
		ID:        id,
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	device, err := m.DetachVolume(t.Context(), id, "vol-1", "", false)
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdf", device)
	assert.False(t, qmpCalled.Load(), "a confirmed-dead QEMU must receive no QMP command")
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne, "the ebs.unmount seal must still run")
	calls := updater.snapshot()
	require.Len(t, calls, 1, "the stale attachment must be cleared exactly once")
	assert.Equal(t, "available", calls[0].State, "the volume must be returned to available")
}

// TestDetachVolume_BlockdevDelAlreadyRemoved_IdempotentSuccess covers the
// AWS-CLI-retry resume path end to end: a prior detach removed the block
// node, so blockdev-del returns "Failed to find node" on the second call.
// DetachVolume must treat that as success and complete the detach, not
// surface it as a hard failure.
func TestDetachVolume_BlockdevDelAlreadyRemoved_IdempotentSuccess(t *testing.T) {
	qmpClient, cancel := newMockQMPClientWithEvents(t, func(cmd qmp.QMPCommand, srv *mockQMPServer) map[string]any {
		if cmd.Execute == "blockdev-del" {
			return map[string]any{"error": map[string]any{"class": "GenericError", "desc": "Failed to find node with node-name='nbd-vol-1'"}}
		}
		return nil
	})
	defer cancel()

	mounter := &fakeVolumeMounter{}
	m := NewManagerWithDeps(Deps{VolumeMounter: mounter, DeviceDeletedTimeout: 30 * time.Millisecond})
	m.Insert(&VM{
		ID:        "i-1",
		Status:    StateRunning,
		Instance:  &ec2.Instance{},
		QMPClient: qmpClient,
		EBSRequests: types.EBSRequests{
			Requests: []types.EBSRequest{{Name: "vol-1", DeviceName: "/dev/sdf"}},
		},
	})

	device, err := m.DetachVolume(t.Context(), "i-1", "vol-1", "", false)
	require.NoError(t, err)
	assert.Equal(t, "/dev/sdf", device)
	assert.Equal(t, []string{"vol-1"}, mounter.unmountedOne,
		"an already-removed block node must resume through to the unmount seal")
}
