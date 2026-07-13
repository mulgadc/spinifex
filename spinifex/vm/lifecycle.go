package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const vmTracerName = "github.com/mulgadc/spinifex/spinifex/vm"

// noTraceKey marks a context whose QMP commands should not open spans —
// used by the heartbeat poller, which would otherwise root a trace per tick.
type noTraceKey struct{}

// endSpanWithError records err (if any) on span and ends it.
func endSpanWithError(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// recordInstanceFailure surfaces a reaped-launch as an APM error event on the
// active span so the failure reason is queryable in the error stream,
// correlated by trace id. No-op when ctx carries no recording span.
func recordInstanceFailure(ctx context.Context, instanceID, reason string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.RecordError(fmt.Errorf("instance %s launch failed: %s", instanceID, reason),
		trace.WithAttributes(
			attribute.String("instance.id", instanceID),
			attribute.String("failure.reason", reason),
		))
}

// RG-4 guest OOM tiers: user guests are reaped first; system instances (ELBv2,
// EKS) rank above user guests but below infra (OOMScoreAdjust=-500).
const (
	oomScoreUserGuest      = 500 // ManagedBy == "" — customer instance, reaped first
	oomScoreSystemInstance = 0   // ManagedBy != "" — elbv2 / eks, protected above user guests
)

// guestOOMScore returns the oom_score_adj per the RG-4 ladder.
// Pure function; split out for unit-testability.
func guestOOMScore(managedBy string) int {
	if managedBy == "" {
		return oomScoreUserGuest
	}
	return oomScoreSystemInstance
}

// Run launches a VM: validate state, mount volumes, exec QEMU, attach QMP,
// transition to Running, fire OnInstanceUp. Inserts the instance before
// transitioning. Used by RunInstances, start-stopped handler, restore, and crash recovery.
func (m *Manager) Run(ctx context.Context, instance *VM) error {
	return m.launch(ctx, instance)
}

// Start re-launches a stopped instance by id. Returns ErrInstanceNotFound when
// id is unknown so callers can map the failure to InvalidInstanceID.NotFound.
func (m *Manager) Start(ctx context.Context, id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrInstanceNotFound, id)
	}
	// StateError is rejected by launchStillValid; move to Pending first so
	// launch proceeds (resource re-allocation is the caller's responsibility).
	if m.Status(instance) == StateError && m.deps.TransitionState != nil {
		if err := m.deps.TransitionState(instance, StatePending); err != nil {
			return err
		}
	}
	return m.launch(ctx, instance)
}

// Reboot issues a QMP system_reset; the VM stays in StateRunning while QEMU
// re-runs firmware. Returns ErrInstanceNotFound or ErrInvalidTransition as appropriate.
func (m *Manager) Reboot(ctx context.Context, id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}
	if status := m.Status(instance); status != StateRunning {
		return fmt.Errorf("%w: cannot reboot instance %s in state %s",
			ErrInvalidTransition, id, status)
	}
	if _, err := sendQMPCommand(ctx, instance.QMPClient, qmp.QMPCommand{Execute: "system_reset"}, id); err != nil {
		return fmt.Errorf("QMP system_reset: %w", err)
	}
	return nil
}

// launchStillValid returns true when status is still pending/stopped/provisioning.
// Returns false if a concurrent terminate took ownership; launch must bail.
func (m *Manager) launchStillValid(instance *VM) bool {
	status := m.Status(instance)
	if status == StatePending || status == StateStopped || status == StateProvisioning {
		return true
	}
	slog.Info("Launch aborted by concurrent terminate", "instanceId", instance.ID, "status", string(status))
	return false
}

// launch is the orchestrator: pid check, mount volumes, exec QEMU, attach
// QMP, fire OnInstanceUp, transition to Running.
func (m *Manager) launch(ctx context.Context, instance *VM) (err error) {
	ctx, span := otel.Tracer(vmTracerName).Start(ctx, "vm.launch",
		trace.WithAttributes(
			attribute.String("instance.id", instance.ID),
			attribute.String("instance.type", instance.InstanceType),
		))
	defer func() { endSpanWithError(span, err) }()

	if !m.launchStillValid(instance) {
		return nil
	}

	pid, _ := utils.ReadPidFile(instance.ID)
	if pid > 0 {
		process, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := process.Signal(syscall.Signal(0)); err == nil {
			slog.ErrorContext(ctx, "Instance is already running", "InstanceID", instance.ID, "pid", pid)
			return errors.New("instance is already running")
		}
	}

	_, mountSpan := otel.Tracer(vmTracerName).Start(ctx, "vm.launch.mount_volumes")
	mountErr := m.deps.VolumeMounter.Mount(instance)
	endSpanWithError(mountSpan, mountErr)
	if mountErr != nil {
		slog.ErrorContext(ctx, "Failed to mount volumes", "err", mountErr)
		return mountErr
	}

	// Re-check status — Mount can take 30+s on cold AMIs, and a terminate may
	// race in during that window; bail to avoid resource contention.
	if !m.launchStillValid(instance) {
		return nil
	}

	_, qemuSpan := otel.Tracer(vmTracerName).Start(ctx, "vm.launch.start_qemu")
	qemuErr := m.startQEMU(instance)
	endSpanWithError(qemuSpan, qemuErr)
	if qemuErr != nil {
		slog.ErrorContext(ctx, "Failed to launch instance", "err", qemuErr)
		return qemuErr
	}

	_, qmpSpan := otel.Tracer(vmTracerName).Start(ctx, "vm.launch.qmp_connect")
	qmpClient, err := newQMPClientWithHandshake(ctx, instance)
	endSpanWithError(qmpSpan, err)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create QMP client", "err", err)
		// QEMU started but QMP handshake failed. Kill it synchronously so the
		// VFIO device is released before the caller frees the GPU pool entry;
		// otherwise the next Claim gets the same PCI address while QEMU still
		// holds /dev/vfio/<group>, causing "device or resource busy".
		if pid, pidErr := utils.ReadPidFile(instance.ID); pidErr == nil && pid > 0 {
			if proc, procErr := os.FindProcess(pid); procErr == nil {
				_ = proc.Signal(syscall.SIGKILL)
			}
			_ = utils.WaitForProcessExit(pid, 10*time.Second)
		}
		return err
	}
	instance.QMPClient = qmpClient
	go m.qmpHeartbeat(instance) //nolint:gosec // heartbeat outlives the launch request; must not inherit its cancellation

	m.Insert(instance)

	// Final race check — a concurrent terminate may have already transitioned to
	// shutting-down; let that goroutine own cleanup.
	if !m.launchStillValid(instance) {
		return nil
	}

	if m.deps.TransitionState != nil {
		_, stateSpan := otel.Tracer(vmTracerName).Start(ctx, "vm.launch.persist_state")
		transitionErr := m.deps.TransitionState(instance, StateRunning)
		endSpanWithError(stateSpan, transitionErr)
		if transitionErr != nil {
			slog.ErrorContext(ctx, "Failed to transition instance to running", "instanceId", instance.ID, "err", transitionErr)
			return transitionErr
		}
	}

	// Mark attached volumes as "in-use" now that instance is confirmed running.
	m.markAttachedVolumesInUse(instance)

	if m.deps.Hooks.OnInstanceUp != nil {
		// Launch path: per-instance subscribe failures are logged and the
		// launch still succeeds. The instance is reachable via cluster
		// fan-out (DescribeInstances) and the next OnInstanceUp on a
		// state-touching event will reinstall the subs idempotently.
		if err := m.deps.Hooks.OnInstanceUp(instance); err != nil {
			slog.ErrorContext(ctx, "OnInstanceUp hook reported error during launch",
				"instance", instance.ID, "err", err)
		}
	}

	return nil
}

// markAttachedVolumesInUse re-asserts "in-use" status for every volume an
// instance currently has attached (boot and non-boot) once it is confirmed
// running. Used by the launch path and the daemon-restart reconnect path so
// both keep volume state consistent with a running instance — otherwise a
// non-boot volume (e.g. an EKS stateful-pod data volume) can read "available"
// while the instance still has it attached. Errors are logged, not fatal.
func (m *Manager) markAttachedVolumesInUse(instance *VM) {
	if m.deps.VolumeStateUpdater == nil {
		return
	}
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()
	for _, ebsReq := range instance.EBSRequests.Requests {
		// EFI pflash is not a KV-tracked EBS volume; skip it like the unmount gate.
		if ebsReq.EFI {
			continue
		}
		if err := m.deps.VolumeStateUpdater.UpdateVolumeState(ebsReq.Name, "in-use", instance.ID, ebsReq.DeviceName); err != nil {
			slog.Error("Failed to update volume state to in-use", "volumeId", ebsReq.Name, "err", err)
		}
	}
}

const (
	nbdReadyTimeout    = 5 * time.Second
	qemuStartupTimeout = 5 * time.Second
)

// startQEMU launches the QEMU process for instance and waits for startup to
// confirm.
func (m *Manager) startQEMU(instance *VM) error {
	pidFile, err := utils.GeneratePidFile(instance.ID)
	if err != nil {
		slog.Error("Failed to generate PID file", "err", err)
		return err
	}

	if m.deps.InstanceTypes == nil {
		return fmt.Errorf("InstanceTypeResolver not wired")
	}
	spec, ok := m.deps.InstanceTypes.Resolve(instance.InstanceType)
	if !ok {
		return fmt.Errorf("instance type %s not found", instance.InstanceType)
	}

	runtimeDir := utils.RuntimeDir()
	consoleLogPath := filepath.Join(runtimeDir, fmt.Sprintf("console-%s.log", instance.ID))
	serialSocket := filepath.Join(runtimeDir, fmt.Sprintf("serial-%s.sock", instance.ID))

	if instance.DirectBoot {
		// Direct-boot (microvm) path: vm.Config was pre-built by the launcher.
		// Only fill in the runtime-generated paths that are not known until now.
		// SerialSocket is intentionally omitted: microvm uses a file chardev
		// so kernel output is captured without a socket client.
		instance.Config.PIDFile = pidFile
		instance.Config.ConsoleLogPath = consoleLogPath
	} else {
		instance.Config = buildBaseVMConfig(instance.ID, instance.InstanceType, pidFile, consoleLogPath, serialSocket, spec.Architecture, instance.BootMode, spec.VCPUs, spec.MemoryMiB)
		m.initENIRequests(instance)

		instance.EBSRequests.Mu.Lock()
		drives, iothreads, devices, err := buildDrives(instance.EBSRequests.Requests, spec.VCPUs, instance.Config.MachineType)
		instance.EBSRequests.Mu.Unlock()
		if err != nil {
			return err
		}
		instance.Config.Drives = append(instance.Config.Drives, drives...)
		instance.Config.IOThreads = append(instance.Config.IOThreads, iothreads...)
		instance.Config.Devices = append(instance.Config.Devices, devices...)
	}

	if instance.DirectBoot {
		// Direct-boot (microvm) path: NetDevs and Devices are already set in
		// the pre-built Config. Only create host-side tap devices for VPC ENIs
		// so the kernel tap interfaces exist before QEMU opens them.
		if instance.ENIId != "" && m.deps.NetworkPlumber != nil {
			// Primary tap goes on br-imds (so its egress meets the IMDS demux
			// flows); the bridge must exist before SetupTap can attach to it.
			if err := m.deps.NetworkPlumber.EnsureIMDSDatapathBridge(); err != nil {
				slog.Error("Failed to ensure IMDS bridge (direct-boot)", "eni", instance.ENIId, "err", err)
				return fmt.Errorf("ensure IMDS bridge: %w", err)
			}
			tapSpec := IMDSPrimaryTapSpec(instance.ENIId)
			if err := m.deps.NetworkPlumber.SetupTap(tapSpec); err != nil {
				slog.Error("Failed to set up tap device (direct-boot)", "eni", instance.ENIId, "err", err)
				return fmt.Errorf("setup tap device: %w", err)
			}
			slog.Info("VPC tap configured (direct-boot)", "tap", tapSpec.Name, "bridge", tapSpec.Bridge, "eni", instance.ENIId, "mac", instance.ENIMac)
			if err := m.attachPrimaryIMDSDatapath(instance); err != nil {
				return err
			}
			for _, extra := range instance.ExtraENIs {
				extraSpec := VPCTapSpec(extra.ENIID, extra.ENIMac)
				if err := m.deps.NetworkPlumber.SetupTap(extraSpec); err != nil {
					slog.Error("Failed to set up extra ENI tap (direct-boot)", "eni", extra.ENIID, "err", err)
					return fmt.Errorf("setup tap device for extra ENI %s: %w", extra.ENIID, err)
				}
			}
		}
		if instance.MgmtMAC != "" && m.deps.NetworkPlumber != nil {
			mgmtTap := MgmtTapName(instance.ID)
			mgmtBridge := "br-mgmt"
			if err := m.deps.NetworkPlumber.SetupTap(TapSpec{Name: mgmtTap, Bridge: mgmtBridge}); err != nil {
				slog.Error("Failed to set up mgmt tap (direct-boot)", "tap", mgmtTap, "err", err)
				return fmt.Errorf("setup mgmt tap: %w", err)
			}
			slog.Info("Mgmt tap configured (direct-boot)", "tap", mgmtTap, "mac", instance.MgmtMAC, "ip", instance.MgmtIP, "instanceId", instance.ID)
		}
	} else {
		if instance.ENIId != "" && m.deps.NetworkPlumber != nil {
			// Primary tap goes on br-imds (so its egress meets the IMDS demux
			// flows); the bridge must exist before SetupTap can attach to it.
			if err := m.deps.NetworkPlumber.EnsureIMDSDatapathBridge(); err != nil {
				slog.Error("Failed to ensure IMDS bridge", "eni", instance.ENIId, "err", err)
				return fmt.Errorf("ensure IMDS bridge: %w", err)
			}
			spec := IMDSPrimaryTapSpec(instance.ENIId)
			if err := m.deps.NetworkPlumber.SetupTap(spec); err != nil {
				slog.Error("Failed to set up tap device", "eni", instance.ENIId, "err", err)
				return fmt.Errorf("setup tap device: %w", err)
			}
			tapName := spec.Name

			instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
				Value: fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", tapName),
			})
			instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, "net0", instance.ENIMac))
			slog.Info("VPC networking configured", "tap", tapName, "bridge", spec.Bridge, "eni", instance.ENIId, "mac", instance.ENIMac)
			if err := m.attachPrimaryIMDSDatapath(instance); err != nil {
				return err
			}

			if err := m.setupExtraENINICs(instance); err != nil {
				return err
			}

			if m.deps.DevNetworking {
				m.appendDevHostfwdNIC(instance)
			}
		} else {
			sshDebugAddr, err := viperblock.FindFreePort()
			if err != nil {
				slog.Error("Failed to find free port", "err", err)
				return err
			}
			_, sshDebugPort, err := net.SplitHostPort(sshDebugAddr)
			if err != nil {
				slog.Error("Failed to parse port from address", "addr", sshDebugAddr, "err", err)
				return fmt.Errorf("parse port from %s: %w", sshDebugAddr, err)
			}

			bindIP := m.deps.BindHost
			if bindIP == "" || bindIP == "0.0.0.0" {
				bindIP = "127.0.0.1"
			}
			instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
				Value: fmt.Sprintf("user,id=net0,hostfwd=tcp:%s:%s-:22", bindIP, sshDebugPort),
			})
			instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, "net0", ""))
		}

		if instance.MgmtMAC != "" {
			mgmtTap := MgmtTapName(instance.ID)
			// Pre-create the mgmt tap owned by the daemon euid, mirroring the
			// direct-boot branch above. The non-root daemon has no CAP_NET_ADMIN,
			// so QEMU can only attach a tap it already owns; without this the
			// disk-boot/restart path fails with /dev/net/tun: Operation not
			// permitted (blocks stopped-instance restart incl. EKS CP recovery).
			if m.deps.NetworkPlumber != nil {
				if err := m.deps.NetworkPlumber.SetupTap(TapSpec{Name: mgmtTap, Bridge: "br-mgmt"}); err != nil {
					slog.Error("Failed to set up mgmt tap", "tap", mgmtTap, "err", err)
					return fmt.Errorf("setup mgmt tap: %w", err)
				}
			}
			instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
				Value: fmt.Sprintf("tap,id=mgmt0,ifname=%s,script=no,downscript=no", mgmtTap),
			})
			instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, "mgmt0", instance.MgmtMAC))
			if err := m.appendSystemNetcfgFwCfg(instance); err != nil {
				return fmt.Errorf("attach system netcfg: %w", err)
			}
			slog.Info("Management NIC configured", "tap", mgmtTap, "mac", instance.MgmtMAC, "ip", instance.MgmtIP, "instanceId", instance.ID)
		}

		instance.Config.Devices = append(instance.Config.Devices, RngDevice(instance.Config.MachineType))
	}

	for i, att := range instance.GPUAttachments {
		var devSpec string
		if att.MdevPath != "" {
			devSpec = fmt.Sprintf("vfio-pci,sysfsdev=%s,id=gpu%d,x-vga=off", att.MdevPath, i)
			slog.Info("MIG device configured", "mdev", att.MdevPath, "index", i, "instanceId", instance.ID)
		} else {
			xvga := "off"
			if att.XVGAEnabled {
				xvga = "on"
			}
			devSpec = fmt.Sprintf("vfio-pci,host=%s,id=gpu%d,x-vga=%s", att.PCIAddress, i, xvga)
			slog.Info("GPU passthrough device configured",
				"pci", att.PCIAddress, "index", i, "instanceId", instance.ID, "xvga", xvga)
		}
		instance.Config.Devices = append(instance.Config.Devices, Device{Value: devSpec})
	}

	qmpSocket, err := utils.GenerateSocketFile(fmt.Sprintf("qmp-%s", instance.ID))
	if err != nil {
		slog.Error("Failed to generate QMP socket", "err", err)
		return err
	}
	instance.Config.QMPSocket = qmpSocket

	// A predecessor QEMU killed with SIGKILL (crash/etcd-reset restart) leaves its
	// qmp-<id>.sock inode behind. QEMU rebinds the socket, but the stale inode
	// makes the startup os.Stat probe and the QMP dial race a dead listener
	// (connection refused). Unlink it so the socket that appears is the fresh
	// QEMU's. The launch pid-check above already ruled out a live owner.
	if err := removeStaleQMPSocket(qmpSocket); err != nil {
		slog.Warn("Failed to remove stale QMP socket", "path", qmpSocket, "err", err)
	}

	// Second QMP monitor for the metrics collector; a stale socket from a
	// SIGKILLed QEMU is unlinked so the fresh process can bind. Telemetry
	// never blocks a launch — failures degrade to no metrics for this VM.
	if telemetrySocket, terr := utils.GenerateSocketFile(utils.QMPTelemetryPrefix + instance.ID); terr != nil {
		slog.Warn("Failed to generate telemetry QMP socket", "instanceId", instance.ID, "err", terr)
	} else {
		_ = os.Remove(telemetrySocket)
		instance.Config.TelemetryQMPSocket = telemetrySocket
		refreshTelemetryMeta(instance)
	}

	instance.EBSRequests.Mu.Lock()
	nbdEndpoints := make([]struct{ name, uri string }, 0, len(instance.EBSRequests.Requests))
	for _, req := range instance.EBSRequests.Requests {
		if req.NBDURI != "" {
			nbdEndpoints = append(nbdEndpoints, struct{ name, uri string }{req.Name, req.NBDURI})
		}
	}
	instance.EBSRequests.Mu.Unlock()
	for _, ep := range nbdEndpoints {
		if err := utils.WaitForNBDReady(ep.uri, nbdReadyTimeout); err != nil {
			return fmt.Errorf("nbd endpoint not ready for %s: %w", ep.name, err)
		}
	}

	processChan := make(chan int, 1)
	exitChan := make(chan int, 1)
	startupConfirmed := make(chan bool, 1)

	go func() {
		cmd, err := instance.Config.Execute()
		if err != nil {
			slog.Error("Failed to execute VM", "err", err)
			processChan <- 0
			return
		}

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			slog.Error("Failed to pipe STDOUT VM", "err", err)
			processChan <- 0
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			slog.Error("Failed to pipe STDERR VM", "err", err)
			processChan <- 0
			return
		}

		if err := cmd.Start(); err != nil {
			slog.Error("Failed to start VM", "err", err)
			processChan <- 0
			return
		}

		slog.Info("VM started successfully", "pid", cmd.Process.Pid)

		oomScore := guestOOMScore(instance.ManagedBy)
		if err := utils.SetOOMScore(cmd.Process.Pid, oomScore); err != nil {
			slog.Warn("Failed to set QEMU OOM score", "pid", cmd.Process.Pid, "score", oomScore, "err", err)
		}

		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				slog.Info("[qemu]", "line", scanner.Text())
			}
		}()
		go func() {
			scanner := bufio.NewScanner(stderr)
			slog.Info("QEMU stderr reader started")
			for scanner.Scan() {
				slog.Error("[qemu-stderr]", "line", scanner.Text())
			}
		}()

		processChan <- cmd.Process.Pid

		waitErr := cmd.Wait()
		if waitErr != nil {
			slog.Error("VM process exited", "instance", instance.ID, "err", waitErr)
		}

		select {
		case exitChan <- 1:
		default:
		}

		confirmed := <-startupConfirmed
		if !confirmed {
			return
		}

		if waitErr != nil {
			if m.deps.CrashHandler != nil {
				m.deps.CrashHandler(instance, waitErr)
			}
		} else {
			slog.Info("VM process exited cleanly", "instance", instance.ID)
		}
	}()

	pid := <-processChan
	if pid == 0 {
		return fmt.Errorf("failed to start qemu")
	}

	// Race QMP-socket appearance against early process exit; SIGKILL on timeout
	// so a wedged QEMU does not orphan its tap / volumes.
	timeoutTimer := time.NewTimer(qemuStartupTimeout)
	defer timeoutTimer.Stop()
	pollTicker := time.NewTicker(20 * time.Millisecond)
	defer pollTicker.Stop()
	for {
		if _, err := os.Stat(instance.Config.QMPSocket); err == nil {
			startupConfirmed <- true
			slog.Info("QEMU started successfully and is running",
				"console_log", instance.Config.ConsoleLogPath,
				"serial_socket", instance.Config.SerialSocket)
			break
		}
		select {
		case exitErr := <-exitChan:
			startupConfirmed <- false
			errorMsg := fmt.Errorf("qemu exited during startup (code=%d)", exitErr)
			slog.Error("Failed to launch qemu", "err", errorMsg, "instanceId", instance.ID)
			return errorMsg
		case <-timeoutTimer.C:
			startupConfirmed <- false
			if proc, findErr := os.FindProcess(pid); findErr == nil {
				_ = proc.Signal(syscall.SIGKILL)
			}
			return fmt.Errorf("timeout waiting for QMP socket %s", instance.Config.QMPSocket)
		case <-pollTicker.C:
		}
	}

	// Wait for the pidfile so we don't tear down the tap before QEMU finishes
	// attaching to it (can lag under post-reboot recovery load).
	if _, err := utils.WaitForPidFile(instance.ID, 3*time.Second); err != nil {
		slog.Error("Failed to read PID file", "err", err)
		return err
	}

	return nil
}

// findFreePort is a var so tests can swap in a stub to reach appendDevHostfwdNIC's
// failure paths (viperblock.FindFreePort always succeeds in production).
var findFreePort = viperblock.FindFreePort

// appendDevHostfwdNIC adds a user-mode NIC with SSH hostfwd for dev access.
func (m *Manager) appendDevHostfwdNIC(instance *VM) {
	sshDebugAddr, err := findFreePort()
	if err != nil {
		slog.Warn("DEV_NETWORKING: failed to find free port for dev NIC", "err", err)
		return
	}
	_, sshDebugPort, err := net.SplitHostPort(sshDebugAddr)
	if err != nil {
		slog.Warn("DEV_NETWORKING: failed to parse port from address", "addr", sshDebugAddr, "err", err)
		return
	}
	bindIP := m.deps.BindHost
	if bindIP == "" || bindIP == "0.0.0.0" {
		bindIP = "127.0.0.1"
	}
	var nb strings.Builder
	fmt.Fprintf(&nb, "user,id=dev0,hostfwd=tcp:%s:%s-:22", bindIP, sshDebugPort)

	if instance.ExtraHostfwd != nil {
		for guestPort := range instance.ExtraHostfwd {
			fwdAddr, fwdErr := findFreePort()
			if fwdErr != nil {
				slog.Warn("DEV_NETWORKING: failed to find free port for extra hostfwd", "guestPort", guestPort, "err", fwdErr)
				continue
			}
			_, hostPort, splitErr := net.SplitHostPort(fwdAddr)
			if splitErr != nil {
				slog.Warn("DEV_NETWORKING: failed to parse extra hostfwd address", "fwdAddr", fwdAddr, "err", splitErr)
				continue
			}
			hostPortInt, convErr := strconv.Atoi(hostPort)
			if convErr != nil {
				slog.Warn("DEV_NETWORKING: failed to convert extra hostfwd port", "hostPort", hostPort, "err", convErr)
				continue
			}
			fmt.Fprintf(&nb, ",hostfwd=tcp:%s:%s-:%d", bindIP, hostPort, guestPort)
			instance.ExtraHostfwd[guestPort] = hostPortInt
			slog.Info("DEV_NETWORKING: extra hostfwd", "guestPort", guestPort, "hostPort", hostPort, "instanceId", instance.ID)
		}
	}

	instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{Value: nb.String()})
	devMac := GenerateDevMAC(instance.ID)
	instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, "dev0", devMac))
	slog.Info("DEV_NETWORKING: added dev NIC with SSH hostfwd",
		"bindIP", bindIP, "port", sshDebugPort, "mac", devMac, "instanceId", instance.ID)
}

// AttachQMP connects a QMP client to an already-running QEMU process and
// starts the heartbeat goroutine. Used by reconnect callers on daemon restart.
func (m *Manager) AttachQMP(instance *VM) error {
	// Reconnect path runs outside any request; not request-scoped.
	client, err := newQMPClientWithHandshake(context.Background(), instance)
	if err != nil {
		return err
	}
	instance.QMPClient = client
	go m.qmpHeartbeat(instance)
	return nil
}

const (
	// qmpDialTimeout bounds how long the QMP dial retries a transient connect
	// failure after a QEMU relaunch before giving up.
	qmpDialTimeout = 3 * time.Second
	// qmpDialRetryInterval is the backoff between QMP connect retries.
	qmpDialRetryInterval = 50 * time.Millisecond
)

// qmpSocketWaitTimeout bounds how long newQMPClientWithHandshake waits for the
// QMP socket inode to appear before dialling. Mirrors qmpDialTimeout; a test
// seam overrides it to keep dial-failure tests off the wall clock.
var qmpSocketWaitTimeout = 3 * time.Second

const (
	// qmpVFIOGreetingBase is the fixed VFIO startup overhead before RAM pinning
	// dominates (IOMMU domain setup, device realize). The RAM term is added on
	// top; the result is floored so small guests keep the proven deadline.
	qmpVFIOGreetingBase = 30 * time.Second
	// qmpVFIOGreetingPerGiB is the wait added per GiB of guest RAM: the VFIO pin
	// is synchronous and its cost grows ~linearly with RAM and host memory
	// pressure, so a flat deadline under-waits large guests and SIGKILLs a QEMU
	// that would have come up. Generous to cover a loaded host.
	qmpVFIOGreetingPerGiB = 8 * time.Second
	// qmpVFIOGreetingFloor is the minimum VFIO greeting wait. QEMU must
	// lock+DMA-map the whole guest RAM before the monitor answers, which far
	// exceeds the plain-VM default even for a small guest (a 16GB EKS GPU worker
	// takes >30s on wattle). Small guests land here.
	qmpVFIOGreetingFloor = 180 * time.Second
	// qmpVFIOGreetingCap bounds the scaled wait so a mis-sized guest cannot
	// wedge a launch indefinitely.
	qmpVFIOGreetingCap = 600 * time.Second
)

// qmpGreetingTimeout picks the QMP greeting deadline for a VM: the plain default
// unless the guest has GPU/VFIO passthrough, whose synchronous RAM pin delays the
// monitor. For VFIO the deadline is base + perGiB*RAM, floored so small guests
// keep the proven deadline and capped so a huge guest cannot wedge a launch.
func qmpGreetingTimeout(v *VM) time.Duration {
	if len(v.GPUAttachments) == 0 {
		return qmp.DefaultGreetingTimeout
	}
	memGiB := v.Config.Memory / 1024
	scaled := qmpVFIOGreetingBase + time.Duration(memGiB)*qmpVFIOGreetingPerGiB
	scaled = max(scaled, qmpVFIOGreetingFloor)
	scaled = min(scaled, qmpVFIOGreetingCap)
	return scaled
}

// removeStaleQMPSocket unlinks a leftover QMP socket inode from a prior QEMU so
// the startup probe and QMP dial cannot race a dead listener. A missing file is
// not an error.
func removeStaleQMPSocket(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// dialQMPWithRetry redials the QMP socket until the connect+greeting succeeds or
// the deadline passes. A just-relaunched QEMU can present the socket inode a beat
// before it listen()s, and a stale inode from a SIGKILLed predecessor refuses the
// first connect; a fresh QEMU also accepts the connect then resets mid-greeting
// while it initialises. A single dial then loses the race. Only transient connect
// or reset errors are retried — a decode/handshake failure from a settled QEMU
// surfaces immediately.
func dialQMPWithRetry(path string, greetingTimeout time.Duration) (*qmp.QMPClient, error) {
	deadline := time.Now().Add(qmpDialTimeout)
	for {
		client, err := qmp.NewQMPClientWithGreetingTimeout(path, greetingTimeout)
		if err == nil {
			return client, nil
		}
		if time.Now().After(deadline) || !isTransientDialError(err) {
			return nil, err
		}
		time.Sleep(qmpDialRetryInterval)
	}
}

// isTransientDialError reports whether a QMP connect failed because the listener
// is not up yet: connection refused (bound but pre-listen, or a stale dead
// inode), the socket momentarily absent between unlink and rebind, or the peer
// tearing the connection down mid-greeting (reset/broken pipe) — a fresh QEMU
// that accepts then resets before it can service the monitor. Each clears once
// QEMU finishes initialising, so the dial is retried rather than surfaced.
func isTransientDialError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// newQMPClientWithHandshake dials the QMP socket and runs the qmp_capabilities
// handshake. The caller is responsible for starting the heartbeat goroutine.
func newQMPClientWithHandshake(ctx context.Context, v *VM) (*qmp.QMPClient, error) {
	// QMP socket bind lags the pidfile under recovery load; wait for the
	// socket inode to exist before dialling to avoid an ENOENT race.
	if err := utils.WaitForUnixSocket(v.Config.QMPSocket, qmpSocketWaitTimeout); err != nil {
		return nil, fmt.Errorf("connect QMP socket %s: %w", v.Config.QMPSocket, err)
	}
	client, err := dialQMPWithRetry(v.Config.QMPSocket, qmpGreetingTimeout(v))
	if err != nil {
		return nil, fmt.Errorf("connect QMP socket %s: %w", v.Config.QMPSocket, err)
	}
	// A VFIO guest greets before its RAM pin completes, then cannot service
	// qmp_capabilities until the pin finishes — which scales with guest RAM and
	// host memory pressure. Give the handshake the same budget as the greeting so
	// the reply is not decode-timed-out and the launch SIGKILLed mid-pin.
	if _, err := sendQMPCommandWithTimeout(ctx, client, qmp.QMPCommand{Execute: "qmp_capabilities"}, v.ID, qmpGreetingTimeout(v)); err != nil {
		_ = client.Conn.Close()
		return nil, err
	}
	slog.DebugContext(ctx, "QMP handshake complete", "instance", v.ID)
	return client, nil
}

const (
	// qmpHeartbeatInterval is the QMP query-status poll period.
	qmpHeartbeatInterval = 30 * time.Second
	// QMPMaxConsecutiveFailures is how many back-to-back query-status failures
	// mark an instance impaired and trigger a process-liveness check (90s of
	// unresponsiveness). Exported so DescribeInstanceStatus uses the same gate.
	QMPMaxConsecutiveFailures = 3
)

// qmpHeartbeat polls query-status every qmpHeartbeatInterval and acts on
// failures: it tracks consecutive failures, and once they reach
// QMPMaxConsecutiveFailures it checks process liveness. A dead process triggers
// crash recovery; an alive-but-wedged QEMU stays impaired for an operator. The
// goroutine exits and closes the QMP connection on any terminal/transitional state.
func (m *Manager) qmpHeartbeat(instance *VM) {
	for {
		time.Sleep(qmpHeartbeatInterval)

		status := m.Status(instance)

		if status == StateStopping || status == StateStopped ||
			status == StateShuttingDown || status == StateTerminated || status == StateError {
			slog.Info("QMP heartbeat exiting - instance not running", "instance", instance.ID, "status", status)
			if instance.QMPClient != nil && instance.QMPClient.Conn != nil {
				if err := instance.QMPClient.Conn.Close(); err != nil {
					slog.Error("Failed to close QMP connection", "instance", instance.ID, "err", err)
				}
			}
			return
		}

		slog.Debug("QMP heartbeat", "instance", instance.ID)
		qmpStatus, err := sendQMPCommand(context.WithValue(context.Background(), noTraceKey{}, true),
			instance.QMPClient, qmp.QMPCommand{Execute: "query-status"}, instance.ID)
		if err != nil {
			failures := m.recordQMPFailure(instance)
			slog.Warn("QMP heartbeat failed", "instance", instance.ID, "consecutiveFailures", failures, "err", err)
			if failures < QMPMaxConsecutiveFailures {
				continue
			}
			if !isInstanceProcessRunning(instance) {
				slog.Error("QEMU process dead and QMP unresponsive, triggering crash recovery",
					"instance", instance.ID, "consecutiveFailures", failures)
				m.HandleCrash(instance, fmt.Errorf("qmp unresponsive (%d failures), process dead", failures))
				return
			}
			slog.Error("QEMU process alive but QMP unresponsive", "instance", instance.ID, "consecutiveFailures", failures)
			continue
		}

		m.recordQMPSuccess(instance)
		slog.Debug("QMP status", "instance", instance.ID, "status", string(qmpStatus.Return))
	}
}

// recordQMPFailure increments the consecutive QMP failure counter, stamping
// ImpairedSince when the count first reaches QMPMaxConsecutiveFailures. Returns
// the new count.
func (m *Manager) recordQMPFailure(instance *VM) int {
	var count int
	m.UpdateState(instance.ID, func(v *VM) {
		v.Health.QMPConsecutiveFailures++
		count = v.Health.QMPConsecutiveFailures
		if count == QMPMaxConsecutiveFailures {
			v.Health.ImpairedSince = time.Now()
		}
	})
	return count
}

// recordQMPSuccess clears the failure counter and impaired marker after a
// healthy poll and records the success time.
func (m *Manager) recordQMPSuccess(instance *VM) {
	m.UpdateState(instance.ID, func(v *VM) {
		v.Health.QMPConsecutiveFailures = 0
		v.Health.ImpairedSince = time.Time{}
		v.Health.LastQMPSuccess = time.Now()
	})
}

// qmpCommandTimeout bounds a QMP command's response decode for a running guest.
// The handshake on a VFIO launch needs longer (the monitor greets before the RAM
// pin finishes, then stalls the qmp_capabilities reply) and passes its own value.
const qmpCommandTimeout = 30 * time.Second

// sendQMPCommand encodes cmd and decodes the response, skipping event messages.
func sendQMPCommand(ctx context.Context, q *qmp.QMPClient, cmd qmp.QMPCommand, instanceID string) (*qmp.QMPResponse, error) {
	return sendQMPCommandWithTimeout(ctx, q, cmd, instanceID, qmpCommandTimeout)
}

// sendQMPCommandWithTimeout is sendQMPCommand with an explicit response deadline,
// re-armed after each interleaved event. VFIO handshakes pass a RAM-scaled value.
func sendQMPCommandWithTimeout(ctx context.Context, q *qmp.QMPClient, cmd qmp.QMPCommand, instanceID string, timeout time.Duration) (_ *qmp.QMPResponse, err error) {
	if ctx.Value(noTraceKey{}) == nil {
		var span trace.Span
		_, span = otel.Tracer(vmTracerName).Start(ctx, "qmp "+cmd.Execute,
			trace.WithAttributes(
				attribute.String("qmp.command", cmd.Execute),
				attribute.String("instance.id", instanceID),
			))
		defer func() { endSpanWithError(span, err) }()
	}

	if q == nil || q.Encoder == nil || q.Decoder == nil {
		return nil, fmt.Errorf("QMP client is not initialized")
	}

	q.Mu.Lock()
	defer q.Mu.Unlock()

	// A prior timed-out decode leaves the shared Decoder mid-message, wedging
	// every subsequent command (a single contention spike would otherwise break
	// EBS attach/detach on this instance for the VM's lifetime). Redial first.
	if q.Dead {
		if err := reconnectQMP(q, instanceID); err != nil {
			return nil, fmt.Errorf("reconnect wedged QMP client: %w", err)
		}
	}

	if err := q.Conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = q.Conn.SetReadDeadline(time.Time{}) }()

	if err := q.Encoder.Encode(cmd); err != nil {
		q.Dead = true
		return nil, fmt.Errorf("encode error: %w", err)
	}

	for {
		var msg map[string]any
		if err := q.Decoder.Decode(&msg); err != nil {
			// The stream position is now unknown; force a reconnect next call.
			q.Dead = true
			return nil, fmt.Errorf("decode error: %w", err)
		}
		if _, ok := msg["event"]; ok {
			slog.InfoContext(ctx, "QMP event", "event", msg["event"], "instanceId", instanceID)
			if err := q.Conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				return nil, fmt.Errorf("set read deadline: %w", err)
			}
			continue
		}
		if errObj, ok := msg["error"].(map[string]any); ok {
			return nil, fmt.Errorf("QMP error: %s: %s", errObj["class"], errObj["desc"])
		}
		if _, ok := msg["return"]; ok {
			respBytes, err := json.Marshal(msg)
			if err != nil {
				return nil, fmt.Errorf("marshal QMP response: %w", err)
			}
			var resp qmp.QMPResponse
			if err := json.Unmarshal(respBytes, &resp); err != nil {
				return nil, fmt.Errorf("unmarshal error: %w", err)
			}
			return &resp, nil
		}
	}
}

// reconnectQMP redials a wedged QMP client's socket and re-runs the
// qmp_capabilities handshake in place, so the cached *qmp.QMPClient pointer the
// instance holds stays valid. The caller must hold q.Mu. The capabilities
// exchange runs inline rather than via sendQMPCommand because the lock is held.
func reconnectQMP(q *qmp.QMPClient, instanceID string) error {
	if q.Path == "" {
		return fmt.Errorf("QMP client has no socket path")
	}

	// QEMU's monitor is a single-client `server,nowait` listener: the old
	// connection must be closed before redialing, or it still holds the
	// only client slot and the fresh dial gets no greeting, blocking until
	// NewQMPClient's read deadline expires. dialQMPWithRetry absorbs the
	// brief post-close ECONNREFUSED window while the listener re-arms.
	if q.Conn != nil {
		_ = q.Conn.Close()
	}

	// A reconnect targets an already-running QEMU whose RAM is long pinned, so
	// the greeting returns promptly — the plain default deadline is enough.
	fresh, err := dialQMPWithRetry(q.Path, qmp.DefaultGreetingTimeout)
	if err != nil {
		return fmt.Errorf("redial QMP socket %s: %w", q.Path, err)
	}
	// QEMU starts in Negotiation mode and rejects every command until
	// qmp_capabilities completes, so run it before swapping the client in.
	if err := fresh.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		_ = fresh.Conn.Close()
		return fmt.Errorf("set capabilities deadline: %w", err)
	}
	if err := fresh.Encoder.Encode(qmp.QMPCommand{Execute: "qmp_capabilities"}); err != nil {
		_ = fresh.Conn.Close()
		return fmt.Errorf("encode qmp_capabilities: %w", err)
	}
	for {
		var msg map[string]any
		if err := fresh.Decoder.Decode(&msg); err != nil {
			_ = fresh.Conn.Close()
			return fmt.Errorf("decode qmp_capabilities: %w", err)
		}
		if _, ok := msg["event"]; ok {
			continue
		}
		if errObj, ok := msg["error"].(map[string]any); ok {
			_ = fresh.Conn.Close()
			return fmt.Errorf("qmp_capabilities error: %v: %v", errObj["class"], errObj["desc"])
		}
		if _, ok := msg["return"]; ok {
			break
		}
	}
	_ = fresh.Conn.SetReadDeadline(time.Time{})

	q.Conn = fresh.Conn
	q.Decoder = fresh.Decoder
	q.Encoder = fresh.Encoder
	q.Dead = false
	slog.Info("QMP client reconnected after wedged stream", "instance", instanceID, "socket", q.Path)
	return nil
}

// EBSHotPlugSlotCount is the fixed number of PCIe root ports pre-allocated for
// EBS hot-plug, matching the /dev/sd[f-p] range. Cannot grow without QEMU restart.
const EBSHotPlugSlotCount = 11

// appendSystemNetcfgFwCfg attaches an fw_cfg netcfg blob describing a multi-NIC
// BootAMI system VM's interfaces so the guest brings them up deterministically
// by MAC. The EKS control plane is the case: cloud-init on a stock Alpine guest
// cannot reliably pick the right NIC out of two, so it must be told which is
// which. The primary data ENI is marked DHCP (OVN serves it; it carries the
// default route, and bringing it up before cloud-init's network stage is what
// lets the Ec2 datasource reach IMDS — without it cloud-init brings up the mgmt
// NIC instead and falls to DataSourceNone). mgmt0 lives on br-mgmt with no DHCP,
// so its static address is delivered here; it is never the default route. The
// blob key format matches daemon.buildNetcfgBlob and build/microvm/init.sh.
// No-op without a mgmt NIC, so single-NIC guests are untouched — cloud-init
// brings their one NIC up.
func (m *Manager) appendSystemNetcfgFwCfg(instance *VM) error {
	if instance.MgmtMAC == "" || instance.MgmtIP == "" {
		return nil
	}
	var b strings.Builder
	n := 0
	// Primary data ENI: DHCP (OVN-served) + default route.
	if instance.ENIMac != "" {
		fmt.Fprintf(&b, "NIC%d_MAC=%s\nNIC%d_DHCP=1\nNIC%d_DEFAULT=1\n", n, instance.ENIMac, n, n)
		n++
	}
	// Management NIC: static, off br-mgmt, never the default route.
	fmt.Fprintf(&b, "NIC%d_MAC=%s\nNIC%d_CIDR=%s/24\nNIC%d_DEFAULT=0\n", n, instance.MgmtMAC, n, instance.MgmtIP, n)

	path := filepath.Join(utils.RuntimeDir(), fmt.Sprintf("fwcfg-%s-netcfg.tmp", instance.ID))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write system netcfg blob: %w", err)
	}
	instance.Config.FwCfg = append(instance.Config.FwCfg, FwCfgEntry{Name: "opt/spinifex/netcfg", File: path})
	return nil
}

// ec2SMBIOSUUID derives a deterministic, "ec2"-prefixed system UUID from the
// instance ID so cloud-init's Ec2 datasource activates (stable across reboot).
func ec2SMBIOSUUID(instanceID string) string {
	sum := sha256.Sum256([]byte("ec2-smbios:" + instanceID))
	h := "ec2" + hex.EncodeToString(sum[:])[3:32]
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// buildBaseVMConfig creates a Config with base QEMU settings and two pre-allocated
// PCIe root-port pools: hotplug-ebs{1..N} for EBS (/dev/sd[f-p]) and
// hotplug-eni{1..M} for ENI hot-plug. bootMode "uefi"/"uefi-preferred" sets UseUEFI.
func buildBaseVMConfig(instanceID, instanceType, pidFile, consoleLogPath, serialSocket, architecture, bootMode string, vCPUs, memoryMiB int) Config {
	cfg := Config{
		Name:           instanceID,
		PIDFile:        pidFile,
		EnableKVM:      true,
		NoGraphic:      true,
		MachineType:    "q35",
		ConsoleLogPath: consoleLogPath,
		SerialSocket:   serialSocket,
		CPUType:        "host",
		Memory:         memoryMiB,
		CPUCount:       vCPUs,
		Architecture:   architecture,
		InstanceType:   instanceType,
		UseUEFI:        bootMode == "uefi" || bootMode == "uefi-preferred",

		// Present EC2-shaped DMI so stock cloud-init activates the Ec2 datasource.
		SMBIOSUUID:         ec2SMBIOSUUID(instanceID),
		SMBIOSManufacturer: "Amazon EC2",
		SMBIOSAssetTag:     "Amazon EC2",
	}

	for i := 1; i <= EBSHotPlugSlotCount; i++ {
		cfg.Devices = append(cfg.Devices, Device{
			Value: fmt.Sprintf("pcie-root-port,id=hotplug-ebs%d,chassis=%d,slot=0", i, i),
		})
	}

	eniSlots := instancetypes.HotPlugENISlotsForType(instanceType)
	for i := 1; i <= eniSlots; i++ {
		chassis := EBSHotPlugSlotCount + i
		cfg.Devices = append(cfg.Devices, Device{
			Value: fmt.Sprintf("pcie-root-port,id=hotplug-eni%d,chassis=%d,slot=0", i, chassis),
		})
	}

	return cfg
}

// initENIRequests resets the per-VM ENI slot free-list to mirror the
// hotplug-eni{1..N} root ports in buildBaseVMConfig. AttachedByENIID is
// preserved across restart; on a cold boot it starts empty.
func (m *Manager) initENIRequests(instance *VM) {
	eniSlots := instancetypes.HotPlugENISlotsForType(instance.InstanceType)
	instance.ENIRequests.Mu.Lock()
	defer instance.ENIRequests.Mu.Unlock()
	instance.ENIRequests.AvailableSlots = make([]int, 0, eniSlots)
	for i := 1; i <= eniSlots; i++ {
		instance.ENIRequests.AvailableSlots = append(instance.ENIRequests.AvailableSlots, i)
	}
	if instance.ENIRequests.AttachedByENIID == nil {
		instance.ENIRequests.AttachedByENIID = make(map[string]int)
	}
}

// buildDrives converts EBS requests into QEMU drive/iothread/device configs.
// Returns an error if any volume is missing its NBDURI. EFI volumes emit
// pflash unit=1; the readonly CODE blob (unit=0) is added by Config.Execute.
func buildDrives(requests []types.EBSRequest, cpuCount int, machineType string) ([]Drive, []IOThread, []Device, error) {
	var drives []Drive
	var iothreads []IOThread
	var devices []Device

	for _, v := range requests {
		if v.NBDURI == "" {
			return nil, nil, nil, fmt.Errorf("NBDURI not set for volume %s - was volume mounted?", v.Name)
		}

		drive := Drive{File: v.NBDURI}

		if v.Boot {
			drive.Format = "raw"
			drive.If = "none"
			drive.Media = "disk"
			drive.ID = "os"
			drive.Cache = "none"

			iothreadID := "ioth-os"
			iothreads = append(iothreads, IOThread{ID: iothreadID})
			devices = append(devices, BlkDevice(machineType, drive.ID, iothreadID, cpuCount, 1))
		}

		if v.EFI {
			drive.Format = "raw"
			drive.If = "pflash"
			drive.Unit = 1
		}

		slog.Info("Using NBD URI for drive", "volume", v.Name, "uri", v.NBDURI)
		drives = append(drives, drive)
	}

	return drives, iothreads, devices, nil
}
