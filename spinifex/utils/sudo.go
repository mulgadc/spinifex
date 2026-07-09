package utils

import (
	"context"
	"os"
	"os/exec"
)

// sudoCommand is the private runtime implementation; use SetSudoCommandForTest in tests.
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

// SudoCommandContext is SudoCommand bound to a context so a wedged subprocess is
// killed when the context is cancelled or its deadline elapses.
func SudoCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if os.Getuid() == 0 {
		return exec.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, "sudo", append([]string{name}, args...)...)
}

// SetSudoCommandForTest swaps the command builder for a test, returning a restore func for t.Cleanup.
// Tests must stub this — running against real OVS would mutate the live cluster.
func SetSudoCommandForTest(stub func(name string, args ...string) *exec.Cmd) (restore func()) {
	orig := sudoCommand
	sudoCommand = stub
	return func() { sudoCommand = orig }
}
