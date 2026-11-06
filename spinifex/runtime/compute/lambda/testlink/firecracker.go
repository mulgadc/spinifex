//go:build lambdatestlink

package testlink

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// firecrackerArg0 is the argv[0] that turns the spx binary into the
// Firecracker supervisor, so the supervisor is the already-pinned binary.
const firecrackerArg0 = "spx-lambda-firecracker"

// The supervisor inherits the sealed kernel and initramfs as fds 3 and 4,
// which it passes on to Firecracker unchanged, and its ack socket as fd 5.
const (
	fcKernelFD = 3
	fcInitrdFD = 4
	fcAckFD    = 5
	fcAck      = "armed"
)

// firecrackerCmdline is the guest kernel command line; reboot=k lets a guest
// reboot end Firecracker, and Firecracker adds the virtio-mmio devices itself.
const firecrackerCmdline = "console=ttyS0 quiet loglevel=3 reboot=k panic=1 pci=off"

const (
	configArchive = "config.tar"
	fcConfigFile  = "firecracker.json"
	// fcStopWait bounds proving Firecracker gone after its supervisor stopped.
	fcStopWait = 5 * time.Second
)

func init() {
	if filepath.Base(os.Args[0]) == firecrackerArg0 {
		os.Exit(supervise(os.Args[1:]))
	}
}

// supervise arms a CLOCK_BOOTTIME SIGKILL on itself at the absolute deadline,
// then starts Firecracker with SIGKILL as its parent-death signal, so the
// kernel ends both at the deadline even if this process and the daemon stop.
func supervise(args []string) int {
	if len(args) != 3 {
		return 2
	}
	deadline, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return 2
	}
	unix.CloseOnExec(fcAckFD)
	// The parent-death signal follows the forking thread, which never exits.
	runtime.LockOSThread()
	if err := armBoottimeKill(time.Duration(deadline)); err != nil {
		fmt.Fprintln(os.Stderr, "firecracker supervisor:", err)
		return 3
	}
	pidfd := -1
	cmd := exec.Command(args[0], "--no-api", "--config-file", args[1], "--level", "Warning")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(fcKernelFD, "kernel"), os.NewFile(fcInitrdFD, "initrd")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, PidFD: &pidfd}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "firecracker supervisor: start:", err)
		return 4
	}
	if err := unix.Sendmsg(fcAckFD, []byte(fcAck), unix.UnixRights(pidfd), nil, 0); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 5
	}
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 6
	}
	return 0
}

// sigevent is the kernel's struct sigevent on 64-bit Linux.
type sigevent struct {
	value  uintptr
	signo  int32
	notify int32
	_      [48]byte
}

const (
	sigevSignal  = 0
	timerAbstime = 1
)

// armBoottimeKill arms a POSIX timer that delivers SIGKILL to this process at
// the absolute CLOCK_BOOTTIME deadline; a passed deadline fires at once.
func armBoottimeKill(deadline time.Duration) error {
	ev := sigevent{signo: int32(unix.SIGKILL), notify: sigevSignal}
	var id int32
	if _, _, e := unix.Syscall(unix.SYS_TIMER_CREATE, unix.CLOCK_BOOTTIME,
		uintptr(unsafe.Pointer(&ev)), uintptr(unsafe.Pointer(&id))); e != 0 {
		return fmt.Errorf("timer_create: %w", e)
	}
	spec := unix.ItimerSpec{Value: unix.NsecToTimespec(int64(deadline))}
	if _, _, e := unix.Syscall6(unix.SYS_TIMER_SETTIME, uintptr(id), timerAbstime,
		uintptr(unsafe.Pointer(&spec)), 0, 0, 0); e != 0 {
		return fmt.Errorf("timer_settime: %w", e)
	}
	return nil
}

// supervisorCommand builds the supervisor for one attempt: the spx binary
// under firecrackerArg0, the sealed kernel and initramfs, and the ack socket.
func (d *Daemon) supervisorCommand(configPath string, deadline time.Duration, ack *os.File) (*exec.Cmd, error) {
	if d.Image == nil || d.Image.Kernel == nil || d.Image.Initrd == nil {
		return nil, errors.New("runtime image not prepared")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, d.Firecracker, configPath, strconv.FormatInt(int64(deadline), 10))
	cmd.Args[0] = firecrackerArg0
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{d.Image.Kernel, d.Image.Initrd, ack}
	return cmd, nil
}

// ackPair is the socket the supervisor acks on; the daemon keeps one end.
func ackPair() (daemonEnd *net.UnixConn, child *os.File, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	child = os.NewFile(uintptr(fds[1]), "fc-ack")
	f := os.NewFile(uintptr(fds[0]), "fc-ack-daemon")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return c.(*net.UnixConn), child, nil
}

// awaitFirecracker reads the supervisor's ack and the pidfd it carries, and
// checks that pidfd names a live child of the supervisor.
func awaitFirecracker(ctx context.Context, ack *net.UnixConn, supervisor int, wait time.Duration) (pidfd, pid int, err error) {
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ack.SetReadDeadline(deadline); err != nil {
		return -1, 0, err
	}
	buf := make([]byte, 16)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := ack.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, 0, fmt.Errorf("%w: %w", ErrNotArmed, err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return -1, 0, fmt.Errorf("%w: no pidfd in ack", ErrNotArmed)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return -1, 0, fmt.Errorf("%w: no pidfd in ack", ErrNotArmed)
	}
	pidfd = fds[0]
	unix.CloseOnExec(pidfd)
	if string(buf[:n]) != fcAck {
		_ = unix.Close(pidfd)
		return -1, 0, fmt.Errorf("%w: unexpected ack", ErrNotArmed)
	}
	if pid, err = pidfdPID(pidfd); err == nil {
		err = checkParent(pid, supervisor)
	}
	if err != nil {
		_ = unix.Close(pidfd)
		return -1, 0, fmt.Errorf("%w: %w", ErrNotArmed, err)
	}
	return pidfd, pid, nil
}

// pidfdPID reads the PID a pidfd refers to from its fdinfo.
func pidfdPID(pidfd int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", pidfd))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Pid:"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("pidfd names no live process (%q)", strings.TrimSpace(v))
			}
			return pid, nil
		}
	}
	return 0, errors.New("pidfd fdinfo has no Pid")
}

func checkParent(pid, parent int) error {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PPid:"); ok {
			if strings.TrimSpace(v) != strconv.Itoa(parent) {
				return fmt.Errorf("process %d is not the supervisor's child", pid)
			}
			return nil
		}
	}
	return errors.New("process status has no PPid")
}

// stopFirecracker kills Firecracker through its pidfd and reports whether its
// exit was proven; the supervisor's own exit proves nothing about it.
func stopFirecracker(pidfd int) bool {
	if pidfd < 0 {
		return false
	}
	_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
	exited, err := pidfdExited(pidfd, fcStopWait)
	return err == nil && exited
}

// writeConfigArchive writes entries as a tar archive in the fw_cfg sysfs
// layout, by_name/<entry>/raw, which the guest's init extracts.
func writeConfigArchive(path string, entries map[string][]byte) error {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, name := range names {
		file := "by_name/" + name + "/raw"
		var parents []string
		for dir := filepath.Dir(file); dir != "." && !dirs[dir]; dir = filepath.Dir(dir) {
			parents = append(parents, dir)
			dirs[dir] = true
		}
		for i := len(parents) - 1; i >= 0; i-- {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: parents[i] + "/", Mode: 0o700}); err != nil {
				return err
			}
		}
		b := entries[name]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: file, Mode: 0o600, Size: int64(len(b))}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

type fcConfig struct {
	BootSource struct {
		Kernel   string `json:"kernel_image_path"`
		Initrd   string `json:"initrd_path"`
		BootArgs string `json:"boot_args"`
	} `json:"boot-source"`
	Drives  []fcDrive `json:"drives"`
	Machine struct {
		VCPUs  int  `json:"vcpu_count"`
		MemMiB int  `json:"mem_size_mib"`
		SMT    bool `json:"smt"`
	} `json:"machine-config"`
	Net []fcNet `json:"network-interfaces"`
}

type fcDrive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
}

type fcNet struct {
	ID  string `json:"iface_id"`
	MAC string `json:"guest_mac"`
	Tap string `json:"host_dev_name"`
}

// writeFirecrackerConfig writes the attempt's Firecracker config: the sealed
// kernel and initramfs, the package disk then the configuration disk, and the tap.
func writeFirecrackerConfig(path, pkg, cfgDisk, tap, mac string, memMiB int) error {
	var c fcConfig
	c.BootSource.Kernel = fmt.Sprintf("/proc/self/fd/%d", fcKernelFD)
	c.BootSource.Initrd = fmt.Sprintf("/proc/self/fd/%d", fcInitrdFD)
	c.BootSource.BootArgs = firecrackerCmdline
	c.Drives = []fcDrive{{ID: "pkg", Path: pkg, ReadOnly: true}, {ID: "cfg", Path: cfgDisk}}
	c.Machine.VCPUs, c.Machine.MemMiB = 1, memMiB
	c.Net = []fcNet{{ID: "net0", MAC: mac, Tap: tap}}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// vmHandle owns Firecracker's pidfd across the session callback, which can
// still be running when attempt.Run returns; whichever side finishes last
// closes an abandoned fd.
type vmHandle struct {
	mu        sync.Mutex
	fd        int
	finished  bool
	abandoned bool
}

func (h *vmHandle) set(fd int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fd = fd
}

// callbackDone runs as the callback returns.
func (h *vmHandle) callbackDone() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = true
	h.closeIfAbandoned()
}

// abandon gives up the fd while the callback may still run.
func (h *vmHandle) abandon() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.abandoned = true
	h.closeIfAbandoned()
}

func (h *vmHandle) closeIfAbandoned() {
	if h.abandoned && h.finished && h.fd >= 0 {
		_ = unix.Close(h.fd)
		h.fd = -1
	}
}

// take hands the fd to teardown once the callback has returned.
func (h *vmHandle) take() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	fd := h.fd
	h.fd = -1
	return fd
}
