package vm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
)

// Run launches a VM through the manager's lifecycle pipeline: validate state,
// mount volumes, exec QEMU, attach QMP, transition to Running, fire
// OnInstanceUp. The instance need not be in the manager's map yet — Run
// inserts it before transitioning. Used by RunInstances, the start-stopped
// handler, restore, and crash recovery.
func (m *Manager) Run(instance *VM) error {
	return m.launch(instance)
}

// Start re-launches a stopped instance held in the manager's map by id. Used
// by the EC2 StartInstances handler when the instance is already local.
func (m *Manager) Start(id string) error {
	instance, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("instance %s not found", id)
	}
	return m.launch(instance)
}

// Reboot issues a QMP system_reset to a running instance. The VM stays
// in StateRunning across the reset; QEMU re-runs firmware and the guest
// kernel reboots in place. Returns ErrInstanceNotFound when id is unknown
// and ErrInvalidTransition when the instance is not Running.
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

// launchStillValid returns true while the launch pipeline may continue
// setting up resources for instance. Returns false if a concurrent terminate
// has flipped status out of pending/stopped/provisioning — at that point the
// terminate goroutine owns cleanup and launch must bail without further side
// effects.
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

	// Re-check status — Mount can take 30+s on cold AMIs (NBD clone), and a
	// terminate can race in during that window. Skip the remaining setup so
	// the concurrent terminate goroutine doesn't fight SetupTapDevice and leak
	// resources.
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
		return err
	}
	instance.QMPClient = qmpClient
	go m.qmpHeartbeat(instance)

	m.Insert(instance)

	// Final race check — QEMU is up, but if a terminate fired during start /
	// QMP attach, the concurrent goroutine has already transitioned status to
	// shutting-down. Attempting StateRunning here would log a spurious error;
	// let the terminate cleanup own teardown.
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

	instance.Config = buildBaseVMConfig(instance.ID, pidFile, consoleLogPath, serialSocket, spec.Architecture, spec.VCPUs, spec.MemoryMiB)

	instance.EBSRequests.Mu.Lock()
	drives, iothreads, devices, err := buildDrives(instance.EBSRequests.Requests, spec.VCPUs)
	instance.EBSRequests.Mu.Unlock()
	if err != nil {
		return err
	}
	instance.Config.Drives = append(instance.Config.Drives, drives...)
	instance.Config.IOThreads = append(instance.Config.IOThreads, iothreads...)
	instance.Config.Devices = append(instance.Config.Devices, devices...)

	if instance.ENIId != "" && m.deps.NetworkPlumber != nil {
		spec := VPCTapSpec(instance.ENIId, instance.ENIMac)
		if err := m.deps.NetworkPlumber.SetupTap(spec); err != nil {
			slog.Error("Failed to set up tap device", "eni", instance.ENIId, "err", err)
			return fmt.Errorf("setup tap device: %w", err)
		}
		tapName := spec.Name

		instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
			Value: fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", tapName),
		})
		instance.Config.Devices = append(instance.Config.Devices, Device{
			Value: fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", instance.ENIMac),
		})
		slog.Info("VPC networking configured", "tap", tapName, "eni", instance.ENIId, "mac", instance.ENIMac)

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
		instance.Config.Devices = append(instance.Config.Devices, Device{
			Value: "virtio-net-pci,netdev=net0",
		})
	}

	if instance.MgmtMAC != "" {
		mgmtTap := MgmtTapName(instance.ID)
		instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
			Value: fmt.Sprintf("tap,id=mgmt0,ifname=%s,script=no,downscript=no", mgmtTap),
		})
		instance.Config.Devices = append(instance.Config.Devices, Device{
			Value: fmt.Sprintf("virtio-net-pci,netdev=mgmt0,mac=%s", instance.MgmtMAC),
		})
		slog.Info("Management NIC configured", "tap", mgmtTap, "mac", instance.MgmtMAC, "ip", instance.MgmtIP, "instanceId", instance.ID)
	}

	instance.Config.Devices = append(instance.Config.Devices, Device{Value: "virtio-rng-pci"})

	if instance.GPUPCIAddress != "" {
		xvga := "off"
		if instance.GPUXVGAEnabled {
			xvga = "on"
		}
		instance.Config.Devices = append(instance.Config.Devices, Device{
			Value: fmt.Sprintf("vfio-pci,host=%s,id=gpu0,x-vga=%s", instance.GPUPCIAddress, xvga),
		})
		slog.Info("GPU passthrough device configured",
			"pci", instance.GPUPCIAddress, "instanceId", instance.ID, "xvga", xvga)
	}

	qmpSocket, err := utils.GenerateSocketFile(fmt.Sprintf("qmp-%s", instance.ID))
	if err != nil {
		slog.Error("Failed to generate QMP socket", "err", err)
		return err
	}
	instance.Config.QMPSocket = qmpSocket

	// Wait briefly for nbdkit to start.
	// TODO: Improve, confirm nbdkit started for each volume.
	time.Sleep(2 * time.Second)

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

		if err := utils.SetOOMScore(cmd.Process.Pid, 500); err != nil {
			slog.Warn("Failed to set QEMU OOM score", "pid", cmd.Process.Pid, "err", err)
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

	time.Sleep(1 * time.Second)

	select {
	case exitErr := <-exitChan:
		startupConfirmed <- false
		if exitErr != 0 {
			errorMsg := fmt.Errorf("failed: %v", exitErr)
			slog.Error("Failed to launch qemu", "err", errorMsg)
			return errorMsg
		}
	default:
		startupConfirmed <- true
		slog.Info("QEMU started successfully and is running",
			"console_log", instance.Config.ConsoleLogPath,
			"serial_socket", instance.Config.SerialSocket)
	}

	if _, err := utils.ReadPidFile(instance.ID); err != nil {
		slog.Error("Failed to read PID file", "err", err)
		return err
	}

	return nil
}

// appendDevHostfwdNIC adds a user-mode NIC with SSH hostfwd for dev access.
func (m *Manager) appendDevHostfwdNIC(instance *VM) {
	sshDebugAddr, err := viperblock.FindFreePort()
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
	netdevVal := fmt.Sprintf("user,id=dev0,hostfwd=tcp:%s:%s-:22", bindIP, sshDebugPort)

	if instance.ExtraHostfwd != nil {
		for guestPort := range instance.ExtraHostfwd {
			fwdAddr, fwdErr := viperblock.FindFreePort()
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
			netdevVal += fmt.Sprintf(",hostfwd=tcp:%s:%s-:%d", bindIP, hostPort, guestPort)
			instance.ExtraHostfwd[guestPort] = hostPortInt
			slog.Info("DEV_NETWORKING: extra hostfwd", "guestPort", guestPort, "hostPort", hostPort, "instanceId", instance.ID)
		}
	}

	instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{Value: netdevVal})
	devMac := GenerateDevMAC(instance.ID)
	instance.Config.Devices = append(instance.Config.Devices, Device{
		Value: fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s", devMac),
	})
	slog.Info("DEV_NETWORKING: added dev NIC with SSH hostfwd",
		"bindIP", bindIP, "port", sshDebugPort, "mac", devMac, "instanceId", instance.ID)
}

// AttachQMP connects a QMP client to a QEMU process that already exists
// (e.g. a daemon restart finding a still-running QEMU) and starts the
// heartbeat goroutine. AttachQMP is the matching seam for reconnect
// callers; the launch path calls the same helper inline.
func (m *Manager) AttachQMP(instance *VM) error {
	client, err := newQMPClientWithHandshake(instance)
	if err != nil {
		return err
	}
	instance.QMPClient = client
	go m.qmpHeartbeat(instance)
	return nil
}

// newQMPClientWithHandshake dials the instance's QMP socket and runs the
// qmp_capabilities handshake. The caller owns starting the heartbeat
// goroutine on the returned client.
func newQMPClientWithHandshake(v *VM) (*qmp.QMPClient, error) {
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

// qmpHeartbeat sends a query-status QMP command every 30s to confirm the
// QEMU process is still healthy. Exits when the instance reaches a terminal
// or transitional state. Closes the QMP connection on exit.
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

// sendQMPCommand encodes cmd onto client and decodes the matching response,
// skipping informational events.
func sendQMPCommand(q *qmp.QMPClient, cmd qmp.QMPCommand, instanceID string) (*qmp.QMPResponse, error) {
	if q == nil || q.Encoder == nil || q.Decoder == nil {
		return nil, fmt.Errorf("QMP client is not initialized")
	}

	q.Mu.Lock()
	defer q.Mu.Unlock()

	if err := q.Conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = q.Conn.SetReadDeadline(time.Time{}) }()

	if err := q.Encoder.Encode(cmd); err != nil {
		return nil, fmt.Errorf("encode error: %w", err)
	}

	for {
		var msg map[string]any
		if err := q.Decoder.Decode(&msg); err != nil {
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

// buildBaseVMConfig creates a vm.Config with base QEMU settings and PCIe
// hotplug root ports.
func buildBaseVMConfig(instanceID, pidFile, consoleLogPath, serialSocket, architecture string, vCPUs, memoryMiB int) Config {
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
	}
	// 11 PCIe root ports for /dev/sd[f-p] hotplug slots, starting at chassis 1.
	for i := 1; i <= 11; i++ {
		cfg.Devices = append(cfg.Devices, Device{
			Value: fmt.Sprintf("pcie-root-port,id=hotplug%d,chassis=%d,slot=0", i, i),
		})
	}
	return cfg
}

// buildDrives converts EBS volume requests into QEMU drive, iothread, and
// device configurations. Returns an error if any non-EFI volume is missing
// its NBDURI.
func buildDrives(requests []types.EBSRequest, cpuCount int) ([]Drive, []IOThread, []Device, error) {
	var drives []Drive
	var iothreads []IOThread
	var devices []Device

	for _, v := range requests {
		// TODO: Add EFI support
		if v.EFI {
			continue
		}
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
			devices = append(devices, Device{
				Value: fmt.Sprintf("virtio-blk-pci,drive=%s,iothread=%s,num-queues=%d,bootindex=1",
					drive.ID, iothreadID, cpuCount),
			})
		}

		if v.CloudInit {
			drive.Format = "raw"
			drive.If = "virtio"
			drive.Media = "cdrom"
			drive.ID = "cloudinit"
		}

		slog.Info("Using NBD URI for drive", "volume", v.Name, "uri", v.NBDURI)
		drives = append(drives, drive)
	}

	return drives, iothreads, devices, nil
}
