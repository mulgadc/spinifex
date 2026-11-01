package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// A reboot is a clean guest shutdown followed by a fresh QEMU process on the
// same volumes, taps and identity, the way nova's soft reboot and Proxmox's
// qm reboot do it. Resetting the paused process in place instead left some
// guests silent in firmware, while a new process on the same disks always boots.

// rebootPowerdownTimeout is how long the guest gets to shut itself down before
// its QEMU is ended without asking. Nova's shutdown_timeout, and the same shape
// as the hard reboot AWS documents once a guest will not go quietly.
var rebootPowerdownTimeout = 60 * time.Second

// rebootBootProgressTimeout is how long a relaunched guest has to write its
// first console byte. Firmware prints within seconds even on a nested host, so
// silence past this is a hang, not a slow boot.
var rebootBootProgressTimeout = 60 * time.Second

// rebootBootProgressPoll is how often the console log is sampled while waiting.
var rebootBootProgressPoll = time.Second

// bootDiagSampleGap separates the two register samples in a hang report, so a
// frozen RIP can be told from a vCPU spinning in a loop.
var bootDiagSampleGap = 2 * time.Second

const (
	// relaunchQuitTimeout bounds the quit sent to the process being replaced.
	relaunchQuitTimeout = 5 * time.Second
	// relaunchExitTimeout is how long a quit QEMU gets to exit before SIGKILL.
	// QEMU flushes its drives on quit, so this is not cut short.
	relaunchExitTimeout = 20 * time.Second
	// relaunchKillTimeout bounds the wait after SIGKILL.
	relaunchKillTimeout = 10 * time.Second
	// bootDiagCommandTimeout bounds each QMP query in a hang report.
	bootDiagCommandTimeout = 5 * time.Second
)

// startRelaunchedQEMU starts the replacement process. A var so tests can stand
// in for QEMU.
var startRelaunchedQEMU = func(m *Manager, instance *VM) error {
	return m.startQEMU(instance, true)
}

// errRelaunchFailed marks a reboot that ended the old QEMU and could not start
// its replacement. The guest is down, so crash recovery takes over.
var errRelaunchFailed = errors.New("relaunch after reboot failed")

// Reboot admits a reboot and runs it in the background, the way EC2 does: the
// answer says the request was accepted, not that the guest is back. Asking a
// guest to shut down takes as long as the guest takes, which is far longer than
// any caller waits, so only the two admission failures can be reported at all.
func (m *Manager) Reboot(ctx context.Context, id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}
	if status := m.Status(instance); status != StateRunning {
		return fmt.Errorf("%w: cannot reboot instance %s in state %s",
			ErrInvalidTransition, id, status)
	}

	// Claiming the flag tells the heartbeat to leave the guest alone while it
	// is down, and serialises reboots, so a repeated request joins the one in
	// flight instead of racing it.
	if !instance.rebooting.CompareAndSwap(false, true) {
		slog.InfoContext(ctx, "Reboot already in flight, ignoring the repeat", "instanceId", id)
		return nil
	}

	// Detached rather than derived: the caller is answered immediately, so its
	// context is cancelled long before the guest has finished shutting down.
	rebootCtx := context.WithoutCancel(ctx)
	m.goroutineWg.Go(func() {
		err := m.rebootNow(rebootCtx, instance)
		instance.rebooting.Store(false)
		if err == nil {
			return
		}
		// A stop or terminate that lands mid-reboot owns the guest now.
		if errors.Is(err, ErrInvalidTransition) {
			slog.InfoContext(rebootCtx, "Reboot gave way to a lifecycle change", "instanceId", id, "reason", err)
			return
		}
		slog.ErrorContext(rebootCtx, "Reboot failed", "instanceId", id, "err", err)
		// The old process is gone and no new one runs, which is a crash in
		// all but name: recovery relaunches it under the restart policy.
		if errors.Is(err, errRelaunchFailed) {
			m.HandleCrash(instance, err)
		}
	})
	return nil
}

// rebootNow shuts the guest down, replaces its QEMU, and watches the new one
// reach the console. The instance stays in StateRunning throughout.
func (m *Manager) rebootNow(ctx context.Context, instance *VM) error {
	// Ending QEMU on a running guest syncs nothing and every dirty page is
	// lost. Ask first; force it only once asking has failed.
	if err := m.gracefulPowerdown(ctx, instance, rebootPowerdownTimeout); err != nil {
		slog.WarnContext(ctx, "Guest did not power down for reboot, forcing it off",
			"instanceId", instance.ID, "budget_ms", otelsetup.Millis(rebootPowerdownTimeout), "err", err)
	}

	baseline, err := m.relaunchGuest(ctx, instance)
	if err != nil {
		return err
	}
	if m.bootProgressed(ctx, instance, baseline) {
		slog.InfoContext(ctx, "Guest rebooted into a fresh QEMU", "instanceId", instance.ID)
		return nil
	}
	m.captureBootDiagnostics(ctx, instance, "no console output from the relaunched guest")

	// Silence is only evidence for a guest that has printed before. One that
	// never writes to its serial console is left to boot in its own time.
	if baseline == 0 {
		slog.WarnContext(ctx, "Relaunched guest has written nothing to its console, and never has",
			"instanceId", instance.ID)
		return nil
	}

	slog.WarnContext(ctx, "Relaunched guest is silent, replacing its QEMU once more", "instanceId", instance.ID)
	baseline, err = m.relaunchGuest(ctx, instance)
	if err != nil {
		return err
	}
	if m.bootProgressed(ctx, instance, baseline) {
		slog.InfoContext(ctx, "Guest rebooted on the second fresh QEMU", "instanceId", instance.ID)
		return nil
	}
	m.captureBootDiagnostics(ctx, instance, "no console output after a second relaunch")
	return fmt.Errorf("guest %s wrote nothing to its console within %s of two fresh launches",
		instance.ID, rebootBootProgressTimeout)
}

// relaunchGuest ends the guest's QEMU and starts a fresh one on the same
// mounted volumes, rebuilding taps and the command line as a start does. It
// returns the console log size before the new process ran.
func (m *Manager) relaunchGuest(ctx context.Context, instance *VM) (int64, error) {
	// Hot-plug and stop both act on the process, so neither may land on one
	// that is being replaced.
	instance.attachMu.Lock()
	defer instance.attachMu.Unlock()
	instance.relaunchMu.Lock()
	defer instance.relaunchMu.Unlock()

	if status := m.Status(instance); status != StateRunning {
		return 0, fmt.Errorf("%w: reboot of %s interrupted in state %s", ErrInvalidTransition, instance.ID, status)
	}
	if err := m.endQEMUForRelaunch(ctx, instance); err != nil {
		return 0, fmt.Errorf("end QEMU for reboot: %w", err)
	}
	// A stop that arrived while the old process was ending owns the instance
	// now and finds no process to stop, which is what it wanted.
	if status := m.Status(instance); status != StateRunning {
		return 0, fmt.Errorf("%w: reboot of %s interrupted in state %s", ErrInvalidTransition, instance.ID, status)
	}

	baseline := consoleLogSize(instance)
	if err := startRelaunchedQEMU(m, instance); err != nil {
		return 0, fmt.Errorf("%w: start QEMU: %w", errRelaunchFailed, err)
	}
	fresh, err := newQMPClientWithHandshake(ctx, instance)
	if err != nil {
		if pid, pidErr := hostprocess.ReadPidFile(instance.ID); pidErr == nil && pid > 0 {
			_ = hostprocess.ForceKillProcess(pid, relaunchKillTimeout)
		}
		return 0, fmt.Errorf("%w: connect QMP: %w", errRelaunchFailed, err)
	}
	m.adoptQMPConnection(instance, fresh)

	if err := m.writeRunningState(); err != nil {
		slog.WarnContext(ctx, "Failed to persist state after reboot relaunch", "instanceId", instance.ID, "err", err)
	}
	slog.InfoContext(ctx, "Reboot relaunched QEMU", append([]any{"instanceId", instance.ID,
		"pid", instance.qemuPID.Load()}, CurrentHostVirtStack().logAttrs()...)...)
	return baseline, nil
}

// endQEMUForRelaunch ends the guest's process and waits until it has exited,
// so its NBD connections are closed before the replacement opens them.
func (m *Manager) endQEMUForRelaunch(ctx context.Context, instance *VM) error {
	pid, _ := hostprocess.ReadPidFile(instance.ID)
	// Disown it first: its exit is the reboot's doing, not a crash.
	instance.qemuPID.Store(0)

	if q := instance.QMPClient; q != nil && q.Conn != nil {
		if _, err := sendQMPCommandWithTimeout(ctx, q, qmp.QMPCommand{Execute: "quit"},
			instance.ID, relaunchQuitTimeout); err != nil {
			// QEMU may close the socket before its reply; the exit is the answer.
			slog.DebugContext(ctx, "QMP quit for reboot", "instanceId", instance.ID, "err", err)
		}
	}
	if pid > 0 && hostprocess.WaitForProcessExit(pid, relaunchExitTimeout) != nil {
		slog.WarnContext(ctx, "QEMU did not exit after quit, killing it for reboot",
			"instanceId", instance.ID, "pid", pid)
		if err := hostprocess.ForceKillProcess(pid, relaunchKillTimeout); err != nil {
			return fmt.Errorf("kill QEMU pid %d: %w", pid, err)
		}
	}

	if q := instance.QMPClient; q != nil {
		q.Mu.Lock()
		if q.Conn != nil {
			_ = q.Conn.Close()
		}
		q.Dead = true
		q.Mu.Unlock()
	}
	return nil
}

// adoptQMPConnection moves a fresh monitor connection into the instance's
// existing client, so the heartbeat and every holder of the pointer carry on
// against the new process.
func (m *Manager) adoptQMPConnection(instance *VM, fresh *qmp.QMPClient) {
	q := instance.QMPClient
	if q == nil {
		instance.QMPClient = fresh
		go m.qmpHeartbeat(instance)
		return
	}
	q.Mu.Lock()
	defer q.Mu.Unlock()
	if q.Conn != nil {
		_ = q.Conn.Close()
	}
	q.Conn, q.Decoder, q.Encoder, q.Path = fresh.Conn, fresh.Decoder, fresh.Encoder, fresh.Path
	q.Dead = false
}

// consoleLogSize is the console log's length, or zero when there is none.
func consoleLogSize(instance *VM) int64 {
	if instance.Config.ConsoleLogPath == "" {
		return 0
	}
	info, err := os.Stat(instance.Config.ConsoleLogPath)
	if err != nil {
		return 0
	}
	return info.Size()
}

// bootProgressed waits for the console log to grow past baseline. It also
// reports true once the instance leaves running, because whoever moved it owns
// it now and there is nothing left to judge. A guest with no log passes.
func (m *Manager) bootProgressed(ctx context.Context, instance *VM, baseline int64) bool {
	if instance.Config.ConsoleLogPath == "" {
		return true
	}
	deadline := time.Now().Add(rebootBootProgressTimeout)
	for {
		if consoleLogSize(instance) > baseline || m.Status(instance) != StateRunning {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		timer := time.NewTimer(rebootBootProgressPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
	}
}

// captureBootDiagnostics logs what QEMU and the host say about a guest that
// has gone silent, before anything is done about it: run state, vCPUs, and
// every vCPU's registers sampled twice.
func (m *Manager) captureBootDiagnostics(ctx context.Context, instance *VM, reason string) {
	query := func(cmd qmp.QMPCommand) string {
		resp, err := sendQMPCommandWithTimeout(ctx, instance.QMPClient, cmd, instance.ID, bootDiagCommandTimeout)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(resp.Return)
	}
	registers := qmp.QMPCommand{Execute: "human-monitor-command",
		Arguments: map[string]any{"command-line": "info registers -a"}}

	pid := instance.qemuPID.Load()
	if pid == 0 {
		// A process adopted across a daemon restart has no waiter, only a pid file.
		if p, err := hostprocess.ReadPidFile(instance.ID); err == nil {
			pid = int64(p)
		}
	}
	attrs := []any{"instanceId", instance.ID, "reason", reason, "pid", pid,
		"console_bytes", consoleLogSize(instance)}
	attrs = append(attrs, "query_status", query(qmp.QMPCommand{Execute: "query-status"}))
	cpus := query(qmp.QMPCommand{Execute: "query-cpus-fast"})
	attrs = append(attrs, "query_cpus_fast", cpus, "vcpu_threads", vcpuThreadStates(pid, cpus))
	attrs = append(attrs, "registers_first", query(registers))

	timer := time.NewTimer(bootDiagSampleGap)
	select {
	case <-ctx.Done():
		timer.Stop()
	case <-timer.C:
	}
	attrs = append(attrs, "registers_second", query(registers), "vcpu_threads_second", vcpuThreadStates(pid, cpus))
	attrs = append(attrs, CurrentHostVirtStack().logAttrs()...)
	slog.ErrorContext(ctx, "Guest is silent after reboot; QEMU state follows", attrs...)
}

// vcpuThreadStates reads each vCPU thread's scheduler state and CPU time from
// /proc, which tells a thread spinning in the guest from one asleep in KVM_RUN.
func vcpuThreadStates(pid int64, queryCPUsFast string) string {
	if pid <= 0 {
		return ""
	}
	var cpus []qmp.CPUInfoFast
	if err := json.Unmarshal([]byte(queryCPUsFast), &cpus); err != nil {
		return ""
	}
	parts := make([]string, 0, len(cpus))
	for _, cpu := range cpus {
		task := fmt.Sprintf("/proc/%d/task/%d", pid, cpu.ThreadID)
		stat, err := os.ReadFile(task + "/stat")
		if err != nil {
			continue
		}
		wchan, _ := os.ReadFile(task + "/wchan")
		parts = append(parts, fmt.Sprintf("cpu%d tid=%d %s wchan=%s",
			cpu.CPUIndex, cpu.ThreadID, threadStatFields(string(stat)), strings.TrimSpace(string(wchan))))
	}
	return strings.Join(parts, "; ")
}

// threadStatFields picks state, utime and stime out of a /proc stat line. The
// command name may hold spaces, so fields are counted from its closing paren.
func threadStatFields(stat string) string {
	_, rest, ok := strings.CutLast(stat, ")")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	// After the paren: state is field 3 of the line, utime 14, stime 15.
	if len(fields) < 13 {
		return ""
	}
	return fmt.Sprintf("state=%s utime=%s stime=%s", fields[0], fields[11], fields[12])
}
