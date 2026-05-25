package utils

import (
	"os"
	"os/exec"
)

// sudoCommand is the runtime implementation. Kept private so tests in other
// packages can't reassign it directly — reassign linter rule. Use
// SetSudoCommandForTest from tests.
var sudoCommand = func(name string, args ...string) *exec.Cmd {
	if os.Getuid() == 0 {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

// SudoCommand wraps exec.Command with sudo when running as non-root.
// OVS/OVN/ip require CAP_NET_ADMIN; production daemons run as root, but
// dev environments may not.
func SudoCommand(name string, args ...string) *exec.Cmd {
	return sudoCommand(name, args...)
}

// SetSudoCommandForTest swaps the underlying command builder for the duration
// of a test, returning a restore func that callers pass to t.Cleanup. The
// indirection prevents cross-package reassignment of an exported var, which
// the reassign linter flags. Running the live binary against the dev host's
// OVS would mutate external_ids on the running cluster, so tests must stub.
func SetSudoCommandForTest(stub func(name string, args ...string) *exec.Cmd) (restore func()) {
	orig := sudoCommand
	sudoCommand = stub
	return func() { sudoCommand = orig }
}
