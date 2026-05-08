package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestBuildBaseVMConfig(t *testing.T) {
	tests := []struct {
		name         string
		instanceID   string
		pidFile      string
		consolePath  string
		serialSocket string
		architecture string
		vCPUs        int
		memoryMiB    int
	}{
		{
			name:         "x86_64 instance",
			instanceID:   "i-abc123",
			pidFile:      "/tmp/qemu-i-abc123.pid",
			consolePath:  "/run/console-i-abc123.log",
			serialSocket: "/run/serial-i-abc123.sock",
			architecture: "x86_64",
			vCPUs:        4,
			memoryMiB:    8192,
		},
		{
			name:         "arm64 instance",
			instanceID:   "i-def456",
			pidFile:      "/tmp/qemu-i-def456.pid",
			consolePath:  "/run/console-i-def456.log",
			serialSocket: "/run/serial-i-def456.sock",
			architecture: "arm64",
			vCPUs:        2,
			memoryMiB:    4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildBaseVMConfig(tt.instanceID, tt.pidFile, tt.consolePath, tt.serialSocket, tt.architecture, tt.vCPUs, tt.memoryMiB)

			assert.Equal(t, tt.instanceID, cfg.Name)
			assert.Equal(t, tt.pidFile, cfg.PIDFile)
			assert.Equal(t, tt.consolePath, cfg.ConsoleLogPath)
			assert.Equal(t, tt.serialSocket, cfg.SerialSocket)
			assert.Equal(t, tt.architecture, cfg.Architecture)
			assert.Equal(t, tt.vCPUs, cfg.CPUCount)
			assert.Equal(t, tt.memoryMiB, cfg.Memory)
			assert.True(t, cfg.EnableKVM)
			assert.True(t, cfg.NoGraphic)
			assert.Equal(t, "q35", cfg.MachineType)
			assert.Equal(t, "host", cfg.CPUType)

			require.Len(t, cfg.Devices, 11)
			for i, dev := range cfg.Devices {
				expected := fmt.Sprintf("pcie-root-port,id=hotplug%d,chassis=%d,slot=0", i+1, i+1)
				assert.Equal(t, expected, dev.Value)
			}
		})
	}
}

func TestBuildDrives(t *testing.T) {
	tests := []struct {
		name          string
		requests      []types.EBSRequest
		cpuCount      int
		wantDrives    int
		wantIOThreads int
		wantDevices   int
		wantErr       string
	}{
		{
			name: "boot volume",
			requests: []types.EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
			},
			cpuCount:      4,
			wantDrives:    1,
			wantIOThreads: 1,
			wantDevices:   1,
		},
		{
			name: "cloud-init volume",
			requests: []types.EBSRequest{
				{Name: "vol-ci", NBDURI: "nbd:unix:/tmp/ci.sock", CloudInit: true},
			},
			cpuCount:      2,
			wantDrives:    1,
			wantIOThreads: 0,
			wantDevices:   0,
		},
		{
			name: "EFI volume skipped",
			requests: []types.EBSRequest{
				{Name: "vol-efi", EFI: true},
			},
			cpuCount:      2,
			wantDrives:    0,
			wantIOThreads: 0,
			wantDevices:   0,
		},
		{
			name: "missing NBDURI returns error",
			requests: []types.EBSRequest{
				{Name: "vol-bad"},
			},
			cpuCount: 2,
			wantErr:  "NBDURI not set for volume vol-bad",
		},
		{
			name: "mixed boot + cloud-init + EFI",
			requests: []types.EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
				{Name: "vol-ci", NBDURI: "nbd:unix:/tmp/ci.sock", CloudInit: true},
				{Name: "vol-efi", EFI: true},
			},
			cpuCount:      4,
			wantDrives:    2,
			wantIOThreads: 1,
			wantDevices:   1,
		},
		{
			name:          "empty requests",
			requests:      []types.EBSRequest{},
			cpuCount:      2,
			wantDrives:    0,
			wantIOThreads: 0,
			wantDevices:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drives, iothreads, devices, err := buildDrives(tt.requests, tt.cpuCount)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Len(t, drives, tt.wantDrives)
			assert.Len(t, iothreads, tt.wantIOThreads)
			assert.Len(t, devices, tt.wantDevices)
		})
	}
}

func TestBuildDrives_BootVolume(t *testing.T) {
	requests := []types.EBSRequest{
		{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
	}

	drives, iothreads, devices, err := buildDrives(requests, 4)
	require.NoError(t, err)

	require.Len(t, drives, 1)
	d := drives[0]
	assert.Equal(t, "nbd:unix:/tmp/boot.sock", d.File)
	assert.Equal(t, "raw", d.Format)
	assert.Equal(t, "none", d.If)
	assert.Equal(t, "disk", d.Media)
	assert.Equal(t, "os", d.ID)
	assert.Equal(t, "none", d.Cache)

	require.Len(t, iothreads, 1)
	assert.Equal(t, "ioth-os", iothreads[0].ID)

	require.Len(t, devices, 1)
	assert.Equal(t, "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=4,bootindex=1", devices[0].Value)
}

func TestBuildDrives_CloudInitVolume(t *testing.T) {
	requests := []types.EBSRequest{
		{Name: "vol-ci", NBDURI: "nbd:unix:/tmp/ci.sock", CloudInit: true},
	}

	drives, _, _, err := buildDrives(requests, 2)
	require.NoError(t, err)

	require.Len(t, drives, 1)
	d := drives[0]
	assert.Equal(t, "nbd:unix:/tmp/ci.sock", d.File)
	assert.Equal(t, "raw", d.Format)
	assert.Equal(t, "virtio", d.If)
	assert.Equal(t, "cdrom", d.Media)
	assert.Equal(t, "cloudinit", d.ID)
}

func TestTapDeviceName(t *testing.T) {
	tests := []struct {
		name string
		eni  string
		want string
	}{
		{"short ENI", "eni-abc123", "tapabc123"},
		{"prefix-only", "abc123", "tapabc123"},
		{"truncate to 15", "eni-abc123def456789", "tapabc123def456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TapDeviceName(tt.eni))
		})
	}
}

func TestGenerateDevMAC_Stable(t *testing.T) {
	a := GenerateDevMAC("i-abc123")
	b := GenerateDevMAC("i-abc123")
	assert.Equal(t, a, b, "MAC must be deterministic for the same instance ID")
	assert.NotEqual(t, GenerateDevMAC("i-abc123"), GenerateDevMAC("i-def456"))
}

func TestStartReturnsErrorWhenInstanceUnknown(t *testing.T) {
	m := NewManager()
	err := m.Start("i-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "i-missing")
}

// launchTestManager wires the minimum deps needed to drive Manager.launch
// far enough to exercise the early-exit paths and the VolumeMounter call
// site. Returns the manager, the mounter (for assertions and onMount
// hooks), and a counter of OnInstanceUp invocations.
//
// The success-path "OnInstanceUp fires exactly once" assertion is not
// driven from a vm-package unit test — the launch happy path requires a
// real QEMU process plus a working QMP socket (startQEMU + AttachQMP),
// and the heartbeat goroutine spawned by AttachQMP blocks on a 30s sleep
// with no cancellation seam, so a hermetic test would either leak the
// heartbeat or stall for 30s. The "exactly once" contract is covered by
// the daemon-side e2e tests that drive the full RunInstances pipeline.
// The counter is retained so the negative direction (no fire on aborted
// or failed launches) can be asserted alongside the existing tests.
func launchTestManager(t *testing.T) (*Manager, *fakeVolumeMounter, *int) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{}
	upCalls := 0
	m := NewManager()
	m.SetDeps(Deps{
		VolumeMounter: mounter,
		Hooks: ManagerHooks{
			OnInstanceUp: func(*VM) error { upCalls++; return nil },
		},
	})
	return m, mounter, &upCalls
}

// TestRun_LaunchStillValid_FirstCheck covers the first launchStillValid
// race-check (lifecycle.go:77): when a concurrent terminate has flipped
// status out of {Pending, Stopped, Provisioning} before Run starts, Run
// must return nil immediately without mounting volumes or firing the
// OnInstanceUp hook — the terminate goroutine owns cleanup.
func TestRun_LaunchStillValid_FirstCheck(t *testing.T) {
	for _, status := range []InstanceState{
		StateRunning, StateStopping, StateShuttingDown, StateTerminated, StateError,
	} {
		t.Run(string(status), func(t *testing.T) {
			m, mounter, upCalls := launchTestManager(t)
			instance := &VM{ID: "i-" + string(status), Status: status}
			m.Insert(instance)

			err := m.Run(instance)

			require.NoError(t, err)
			assert.Empty(t, mounter.mounted, "Mount must not be called when launch is aborted by first race check")
			assert.Equal(t, 0, *upCalls, "OnInstanceUp must not fire when launch is aborted")
			assert.Equal(t, status, m.Status(instance), "status must not change")
		})
	}
}

// TestRun_LaunchStillValid_SecondCheck covers the second launchStillValid
// race-check (lifecycle.go:102): a terminate races in during Mount (which
// can take 30+s on cold AMIs) and flips status to ShuttingDown. Run must
// return nil after Mount completes, without proceeding to startQEMU or
// firing OnInstanceUp.
func TestRun_LaunchStillValid_SecondCheck(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	instance := &VM{ID: "i-flip", Status: StatePending}
	m.Insert(instance)

	mounter.onMount = func(v *VM) {
		m.UpdateState(v.ID, func(vv *VM) { vv.Status = StateShuttingDown })
	}

	err := m.Run(instance)

	require.NoError(t, err, "Run must return nil when concurrent terminate flips status during Mount")
	assert.Equal(t, []string{"i-flip"}, mounter.mounted, "Mount must run exactly once before the second race check")
	assert.Equal(t, 0, *upCalls, "OnInstanceUp must not fire when the second race check aborts launch")
	assert.Equal(t, StateShuttingDown, m.Status(instance))
}

// TestRun_VolumeMounterError_Propagates covers the Mount-failure branch
// (lifecycle.go:93): the error must bubble up unchanged, no startQEMU, no
// hook fires, and the manager state is unaffected.
func TestRun_VolumeMounterError_Propagates(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	sentinel := errors.New("mount failed")
	mounter.mountErr = sentinel

	instance := &VM{ID: "i-mount-fail", Status: StatePending}
	m.Insert(instance)

	err := m.Run(instance)

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, []string{"i-mount-fail"}, mounter.mounted)
	assert.Equal(t, 0, *upCalls)
	assert.Equal(t, StatePending, m.Status(instance), "status must be unchanged on Mount failure")
}

// TestRun_AlreadyRunningPID_ReturnsError covers the live-PID guard
// (lifecycle.go:81-91): when the PID file refers to a live process, Run
// must return an error before touching volumes or firing hooks.
func TestRun_AlreadyRunningPID_ReturnsError(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)

	instance := &VM{ID: "i-live-pid", Status: StatePending}
	m.Insert(instance)

	pidDir := os.Getenv("XDG_RUNTIME_DIR")
	pidFile := filepath.Join(pidDir, instance.ID+".pid")
	require.NoError(t, os.WriteFile(pidFile, fmt.Appendf(nil, "%d", os.Getpid()), 0o600))

	err := m.Run(instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already running")
	assert.Empty(t, mounter.mounted, "Mount must not run when an existing live PID is detected")
	assert.Equal(t, 0, *upCalls)
}

// TestStart_DispatchesThroughLaunch asserts Start (the found-instance
// branch) actually drives launch — not merely returns nil. A regression
// that made Start a no-op after the lookup would still pass
// TestStartReturnsErrorWhenInstanceUnknown; this test catches it by
// requiring the launch-internal Mount call.
func TestStart_DispatchesThroughLaunch(t *testing.T) {
	m, mounter, _ := launchTestManager(t)
	sentinel := errors.New("mount failed via Start")
	mounter.mountErr = sentinel

	instance := &VM{ID: "i-start-dispatch", Status: StateStopped}
	m.Insert(instance)

	err := m.Start(instance.ID)

	require.ErrorIs(t, err, sentinel, "Start must dispatch through launch and surface Mount errors")
	assert.Equal(t, []string{"i-start-dispatch"}, mounter.mounted)
}

// TestStart_AbortedByConcurrentTerminate is the Start-side mirror of the
// first launchStillValid check: a status set outside the launchable
// allowlist must abort the launch with no error and no Mount call.
func TestStart_AbortedByConcurrentTerminate(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	instance := &VM{ID: "i-start-abort", Status: StateShuttingDown}
	m.Insert(instance)

	err := m.Start(instance.ID)

	require.NoError(t, err)
	assert.Empty(t, mounter.mounted)
	assert.Equal(t, 0, *upCalls)
}

// TestLaunchStillValid_Allowlist locks in the launchStillValid allowlist
// across every defined InstanceState plus the zero value. The parent
// plan flagged: "a regression in the allowlist would silently break the
// start-stopped path" — e.g. dropping StateStopped would make
// StartInstances on a stopped VM no-op without surfacing an error.
func TestLaunchStillValid_Allowlist(t *testing.T) {
	tests := []struct {
		status InstanceState
		want   bool
		why    string
	}{
		{StateProvisioning, true, "RunInstances entry: launches before status flip to Pending"},
		{StatePending, true, "RunInstances/restore entry: launch must proceed"},
		{StateStopped, true, "StartInstances re-launches a stopped VM through the same pipeline"},
		{StateRunning, false, "already past launch — second Run is a no-op"},
		{StateStopping, false, "stop in flight: stopCleanup owns teardown"},
		{StateShuttingDown, false, "terminate in flight: terminateCleanup owns teardown"},
		{StateTerminated, false, "terminal"},
		{StateError, false, "RestartCrashedInstance drives transitions, not launch"},
		{InstanceState(""), false, "zero value must reject"},
	}

	m := NewManager()
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			instance := &VM{ID: "i-" + string(tt.status), Status: tt.status}
			got := m.launchStillValid(instance)
			assert.Equal(t, tt.want, got, tt.why)
		})
	}
}

// startBrokenQMPListener accepts on a unix socket and immediately closes
// every connection without sending a greeting, so qmp.NewQMPClient's
// greeting decode fails. Returns the socket path and a stop function the
// caller must invoke before goleak.VerifyNone so the accept goroutine
// drains.
func startBrokenQMPListener(t *testing.T) (string, func()) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = ln.Close()
		<-done
	}
	t.Cleanup(stop)
	return sockPath, stop
}

// TestAttachQMP_DialFailure_NoHeartbeatLeak covers the regression the
// parent plan flagged: a swap that started qmpHeartbeat before checking
// the factory error would leak one goroutine per failed reconnect. With
// no listener at the configured socket, net.Dial fails inside
// newQMPClientWithHandshake and AttachQMP must return without spawning
// the heartbeat or mutating instance.QMPClient.
func TestAttachQMP_DialFailure_NoHeartbeatLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	m := NewManager()
	instance := &VM{
		ID:     "i-dial-fail",
		Config: Config{QMPSocket: filepath.Join(t.TempDir(), "missing.sock")},
	}

	err := m.AttachQMP(instance)

	require.Error(t, err)
	assert.Nil(t, instance.QMPClient, "QMPClient must remain unset on dial failure")
}

// TestAttachQMP_HandshakeFailure_NoHeartbeatLeak covers the path where
// the unix socket exists but qmp.NewQMPClient's greeting decode fails
// (server closed). The dial succeeded so the connection must be closed
// before AttachQMP returns, the heartbeat must not start, and
// instance.QMPClient must remain nil.
func TestAttachQMP_HandshakeFailure_NoHeartbeatLeak(t *testing.T) {
	sockPath, stopListener := startBrokenQMPListener(t)

	m := NewManager()
	instance := &VM{ID: "i-handshake-fail", Config: Config{QMPSocket: sockPath}}

	err := m.AttachQMP(instance)

	require.Error(t, err)
	assert.Nil(t, instance.QMPClient, "QMPClient must remain unset on handshake failure")

	stopListener()
	goleak.VerifyNone(t)
}
