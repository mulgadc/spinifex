package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
)

// Debian's qemu-system-x86_64 -machine help, trimmed.
const helpWithMicroVM = `Supported machines are:
microvm              microvm (i386)
pc                   Standard PC (i440FX + PIIX, 1996) (alias of pc-i440fx-10.0)
q35                  Standard PC (Q35 + ICH9, 2009) (alias of pc-q35-10.0)
`

// Oracle Linux 10's AppStream qemu-kvm 10.1.0 -machine help, trimmed. It has
// no microvm row.
const helpWithoutMicroVM = `Supported machines are:
pc                   RHEL 10.0.0 PC (i440FX + PIIX, 1996) (alias of pc-i440fx-rhel10.0.0)
pc-i440fx-rhel10.0.0 RHEL 10.0.0 PC (i440FX + PIIX, 1996) (default) (deprecated)
pc-q35-rhel9.8.0     RHEL-9.8.0 PC (Q35 + ICH9, 2009) (deprecated)
`

var microvmFirmwarePath = filepath.Join(vm.MicroVMDir, vm.MicroVMFirmware)

func TestMain(m *testing.M) {
	microvmChecked = true
	resolveQEMU = func() (string, error) { return hostos.QEMUKVMPath, nil }
	machineHelp = func(string) ([]byte, error) { return []byte(helpWithMicroVM), nil }
	hostnetOVNRemote = func(context.Context) (string, error) { return "", nil }
	unitActive = func(string) bool { return false }
	os.Exit(m.Run())
}

func stubQEMU(t *testing.T, binary string, resolveErr error, help string, helpErr error) {
	t.Helper()
	origResolve, origHelp := resolveQEMU, machineHelp
	t.Cleanup(func() { resolveQEMU, machineHelp = origResolve, origHelp })
	var asked string
	resolveQEMU = func() (string, error) { return binary, resolveErr }
	machineHelp = func(b string) ([]byte, error) {
		asked = b
		return []byte(help), helpErr
	}
	t.Cleanup(func() {
		if resolveErr == nil && asked != binary {
			t.Errorf("machine help ran %q, want the resolved binary %q", asked, binary)
		}
	})
}

func qemuResult(t *testing.T, results []Result) Result {
	t.Helper()
	for _, r := range results {
		if r.Kind == "qemu" {
			return r
		}
	}
	t.Fatalf("no qemu result in %+v", results)
	return Result{}
}

func TestCheckHostAt_QEMUMicroVM(t *testing.T) {
	t.Run("resolved binary lists microvm", func(t *testing.T) {
		stubQEMU(t, hostos.QEMUKVMPath, nil, helpWithMicroVM, nil)
		r := qemuResult(t, CheckHostAt(t.TempDir()))
		if r.Status != OK || r.Path != hostos.QEMUKVMPath {
			t.Errorf("got %+v, want OK for %s", r, hostos.QEMUKVMPath)
		}
	})

	t.Run("resolved binary has no microvm", func(t *testing.T) {
		stubQEMU(t, hostos.QEMUKVMPath, nil, helpWithoutMicroVM, nil)
		r := qemuResult(t, CheckHostAt(t.TempDir()))
		if r.Status != Unsupported || !strings.Contains(r.Detail, "ol10_kvm_utils") {
			t.Errorf("got %+v, want Unsupported naming ol10_kvm_utils", r)
		}
	})

	t.Run("no QEMU resolves", func(t *testing.T) {
		noQEMU := errors.New("no QEMU for x86_64 guests: found neither /usr/libexec/qemu-kvm (package qemu-kvm) nor qemu-system-x86_64 (package qemu-system-x86) on PATH")
		stubQEMU(t, "", noQEMU, "", nil)
		r := qemuResult(t, CheckHostAt(t.TempDir()))
		if r.Status != Missing || r.Detail != noQEMU.Error() {
			t.Errorf("got %+v, want Missing carrying the resolver's error", r)
		}
	})

	t.Run("machine help fails", func(t *testing.T) {
		stubQEMU(t, "/usr/bin/qemu-system-x86_64", nil, "", errors.New("exit status 1"))
		r := qemuResult(t, CheckHostAt(t.TempDir()))
		if r.Status != Missing || !strings.Contains(r.Detail, "exit status 1") {
			t.Errorf("got %+v, want Missing with the exec error", r)
		}
	})
}

func TestCheckHostAt_MicroVMFirmware(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		r := findResult(t, CheckHostAt(t.TempDir()), microvmFirmwarePath)
		if r.Status != Missing {
			t.Errorf("status = %v, want Missing", r.Status)
		}
	})

	t.Run("present", func(t *testing.T) {
		root := t.TempDir()
		writeHelper(t, root, microvmFirmwarePath, []byte("qboot"))
		r := findResult(t, CheckHostAt(root), microvmFirmwarePath)
		if r.Status != OK {
			t.Errorf("status = %v, want OK (detail: %s)", r.Status, r.Detail)
		}
	})
}

func TestCheckHostAt_NoMicroVMChecksOffX86(t *testing.T) {
	orig := microvmChecked
	microvmChecked = false
	t.Cleanup(func() { microvmChecked = orig })

	for _, r := range CheckHostAt(t.TempDir()) {
		if r.Kind == "qemu" || r.Kind == "firmware" {
			t.Errorf("unexpected %s result on a host without system instances: %+v", r.Kind, r)
		}
	}
}
