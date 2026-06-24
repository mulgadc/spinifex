package vm

import (
	"bufio"
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
)

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
func (m *Manager) Run(instance *VM) error {
	return m.launch(instance)
}

// Start re-launches a stopped instance by id. Returns ErrInstanceNotFound when
// id is unknown so callers can map the failure to InvalidInstanceID.NotFound.
func (m *Manager) Start(id string) error {
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
	return m.launch(instance)
}

// Reboot issues a QMP system_reset; the VM stays in StateRunning while QEMU
// re-runs firmware. Returns ErrInstanceNotFound or ErrInvalidTransition as appropriate.
func (m *Manager) Reboot(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return ErrInstanceNotFound
	}
	if status := m.Status(instance); status != StateRunning {
		return fmt.Errorf("%w: cannot reboot instance %s in state %s",
			ErrInvalidTransition, id, status)
	}
	if _, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "system_reset"}, id); err != nil {
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
func (m *Manager) launch(instance *VM) error {
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
			slog.Error("Instance is already running", "InstanceID", instance.ID, "pid", pid)
			return errors.New("instance is already running")
		}
	}

	if err := m.deps.VolumeMounter.Mount(instance); err != nil {
		slog.Error("Failed to mount volumes", "err", err)
		return err
	}

	// Re-check status — Mount can take 30+s on cold AMIs, and a terminate may
	// race in during that window; bail to avoid resource contention.
	if !m.launchStillValid(instance) {
		return nil
	}

	if err := m.startQEMU(instance); err != nil {
		slog.Error("Failed to launch instance", "err", err)
		return err
	}

	qmpClient, err := newQMPClientWithHandshake(instance)
	if err != nil {
		slog.Error("Failed to create QMP client", "err", err)
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
	go m.qmpHeartbeat(instance)

	m.Insert(instance)

	// Final race check — a concurrent terminate may have already transitioned to
	// shutting-down; let that goroutine own cleanup.
	if !m.launchStillValid(instance) {
		return nil
	}

	if m.deps.TransitionState != nil {
		if err := m.deps.TransitionState(instance, StateRunning); err != nil {
			slog.Error("Failed to transition instance to running", "instanceId", instance.ID, "err", err)
			return err
		}
	}

	// Mark boot volumes as "in-use" now that instance is confirmed running.
	if m.deps.VolumeStateUpdater != nil {
		instance.EBSRequests.Mu.Lock()
		for _, ebsReq := range instance.EBSRequests.Requests {
			if ebsReq.Boot {
				if err := m.deps.VolumeStateUpdater.UpdateVolumeState(ebsReq.Name, "in-use", instance.ID, ""); err != nil {
					slog.Error("Failed to update volume state to in-use", "volumeId", ebsReq.Name, "err", err)
				}
			}
		}
		instance.EBSRequests.Mu.Unlock()
	}

	if m.deps.Hooks.OnInstanceUp != nil {
		// Launch path: per-instance subscribe failures are logged and the
		// launch still succeeds. The instance is reachable via cluster
		// fan-out (DescribeInstances) and the next OnInstanceUp on a
		// state-touching event will reinstall the subs idempotently.
		if err := m.deps.Hooks.OnInstanceUp(instance); err != nil {
			slog.Error("OnInstanceUp hook reported error during launch",
				"instance", instance.ID, "err", err)
		}
	}

	return nil
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
			instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
				Value: fmt.Sprintf("tap,id=mgmt0,ifname=%s,script=no,downscript=no", mgmtTap),
			})
			instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, "mgmt0", instance.MgmtMAC))
			if err := m.appendMgmtNetcfgFwCfg(instance); err != nil {
				return fmt.Errorf("attach mgmt netcfg: %w", err)
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
	client, err := newQMPClientWithHandshake(instance)
	if err != nil {
		return err
	}
	instance.QMPClient = client
	go m.qmpHeartbeat(instance)
	return nil
}

// newQMPClientWithHandshake dials the QMP socket and runs the qmp_capabilities
// handshake. The caller is responsible for starting the heartbeat goroutine.
func newQMPClientWithHandshake(v *VM) (*qmp.QMPClient, error) {
	// QMP socket bind lags the pidfile under recovery load; wait for the
	// socket inode to exist before dialling to avoid an ENOENT race.
	if err := utils.WaitForUnixSocket(v.Config.QMPSocket, 3*time.Second); err != nil {
		return nil, fmt.Errorf("connect QMP socket %s: %w", v.Config.QMPSocket, err)
	}
	client, err := qmp.NewQMPClient(v.Config.QMPSocket)
	if err != nil {
		return nil, fmt.Errorf("connect QMP socket %s: %w", v.Config.QMPSocket, err)
	}
	if _, err := sendQMPCommand(client, qmp.QMPCommand{Execute: "qmp_capabilities"}, v.ID); err != nil {
		_ = client.Conn.Close()
		return nil, err
	}
	slog.Debug("QMP handshake complete", "instance", v.ID)
	return client, nil
}

// qmpHeartbeat sends query-status every 30s. Exits and closes the QMP
// connection when the instance reaches a terminal or transitional state.
func (m *Manager) qmpHeartbeat(instance *VM) {
	for {
		time.Sleep(30 * time.Second)

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
		qmpStatus, err := sendQMPCommand(instance.QMPClient, qmp.QMPCommand{Execute: "query-status"}, instance.ID)
		if err != nil {
			slog.Warn("QMP heartbeat failed", "instance", instance.ID, "err", err)
			continue
		}
		slog.Debug("QMP status", "instance", instance.ID, "status", string(qmpStatus.Return))
	}
}

// sendQMPCommand encodes cmd and decodes the response, skipping event messages.
func sendQMPCommand(q *qmp.QMPClient, cmd qmp.QMPCommand, instanceID string) (*qmp.QMPResponse, error) {
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

	if err := q.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
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
			slog.Info("QMP event", "event", msg["event"], "instanceId", instanceID)
			if err := q.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
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
	fresh, err := qmp.NewQMPClient(q.Path)
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

	if q.Conn != nil {
		_ = q.Conn.Close()
	}
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

// appendMgmtNetcfgFwCfg attaches a one-NIC fw_cfg netcfg blob describing the
// management interface so a BootAMI system VM (e.g. an EKS control-plane node)
// can bring mgmt0 up with its static IP. These guests boot from the Ec2 IMDS
// datasource, which renders only the primary ENI; the mgmt NIC lives on br-mgmt
// with no DHCP, so its address is delivered out-of-band via fw_cfg and applied
// by the system image's mgmt-net init. The blob key format matches
// daemon.buildNetcfgBlob and build/microvm/init.sh; mgmt0 is never the default
// route (NIC0_DEFAULT=0) — that comes from the primary ENI.
func (m *Manager) appendMgmtNetcfgFwCfg(instance *VM) error {
	if instance.MgmtMAC == "" || instance.MgmtIP == "" {
		return nil
	}
	blob := fmt.Sprintf("NIC0_MAC=%s\nNIC0_CIDR=%s/24\nNIC0_DEFAULT=0\n", instance.MgmtMAC, instance.MgmtIP)
	path := filepath.Join(utils.RuntimeDir(), fmt.Sprintf("fwcfg-%s-netcfg.tmp", instance.ID))
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		return fmt.Errorf("write mgmt netcfg blob: %w", err)
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
