package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQEMU stands in for successive QEMU processes of one guest. Each start is
// a new generation: a real child process behind the pid file, and a monitor on
// the instance's QMP socket. quit ends that generation the way QEMU does.
type fakeQEMU struct {
	t          *testing.T
	instanceID string
	socket     string

	mu       sync.Mutex
	gen      int
	commands []string // "<gen>:<execute>"
	ln       net.Listener
	conns    []net.Conn
	child    *exec.Cmd
	// poweredOff is per generation: whether the guest acknowledged the button.
	poweredOff bool

	// ignorePowerButton keeps the guest running through system_powerdown.
	ignorePowerButton bool
	// onCommand runs for each command before the reply, for a test to act
	// mid-reboot.
	onCommand func(gen int, cmd qmp.QMPCommand)
}

func newFakeQEMU(t *testing.T, instanceID string) *fakeQEMU {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	hostprocess.ShortenProcessExitPollForTest(t, 5*time.Millisecond)
	f := &fakeQEMU{t: t, instanceID: instanceID, socket: filepath.Join(dir, "qmp.sock")}
	t.Cleanup(f.stop)
	return f
}

// start brings up the next generation.
func (f *fakeQEMU) start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		return err
	}
	// Reaped as soon as it dies, so a liveness probe does not see a zombie.
	go func() { _ = child.Wait() }()
	if err := hostprocess.WritePidFile(f.instanceID, child.Process.Pid); err != nil {
		return err
	}
	_ = os.Remove(f.socket)
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		return err
	}
	f.gen++
	f.ln, f.child, f.poweredOff = ln, child, false
	gen := f.gen
	go f.accept(ln, gen)
	return nil
}

func (f *fakeQEMU) accept(ln net.Listener, gen int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		go f.serve(conn, gen)
	}
}

func (f *fakeQEMU) serve(conn net.Conn, gen int) {
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(map[string]any{"QMP": map[string]any{"capabilities": []string{}}}); err != nil {
		return
	}
	for {
		var cmd qmp.QMPCommand
		if err := dec.Decode(&cmd); err != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, fmt.Sprintf("%d:%s", gen, cmd.Execute))
		hook := f.onCommand
		f.mu.Unlock()
		if hook != nil {
			hook(gen, cmd)
		}

		reply := map[string]any{"return": map[string]any{}}
		f.mu.Lock()
		switch cmd.Execute {
		case "system_powerdown":
			if !f.ignorePowerButton {
				f.poweredOff = true
			}
		case "query-status":
			if f.poweredOff {
				reply = qmpStatusResponse(qmpStatusShutdown, false)
			} else {
				reply = qmpStatusResponse("running", true)
			}
		case "query-cpus-fast":
			reply = map[string]any{"return": []map[string]any{{"cpu-index": 0, "thread-id": 1}}}
		case "human-monitor-command":
			reply = map[string]any{"return": "RIP=000000000000fff0"}
		}
		f.mu.Unlock()
		if err := enc.Encode(reply); err != nil {
			return
		}
		if cmd.Execute == "quit" {
			f.exit(gen)
			return
		}
	}
}

// exit ends generation gen: the process dies, its pid file and socket go.
func (f *fakeQEMU) exit(gen int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if gen != f.gen || f.child == nil {
		return
	}
	_ = f.child.Process.Kill()
	_ = hostprocess.WaitForProcessExit(f.child.Process.Pid, 5*time.Second)
	_ = hostprocess.RemovePidFile(f.instanceID)
	_ = f.ln.Close()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns, f.child = nil, nil
}

func (f *fakeQEMU) stop() {
	f.mu.Lock()
	gen := f.gen
	f.mu.Unlock()
	f.exit(gen)
}

func (f *fakeQEMU) generation() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

func (f *fakeQEMU) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commands)
}

// rebootRig is a running guest on a fakeQEMU, with the reboot's timings cut
// down and the relaunch seam pointed at the fake. writes says, per relaunch,
// whether the new guest prints to its console.
type rebootRig struct {
	m       *Manager
	vm      *VM
	qemu    *fakeQEMU
	console string
	starts  int
}

func newRebootRig(t *testing.T, deps Deps, consoleSoFar string, writes ...bool) *rebootRig {
	t.Helper()
	setFast := func(p *time.Duration, d time.Duration) {
		prev := *p
		*p = d
		t.Cleanup(func() { *p = prev })
	}
	setFast(&rebootPowerdownTimeout, 200*time.Millisecond)
	setFast(&powerdownPollInterval, 5*time.Millisecond)
	setFast(&rebootBootProgressTimeout, 150*time.Millisecond)
	setFast(&rebootBootProgressPoll, 5*time.Millisecond)
	setFast(&bootDiagSampleGap, time.Millisecond)

	r := &rebootRig{qemu: newFakeQEMU(t, "i-1")}
	r.console = filepath.Join(filepath.Dir(r.qemu.socket), "console.log")
	require.NoError(t, os.WriteFile(r.console, []byte(consoleSoFar), 0o600))
	require.NoError(t, r.qemu.start())

	r.vm = &VM{ID: "i-1", Status: StateRunning,
		Config: Config{QMPSocket: r.qemu.socket, ConsoleLogPath: r.console}}
	client, err := newQMPClientWithHandshake(t.Context(), r.vm)
	require.NoError(t, err)
	r.vm.QMPClient = client

	previous := startRelaunchedQEMU
	startRelaunchedQEMU = func(*Manager, *VM) error {
		r.starts++
		if err := r.qemu.start(); err != nil {
			return err
		}
		if r.starts <= len(writes) && writes[r.starts-1] {
			fh, err := os.OpenFile(r.console, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer fh.Close()
			_, err = fmt.Fprintf(fh, "BdsDxe: boot %d\n", r.starts)
			return err
		}
		return nil
	}
	t.Cleanup(func() { startRelaunchedQEMU = previous })

	r.m = NewManagerWithDeps(deps)
	r.m.Insert(r.vm)
	return r
}

func (r *rebootRig) reboot(t *testing.T) {
	t.Helper()
	require.NoError(t, r.m.Reboot(t.Context(), "i-1"))
	r.m.WaitForBackgroundWork()
}

// TestReboot_ReplacesQEMUAfterACleanShutdown is the whole reboot: the guest is
// asked down, its QEMU quit once it has, and a fresh one started. Nothing is
// reset in place, nothing the instance owns moves, and Describe sees running.
func TestReboot_ReplacesQEMUAfterACleanShutdown(t *testing.T) {
	var upCalls, downCalls int
	mounter := &fakeVolumeMounter{}
	r := newRebootRig(t, Deps{
		VolumeMounter: mounter,
		Hooks: ManagerHooks{
			OnInstanceUp:   func(*VM) error { upCalls++; return nil },
			OnInstanceDown: func(string) { downCalls++ },
		},
	}, "first boot\n", true)
	client := r.vm.QMPClient

	r.reboot(t)

	assert.Equal(t, 2, r.qemu.generation(), "the reboot must run on a fresh QEMU")
	log := r.qemu.log()
	assert.NotContains(t, log, "1:system_reset", "a reboot must not reset the old process in place")
	assert.Less(t, slices.Index(log, "1:system_powerdown"), slices.Index(log, "1:quit"),
		"the guest is asked down before its QEMU is ended")
	assert.Equal(t, StateRunning, r.m.Status(r.vm))
	assert.Zero(t, upCalls+downCalls, "a reboot is not a stop and start; no hooks fire")
	assert.Empty(t, mounter.mounted, "volumes stay mounted across the relaunch")
	assert.Empty(t, mounter.unmounted, "volumes stay mounted across the relaunch")

	// The same client now talks to the new process, so the heartbeat and every
	// other holder of it carry on.
	assert.Same(t, client, r.vm.QMPClient)
	_, err := queryQMPStatus(t.Context(), r.vm, time.Second)
	require.NoError(t, err)
	assert.Contains(t, r.qemu.log(), "2:query-status")

	console, err := os.ReadFile(r.console)
	require.NoError(t, err)
	assert.Equal(t, "first boot\nBdsDxe: boot 1\n", string(console),
		"the console log carries on across the reboot")
}

// TestReboot_WedgedGuestIsForcedOffAndRelaunched is the AWS contract: a guest
// that will not shut down cleanly is rebooted anyway once its budget is spent.
func TestReboot_WedgedGuestIsForcedOffAndRelaunched(t *testing.T) {
	r := newRebootRig(t, Deps{}, "x", true)
	r.qemu.ignorePowerButton = true
	start := time.Now()

	r.reboot(t)

	assert.GreaterOrEqual(t, time.Since(start), rebootPowerdownTimeout,
		"the guest is given its whole budget before it is forced off")
	assert.Equal(t, 2, r.qemu.generation())
	assert.Contains(t, r.qemu.log(), "1:quit")
	assert.Equal(t, StateRunning, r.m.Status(r.vm))
}

// TestReboot_SilentGuestIsDiagnosedThenRelaunchedOnce covers the hang this
// design exists for. A relaunched guest that has printed before and now prints
// nothing gets its QEMU state captured first, then one more fresh process.
func TestReboot_SilentGuestIsDiagnosedThenRelaunchedOnce(t *testing.T) {
	r := newRebootRig(t, Deps{}, "first boot\n", false, true)

	r.reboot(t)

	require.Equal(t, 3, r.qemu.generation(), "a silent relaunch is replaced exactly once")
	log := r.qemu.log()
	for _, cmd := range []string{"2:query-status", "2:query-cpus-fast", "2:human-monitor-command", "2:quit"} {
		assert.Contains(t, log, cmd)
	}
	assert.Less(t, slices.Index(log, "2:human-monitor-command"), slices.Index(log, "2:quit"),
		"the silent process is captured before it is ended")
	assert.Equal(t, StateRunning, r.m.Status(r.vm))
}

// TestReboot_GuestThatNeverPrintedIsNotRelaunchedAgain: silence from a guest
// that never writes to its serial console is not evidence of a hang.
func TestReboot_GuestThatNeverPrintedIsNotRelaunchedAgain(t *testing.T) {
	r := newRebootRig(t, Deps{}, "", false)

	r.reboot(t)

	assert.Equal(t, 2, r.qemu.generation())
	assert.Contains(t, r.qemu.log(), "2:human-monitor-command", "the silence is still recorded")
	assert.NotContains(t, r.qemu.log(), "2:quit")
}

// TestReboot_FailedRelaunchGoesToCrashRecovery: once the old process is gone,
// a replacement that will not start is a down guest, and recovery owns it.
func TestReboot_FailedRelaunchGoesToCrashRecovery(t *testing.T) {
	r := newRebootRig(t, Deps{}, "x")
	startRelaunchedQEMU = func(*Manager, *VM) error { return errors.New("no qemu") }

	r.reboot(t)

	var crashes int
	r.m.Inspect(r.vm, func(v *VM) { crashes = v.Health.CrashCount })
	assert.Equal(t, 1, crashes, "the failed relaunch is handed to crash recovery")
}

// TestReboot_StopWhileOldQEMUEndsSkipsTheRelaunch: a stop that lands while the
// old process is ending owns the instance, so no new process is started.
func TestReboot_StopWhileOldQEMUEndsSkipsTheRelaunch(t *testing.T) {
	r := newRebootRig(t, Deps{}, "x", true)
	r.qemu.onCommand = func(_ int, cmd qmp.QMPCommand) {
		if cmd.Execute == "quit" {
			r.m.UpdateState("i-1", func(v *VM) { v.Status = StateStopping })
		}
	}

	r.reboot(t)

	assert.Equal(t, 1, r.qemu.generation(), "no QEMU is started for an instance being stopped")
}

// TestReboot_StopWaitsForTheRelaunchBeforeUnmounting is the data-safety
// invariant: a stop never seals volumes under a QEMU the reboot is starting.
func TestReboot_StopWaitsForTheRelaunchBeforeUnmounting(t *testing.T) {
	mounter := &fakeVolumeMounter{}
	r := newRebootRig(t, Deps{VolumeMounter: mounter}, "x", true)

	stopDone := make(chan struct{})
	inner := startRelaunchedQEMU
	startRelaunchedQEMU = func(m *Manager, instance *VM) error {
		go func() {
			defer close(stopDone)
			_ = m.shutdownAndUnmount(instance)
		}()
		select {
		case <-stopDone:
			t.Error("the stop finished while the relaunch still owned the process")
		case <-time.After(100 * time.Millisecond):
		}
		mounter.mu.Lock()
		assert.Empty(t, mounter.unmounted, "volumes unmounted under a QEMU being started")
		mounter.mu.Unlock()
		return inner(m, instance)
	}

	r.reboot(t)
	<-stopDone
	assert.Equal(t, []string{"i-1"}, mounter.unmounted, "the stop runs once the relaunch is done")
	assert.Contains(t, r.qemu.log(), "2:quit", "the stop ends the process the reboot started")
}

// TestReboot_RepeatJoinsTheOneInFlight asserts two reboots of one guest do not
// race each other through the shutdown and relaunch.
func TestReboot_RepeatJoinsTheOneInFlight(t *testing.T) {
	r := newRebootRig(t, Deps{}, "x", true, true)
	release := make(chan struct{})
	r.qemu.onCommand = func(gen int, cmd qmp.QMPCommand) {
		if gen == 1 && cmd.Execute == "system_powerdown" {
			<-release
		}
	}

	require.NoError(t, r.m.Reboot(t.Context(), "i-1"))
	require.NoError(t, r.m.Reboot(t.Context(), "i-1"))
	close(release)
	r.m.WaitForBackgroundWork()

	assert.Equal(t, 2, r.qemu.generation(), "the repeat must join the reboot in flight")
}

// TestReboot_AnswersBeforeTheGuestIsBack is the contract EC2 documents: the
// reply says the request was accepted, long before the guest is down.
func TestReboot_AnswersBeforeTheGuestIsBack(t *testing.T) {
	r := newRebootRig(t, Deps{}, "x", true)
	release := make(chan struct{})
	r.qemu.onCommand = func(gen int, cmd qmp.QMPCommand) {
		if gen == 1 && cmd.Execute == "system_powerdown" {
			<-release
		}
	}

	done := make(chan error, 1)
	go func() { done <- r.m.Reboot(t.Context(), "i-1") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Error("Reboot blocked on the guest shutting down; callers time out well inside that")
	}
	close(release)
	r.m.WaitForBackgroundWork()
}

func TestReboot_InstanceNotFound(t *testing.T) {
	err := NewManager().Reboot(t.Context(), "i-missing")
	assert.ErrorIs(t, err, ErrInstanceNotFound)
}

func TestReboot_NotRunning(t *testing.T) {
	m := NewManager()
	m.Insert(&VM{ID: "i-1", Status: StateStopped})
	assert.ErrorIs(t, m.Reboot(t.Context(), "i-1"), ErrInvalidTransition)
}

// TestQMPHeartbeatPoll_LeavesARebootingGuestAlone: a reboot powers the guest
// off and replaces its process, so the heartbeat must neither quit it nor
// count the missing socket against it.
func TestQMPHeartbeatPoll_LeavesARebootingGuestAlone(t *testing.T) {
	recorder := &qmpRecorder{}
	qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		recorder.record(cmd)
		return qmpStatusResponse(qmpStatusShutdown, false)
	})
	defer cancel()

	m := NewManager()
	instance := &VM{ID: "i-1", Status: StateRunning, QMPClient: qmpClient}
	instance.rebooting.Store(true)
	m.Insert(instance)

	assert.True(t, m.qmpHeartbeatPoll(instance))
	assert.Empty(t, recorder.executes(), "a rebooting guest must not be quit")
	assert.Zero(t, instance.Health.QMPConsecutiveFailures, "a reboot in flight is not a failed heartbeat")
}

func TestThreadStatFields(t *testing.T) {
	// The comm field holds a space and a paren, which a plain split misreads.
	stat := "4242 (CPU 0/K)VM) R 1 2 3 4 5 6 7 8 9 10 777 888 0 0"
	assert.Equal(t, "state=R utime=777 stime=888", threadStatFields(stat))
	assert.Empty(t, threadStatFields("garbage"))
}

func TestExecute_ConsoleLogAppendOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    Config
		want   string
		absent string
	}{
		{"socket fresh", Config{SerialSocket: "/s", ConsoleLogPath: "/c"}, "logfile=/c", "logappend"},
		{"socket reboot", Config{SerialSocket: "/s", ConsoleLogPath: "/c", ConsoleLogAppend: true}, "logfile=/c,logappend=on", ""},
		{"file reboot", Config{MachineType: "microvm", ConsoleLogPath: "/c", ConsoleLogAppend: true}, "file,id=console0,path=/c,append=on", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.MachineType, cfg.SerialSocket = tc.cfg.MachineType, tc.cfg.SerialSocket
			cfg.ConsoleLogPath, cfg.ConsoleLogAppend = tc.cfg.ConsoleLogPath, tc.cfg.ConsoleLogAppend
			cmd, err := cfg.Execute()
			require.NoError(t, err)
			var chardev string
			for i, a := range cmd.Args {
				if a == "-chardev" && i+1 < len(cmd.Args) {
					chardev = cmd.Args[i+1]
				}
			}
			assert.Contains(t, chardev, tc.want)
			if tc.absent != "" {
				assert.NotContains(t, chardev, tc.absent)
			}
		})
	}
}
