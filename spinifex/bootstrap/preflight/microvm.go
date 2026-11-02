package preflight

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
)

// microvmMachine is the QEMU machine type load balancer system instances boot on.
const microvmMachine = "microvm"

// microvmChecked reports whether this host runs system instances. The microvm
// machine type exists only in x86 QEMU.
var microvmChecked = runtime.GOARCH == "amd64"

// resolveQEMU and machineHelp are variables so tests never run a real QEMU.
var (
	resolveQEMU = func() (string, error) { return hostos.QEMUBinary("x86_64") }
	machineHelp = func(binary string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, binary, "-machine", "help").Output()
	}
)

// checkMicroVM checks the QEMU binary the daemon will run, and the firmware it
// passes, can boot a microvm. A QEMU build without microvm, such as Oracle
// Linux's AppStream qemu-kvm, otherwise surfaces only at the first ALB launch.
func checkMicroVM(root string) []Result {
	if !microvmChecked {
		return nil
	}
	return []Result{checkQEMUMicroVM(), checkMicroVMFirmware(root)}
}

func checkQEMUMicroVM() Result {
	binary, err := resolveQEMU()
	if err != nil {
		return Result{Path: "qemu", Kind: "qemu", Status: Missing, Detail: err.Error()}
	}
	out, err := machineHelp(binary)
	if err != nil {
		return Result{Path: binary, Kind: "qemu", Status: Missing, Detail: fmt.Sprintf("%s -machine help: %v", binary, err)}
	}
	if !listsMachine(out, microvmMachine) {
		return Result{
			Path:   binary,
			Kind:   "qemu",
			Status: Unsupported,
			Detail: fmt.Sprintf("no %s machine type, so load balancers cannot boot; install a QEMU build that has it (on Oracle Linux, qemu-kvm from ol10_kvm_utils)", microvmMachine),
		}
	}
	return Result{Path: binary, Kind: "qemu", Status: OK}
}

// listsMachine reports whether `-machine help` output has a row for name. Rows
// are "<name> <description>", after a "Supported machines are:" header.
func listsMachine(help []byte, name string) bool {
	sc := bufio.NewScanner(bytes.NewReader(help))
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func checkMicroVMFirmware(root string) Result {
	path := filepath.Join(vm.MicroVMDir, vm.MicroVMFirmware)
	if _, err := os.Stat(filepath.Join(root, path)); err != nil {
		return Result{Path: path, Kind: "firmware", Status: Missing, Detail: err.Error()}
	}
	return Result{Path: path, Kind: "firmware", Status: OK}
}
