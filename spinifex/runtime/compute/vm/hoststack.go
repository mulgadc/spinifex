package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// HostVirtStack names the host software a guest boots on: the kernel that runs
// KVM, the QEMU binary, and the UEFI firmware. A firmware or vCPU hang cannot be
// reproduced without these, and nothing else records them.
type HostVirtStack struct {
	Kernel          string
	QEMU            string
	QEMUVersion     string
	Firmware        string
	FirmwarePackage string
	FirmwareSHA256  string
}

// hostStackCommandTimeout bounds each version probe so a wedged package
// database cannot hold up daemon start.
const hostStackCommandTimeout = 5 * time.Second

var (
	hostStackOnce   sync.Once
	hostStackCached HostVirtStack
)

// CurrentHostVirtStack returns the host stack, probed once per process.
func CurrentHostVirtStack() HostVirtStack {
	hostStackOnce.Do(func() { hostStackCached = detectHostVirtStack(qemuArch(runtime.GOARCH)) })
	return hostStackCached
}

// LogHostVirtStack records the host stack at daemon start.
func LogHostVirtStack() {
	slog.Info("Host virtualization stack", CurrentHostVirtStack().logAttrs()...)
}

func (s HostVirtStack) logAttrs() []any {
	return []any{
		"host_kernel", s.Kernel,
		"qemu_binary", s.QEMU,
		"qemu_version", s.QEMUVersion,
		"firmware_code", s.Firmware,
		"firmware_package", s.FirmwarePackage,
		"firmware_sha256", s.FirmwareSHA256,
	}
}

// qemuArch maps a Go architecture onto the names the firmware and QEMU binary
// tables are keyed by.
func qemuArch(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return goarch
}

func detectHostVirtStack(arch string) HostVirtStack {
	var s HostVirtStack
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		s.Kernel = strings.TrimSpace(string(b))
	}
	if bin, err := resolveQEMUBinary(arch); err == nil {
		s.QEMU = bin
		s.QEMUVersion = firstLine(runHostProbe(bin, "--version"))
	}
	if code, _, _, err := FirmwarePaths(arch); err == nil {
		s.Firmware = code
		s.FirmwarePackage = packageOwning(code)
		s.FirmwareSHA256 = fileSHA256(code)
	}
	return s
}

// packageOwning names the distro package and version that installed path, on
// either packaging system. Empty when neither knows it.
func packageOwning(path string) string {
	if out := firstLine(runHostProbe("dpkg-query", "-S", path)); out != "" {
		pkg, _, _ := strings.Cut(out, ":")
		if ver := firstLine(runHostProbe("dpkg-query", "-W", "-f=${Version}", pkg)); ver != "" {
			return pkg + " " + ver
		}
		return pkg
	}
	return firstLine(runHostProbe("rpm", "-qf", path))
}

func runHostProbe(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), hostStackCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
