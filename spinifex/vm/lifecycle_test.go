package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestBuildBaseVMConfig pins the non-pass-through invariants buildBaseVMConfig
// must enforce: KVM + no-graphic, q35 + host CPU regardless of arch, and 11
// pre-allocated PCIe root ports for hot-plug (Linux's PCIe hot-plug requires
// pre-allocated ports — removing one silently breaks AttachVolume/AttachENI).
func TestBuildBaseVMConfig(t *testing.T) {
	for _, arch := range []string{"x86_64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			cfg := buildBaseVMConfig("i-x", "/tmp/x.pid", "/tmp/x.log", "/tmp/x.sock", arch, "", 2, 4096)

			assert.True(t, cfg.EnableKVM)
			assert.True(t, cfg.NoGraphic)
			assert.Equal(t, "q35", cfg.MachineType)
			assert.Equal(t, "host", cfg.CPUType)
			assert.False(t, cfg.UseUEFI, "empty bootMode must default to BIOS")

			require.Len(t, cfg.Devices, 11, "PCIe hot-plug requires 11 pre-allocated root ports")
			for i, dev := range cfg.Devices {
				expected := fmt.Sprintf("pcie-root-port,id=hotplug%d,chassis=%d,slot=0", i+1, i+1)
				assert.Equal(t, expected, dev.Value)
			}
		})
	}
}

// TestBuildBaseVMConfig_BootMode pins the bootMode → UseUEFI mapping.
// "uefi" and AWS's "uefi-preferred" both flip the firmware flag; "bios" and
// any unrecognised value (including "") fall through as BIOS.
func TestBuildBaseVMConfig_BootMode(t *testing.T) {
	tests := []struct {
		bootMode    string
		wantUseUEFI bool
	}{
		{"", false},
		{"bios", false},
		{"uefi", true},
		{"uefi-preferred", true},
		{"garbage", false},
	}
	for _, tc := range tests {
		t.Run(tc.bootMode, func(t *testing.T) {
			cfg := buildBaseVMConfig("i-x", "/tmp/x.pid", "/tmp/x.log", "/tmp/x.sock", "x86_64", tc.bootMode, 2, 4096)
			assert.Equal(t, tc.wantUseUEFI, cfg.UseUEFI)
		})
	}
}

func TestBuildDrives(t *testing.T) {
	tests := []struct {
		name          string
		requests      []types.EBSRequest
		cpuCount      int
		machineType   string
		wantDrives    []Drive
		wantIOThreads []IOThread
		wantDevices   []Device
		wantErr       string
	}{
		{
			name: "boot volume",
			requests: []types.EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
			},
			cpuCount: 4,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/boot.sock", Format: "raw", If: "none", Media: "disk", ID: "os", Cache: "none"},
			},
			wantIOThreads: []IOThread{{ID: "ioth-os"}},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=4,bootindex=1"},
			},
		},
		{
			name: "cloud-init volume",
			requests: []types.EBSRequest{
				{Name: "vol-ci", NBDURI: "nbd:unix:/tmp/ci.sock", CloudInit: true},
			},
			cpuCount: 2,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/ci.sock", Format: "raw", If: "virtio", Media: "cdrom", ID: "cloudinit"},
			},
		},
		{
			name: "EFI volume emits pflash unit=1",
			requests: []types.EBSRequest{
				{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true},
			},
			cpuCount: 2,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/efi.sock", Format: "raw", If: "pflash", Unit: 1},
			},
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
			name: "missing NBDURI on EFI returns error",
			requests: []types.EBSRequest{
				{Name: "vol-efi-bad", EFI: true},
			},
			cpuCount: 2,
			wantErr:  "NBDURI not set for volume vol-efi-bad",
		},
		{
			name: "mixed boot + cloud-init + EFI",
			requests: []types.EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
				{Name: "vol-ci", NBDURI: "nbd:unix:/tmp/ci.sock", CloudInit: true},
				{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true},
			},
			cpuCount: 4,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/boot.sock", Format: "raw", If: "none", Media: "disk", ID: "os", Cache: "none"},
				{File: "nbd:unix:/tmp/ci.sock", Format: "raw", If: "virtio", Media: "cdrom", ID: "cloudinit"},
				{File: "nbd:unix:/tmp/efi.sock", Format: "raw", If: "pflash", Unit: 1},
			},
			wantIOThreads: []IOThread{{ID: "ioth-os"}},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=4,bootindex=1"},
			},
		},
		{
			name:     "empty requests",
			requests: []types.EBSRequest{},
			cpuCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machineType := tt.machineType
			if machineType == "" {
				machineType = "q35"
			}
			drives, iothreads, devices, err := buildDrives(tt.requests, tt.cpuCount, machineType)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantDrives, drives)
			assert.Equal(t, tt.wantIOThreads, iothreads)
			assert.Equal(t, tt.wantDevices, devices)
		})
	}
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

// stubFindFreePort swaps the package-level findFreePort seam with a
// scripted result list. Exhausting the list returns a sentinel error so
// an under-counted test fails fast instead of falling back to the real
// OS allocator. t.Cleanup restores the original.
func stubFindFreePort(t *testing.T, results ...struct {
	addr string
	err  error
}) *int {
	t.Helper()
	orig := findFreePort
	idx := 0
	findFreePort = func() (string, error) {
		if idx >= len(results) {
			idx++
			return "", errors.New("stubFindFreePort: exhausted")
		}
		r := results[idx]
		idx++
		return r.addr, r.err
	}
	t.Cleanup(func() { findFreePort = orig })
	return &idx
}

func newDevNICInstance(id string, extra map[int]int) *VM {
	return &VM{ID: id, ExtraHostfwd: extra}
}

func TestAppendDevHostfwdNIC_SSHPortFails_NICSkipped(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "", err: errors.New("listen failed")})

	m := NewManager()
	m.SetDeps(Deps{BindHost: "10.0.0.1"})
	instance := newDevNICInstance("i-ssh-fail", nil)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls, "FindFreePort must be invoked exactly once before bailing")
	assert.Empty(t, instance.Config.NetDevs, "no NetDev appended when SSH port lookup fails")
	assert.Empty(t, instance.Config.Devices, "no virtio-net device appended when SSH port lookup fails")
}

func TestAppendDevHostfwdNIC_SSHAddrUnparsable_NICSkipped(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "nocolon", err: nil})

	m := NewManager()
	instance := newDevNICInstance("i-ssh-bad-addr", nil)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls)
	assert.Empty(t, instance.Config.NetDevs)
	assert.Empty(t, instance.Config.Devices)
}

func TestAppendDevHostfwdNIC_ExtraHostfwdNil_ShortCircuit(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "127.0.0.1:2222", err: nil})

	m := NewManager()
	m.SetDeps(Deps{BindHost: "0.0.0.0"})
	instance := newDevNICInstance("i-no-extra", nil)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls, "no extra entries means exactly one FindFreePort call")
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value)
	require.Len(t, instance.Config.Devices, 1)
	assert.Equal(t, fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s", GenerateDevMAC("i-no-extra")), instance.Config.Devices[0].Value)
}

func TestAppendDevHostfwdNIC_ExtraPortFails_WarningContinues(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "", err: errors.New("listen failed")},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-fail", map[int]int{8080: 0})

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 2, *calls)
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value, "failed extra entry must not appear in netdev value")
	assert.Equal(t, 0, instance.ExtraHostfwd[8080], "failed entry must leave the map value at its zero")
	require.Len(t, instance.Config.Devices, 1)
}

func TestAppendDevHostfwdNIC_ExtraAddrUnparsable_EntrySkipped(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "garbage-no-colon", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:9090", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-split", map[int]int{8080: 0, 8443: 0})

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 3, *calls)
	require.Len(t, instance.Config.NetDevs, 1)

	var goodGuest, badGuest int
	for g := range instance.ExtraHostfwd {
		if instance.ExtraHostfwd[g] == 9090 {
			goodGuest = g
		} else {
			badGuest = g
		}
	}
	require.NotZero(t, goodGuest, "exactly one entry must have been populated")
	assert.Equal(t, 0, instance.ExtraHostfwd[badGuest], "skipped entry must remain at zero")
	assert.Contains(t, instance.Config.NetDevs[0].Value, fmt.Sprintf("hostfwd=tcp:127.0.0.1:9090-:%d", goodGuest))
	assert.NotContains(t, instance.Config.NetDevs[0].Value, "garbage")
}

func TestAppendDevHostfwdNIC_ExtraPortAtoiFails_EntrySkipped(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:notanumber", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-atoi", map[int]int{8080: 0})

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 2, *calls)
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value, "non-numeric extra port must not be appended to netdev value")
	assert.Equal(t, 0, instance.ExtraHostfwd[8080], "skipped entry must remain at zero")
}

func TestAppendDevHostfwdNIC_ExtraHostfwd_HappyPath(t *testing.T) {
	stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:18080", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-ok", map[int]int{8080: 0})

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 18080, instance.ExtraHostfwd[8080])
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:18080-:8080", instance.Config.NetDevs[0].Value)
	require.Len(t, instance.Config.Devices, 1)
	assert.Equal(t, fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s", GenerateDevMAC("i-extra-ok")), instance.Config.Devices[0].Value)
}

// ---------------------------------------------------------------------------
// startQEMU — DirectBoot branch
//
// We call startQEMU directly (same package) and force early returns via tap
// errors so we cover the DirectBoot config-assignment lines and the tap setup
// paths without spawning a real QEMU process or sleeping 2 s.
// ---------------------------------------------------------------------------

// directBootManager wires the minimum Deps for startQEMU to reach the
// DirectBoot tap-setup block: InstanceTypes resolver + NetworkPlumber.
func directBootManager(t *testing.T, plumber NetworkPlumber) *Manager {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	resolver := fakeInstanceTypeResolver{
		"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512},
	}
	return NewManagerWithDeps(Deps{
		InstanceTypes:  resolver,
		NetworkPlumber: plumber,
	})
}

// TestStartQEMU_DirectBoot_PrimaryTapError verifies that when the
// NetworkPlumber fails to set up the primary ENI tap, startQEMU returns a
// wrapped error. This also drives the DirectBoot config-assignment block
// (PIDFile / ConsoleLogPath / SerialSocket) before the early return.
func TestStartQEMU_DirectBoot_PrimaryTapError(t *testing.T) {
	tapErr := errors.New("tap setup failed")
	plumber := &fakeNetworkPlumber{setupErr: tapErr}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-tap-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		ENIId:        "eni-000000000000aaaa",
		ENIMac:       "02:aa:bb:cc:dd:01",
	}

	err := m.startQEMU(instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "setup tap device")
	// PIDFile and ConsoleLogPath are set before the tap call.
	// SerialSocket is intentionally empty for direct-boot (file chardev used instead).
	assert.NotEmpty(t, instance.Config.PIDFile)
	assert.NotEmpty(t, instance.Config.ConsoleLogPath)
	assert.Empty(t, instance.Config.SerialSocket)
	// Exactly one SetupTap call (the primary ENI tap).
	require.Len(t, plumber.setupCalls, 1)
}

// TestStartQEMU_DirectBoot_ExtraENITapError verifies that a tap failure on a
// secondary ENI returns an error without starting QEMU.
func TestStartQEMU_DirectBoot_ExtraENITapError(t *testing.T) {
	callCount := 0
	plumber := &fakeNetworkPlumber{}
	// Override SetupTap to fail only on the second call (extra ENI).
	realSetupCalls := &plumber.setupCalls
	_ = realSetupCalls

	// Use a scripted plumber: success for primary, error for extra.
	scriptedPlumber := &scriptedNetworkPlumber{
		errs: []error{nil, errors.New("extra tap failed")},
	}
	m := directBootManager(t, scriptedPlumber)
	_ = callCount

	instance := &VM{
		ID:           "i-db-extra-tap-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		ENIId:        "eni-000000000000bbbb",
		ENIMac:       "02:aa:bb:cc:dd:02",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-000000000000cccc", ENIMac: "02:aa:bb:cc:dd:03"},
		},
	}

	err := m.startQEMU(instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "extra ENI")
	assert.Len(t, scriptedPlumber.calls, 2)
}

// TestStartQEMU_DirectBoot_MgmtTapError verifies that a tap failure on the
// management NIC returns a wrapped error.
func TestStartQEMU_DirectBoot_MgmtTapError(t *testing.T) {
	// No primary ENI → skips the ENI tap block; MgmtMAC set → enters mgmt block.
	plumber := &fakeNetworkPlumber{setupErr: errors.New("mgmt tap failed")}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-mgmt-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		MgmtMAC:      "02:aa:bb:cc:dd:ff",
	}

	err := m.startQEMU(instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "mgmt tap")
	require.Len(t, plumber.setupCalls, 1)
	assert.Equal(t, MgmtTapName("i-db-mgmt-err"), plumber.setupCalls[0].Name)
	assert.Equal(t, "br-mgmt", plumber.setupCalls[0].Bridge)
}

// TestStartQEMU_DirectBoot_NoENI_NoMgmt_InstanceTypeNotFound verifies that
// startQEMU returns an error when the instance type is absent from the resolver,
// exercising the early-return before any DirectBoot config is written.
func TestStartQEMU_DirectBoot_InstanceTypeNotFound(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-unknown-type",
		InstanceType: "x9.enormous",
		DirectBoot:   true,
	}

	err := m.startQEMU(instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "x9.enormous")
	assert.Empty(t, plumber.setupCalls)
}

// scriptedNetworkPlumber returns errors from a pre-loaded slice, one per call.
type scriptedNetworkPlumber struct {
	calls []TapSpec
	errs  []error
	idx   int
}

func (p *scriptedNetworkPlumber) SetupTap(spec TapSpec) error {
	p.calls = append(p.calls, spec)
	if p.idx < len(p.errs) {
		err := p.errs[p.idx]
		p.idx++
		return err
	}
	return nil
}

func (p *scriptedNetworkPlumber) CleanupTap(_ string) error { return nil }

var _ NetworkPlumber = (*scriptedNetworkPlumber)(nil)

// TestNbdkitPreExecWait locks in the per-mode pre-exec sleep budget: direct-boot
// is zero (no drives, no nbdkit) and the PC-machine path keeps the 2 s wait that
// the rest of the launch flow depends on.
func TestNbdkitPreExecWait(t *testing.T) {
	assert.Equal(t, time.Duration(0), nbdkitPreExecWait(true))
	assert.Equal(t, 2*time.Second, nbdkitPreExecWait(false))
}

// TestQemuStartSettleWait locks in the per-mode post-fork settle: direct-boot
// trims to 50 ms (QMP socket binds within a few ms when there is no firmware,
// ROM, or block layer setup) and the PC-machine path retains the historical
// 1 s buffer that catches bad-cmdline crashes before the QMP dial.
func TestQemuStartSettleWait(t *testing.T) {
	assert.Equal(t, 50*time.Millisecond, qemuStartSettleWait(true))
	assert.Equal(t, time.Second, qemuStartSettleWait(false))
}
