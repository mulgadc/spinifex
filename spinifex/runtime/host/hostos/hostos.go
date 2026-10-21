// Package hostos resolves the host tools whose names or paths differ between
// host operating systems. Each resolver probes for the tool itself, never reads
// /etc/os-release, and names every candidate it tried when nothing matches.
package hostos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// QEMUKVMPath is where EL-family distributions install QEMU. It only runs
// guests of the host's own architecture.
const QEMUKVMPath = "/usr/libexec/qemu-kvm"

// QEMU guest processes are named qemu-system-<arch> (comm truncates it to 15
// bytes, so qemu-system-x86_64 reads qemu-system-x86) or qemu-kvm.
const (
	qemuSystemPrefix = "qemu-system"
	qemuKVMName      = "qemu-kvm"
	// QEMUProcessPattern is an extended regular expression, for grep -E and
	// pgrep, that matches a QEMU guest's process name or command line.
	QEMUProcessPattern = qemuSystemPrefix + "|" + qemuKVMName
)

// IsQEMUProcess reports whether a /proc/<pid>/comm value names a QEMU guest.
func IsQEMUProcess(comm string) bool {
	comm = strings.TrimSpace(comm)
	return strings.HasPrefix(comm, qemuSystemPrefix) || comm == qemuKVMName
}

// Tool is a host command found by a resolver.
type Tool struct {
	// Name is the candidate that matched, such as "grubby" or "update-grub".
	Name string
	// Path is where it was found on PATH.
	Path string
}

// CATrust is the host's system trust store.
type CATrust struct {
	// Update rebuilds the trust bundle from the anchor directory.
	Update Tool
	// AnchorDir is where locally added CA certificates go.
	AnchorDir string
}

// BootTools are the commands that change the kernel command line and rebuild
// the initramfs.
type BootTools struct {
	Bootloader Tool
	Initramfs  Tool
}

type qemuArch struct {
	binary string
	pkg    string
}

// qemuArches maps the guest architectures the VM config uses to the upstream
// QEMU binary and the Debian package that ships it.
var qemuArches = map[string]qemuArch{
	"x86_64": {binary: "qemu-system-x86_64", pkg: "qemu-system-x86"},
	"arm64":  {binary: "qemu-system-aarch64", pkg: "qemu-system-arm"},
}

var caTrustAnchorDirs = map[string]string{
	"update-ca-certificates": "/usr/local/share/ca-certificates",
	"update-ca-trust":        "/etc/pki/ca-trust/source/anchors",
}

var std = newResolver()

// QEMUBinary returns the QEMU system emulator for guests of arch ("x86_64" or
// "arm64"). QEMUKVMPath wins for host-arch guests, so a stray qemu-system-* on
// PATH never replaces the distro's packaged build.
func QEMUBinary(arch string) (string, error) {
	return std.qemuBinary(arch)
}

// SystemCATrust returns the host's trust-store update command and anchor directory.
func SystemCATrust() (CATrust, error) {
	return std.caTrust()
}

// SystemBootTools returns the host's bootloader and initramfs tools.
func SystemBootTools() (BootTools, error) {
	return std.bootTools()
}

// resolver holds the host lookups behind fields so tests run against a temp
// root and a stubbed PATH. Only successes are cached, so a tool installed
// after a failed lookup is found on the next call.
type resolver struct {
	root     string
	hostArch string
	lookPath func(string) (string, error)

	mu    sync.Mutex
	qemu  map[string]string
	ca    *CATrust
	boots *BootTools
}

func newResolver() *resolver {
	return &resolver{
		hostArch: hostArch(runtime.GOARCH),
		lookPath: exec.LookPath,
		qemu:     map[string]string{},
	}
}

// hostArch maps a GOARCH to the architecture names the VM config uses.
func hostArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "arm64"
	default:
		return goarch
	}
}

func (r *resolver) qemuBinary(arch string) (string, error) {
	want, ok := qemuArches[arch]
	if !ok {
		return "", fmt.Errorf("no QEMU for guest architecture %q: supported architectures are x86_64 and arm64", arch)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if path, ok := r.qemu[arch]; ok {
		return path, nil
	}

	haveKVM := r.isExecutable(QEMUKVMPath)
	if haveKVM && arch == r.hostArch {
		r.qemu[arch] = QEMUKVMPath
		return QEMUKVMPath, nil
	}

	path, err := r.lookPath(want.binary)
	if err == nil {
		r.qemu[arch] = path
		return path, nil
	}
	if haveKVM {
		return "", fmt.Errorf("no QEMU for %s guests: %s (package qemu-kvm) runs only %s guests, and %s (package %s) is not on PATH",
			arch, QEMUKVMPath, r.hostArch, want.binary, want.pkg)
	}
	if arch != r.hostArch {
		return "", fmt.Errorf("no QEMU for %s guests: %s (package %s) is not on PATH", arch, want.binary, want.pkg)
	}
	return "", fmt.Errorf("no QEMU for %s guests: found neither %s (package qemu-kvm) nor %s (package %s) on PATH",
		arch, QEMUKVMPath, want.binary, want.pkg)
}

func (r *resolver) caTrust() (CATrust, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ca != nil {
		return *r.ca, nil
	}
	update, err := r.pickOne("CA trust store update command", "update-ca-certificates", "update-ca-trust")
	if err != nil {
		return CATrust{}, err
	}
	r.ca = &CATrust{Update: update, AnchorDir: caTrustAnchorDirs[update.Name]}
	return *r.ca, nil
}

func (r *resolver) bootTools() (BootTools, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.boots != nil {
		return *r.boots, nil
	}
	bootloader, err := r.pickOne("bootloader tool", "update-grub", "grubby")
	if err != nil {
		return BootTools{}, err
	}
	initramfs, err := r.pickOne("initramfs tool", "update-initramfs", "dracut")
	if err != nil {
		return BootTools{}, err
	}
	r.boots = &BootTools{Bootloader: bootloader, Initramfs: initramfs}
	return *r.boots, nil
}

// pickOne returns the one candidate on PATH. Finding several is an error too:
// a host carrying both tools of a pair cannot tell us which one it uses.
func (r *resolver) pickOne(what string, candidates ...string) (Tool, error) {
	var found []Tool
	for _, name := range candidates {
		if path, err := r.lookPath(name); err == nil {
			found = append(found, Tool{Name: name, Path: path})
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return Tool{}, fmt.Errorf("no %s: found none of %s on PATH", what, strings.Join(candidates, ", "))
	default:
		names := make([]string, len(found))
		for i, t := range found {
			names[i] = fmt.Sprintf("%s (%s)", t.Name, t.Path)
		}
		return Tool{}, fmt.Errorf("ambiguous %s: found %s; cannot tell which one this host uses", what, strings.Join(names, " and "))
	}
}

func (r *resolver) isExecutable(path string) bool {
	info, err := os.Stat(filepath.Join(r.root, path))
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
