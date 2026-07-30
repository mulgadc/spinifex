package utils

import (
	"context"
	"os"
	"os/exec"
)

// socketClients are the OVS/OVN client tools that do all their work over a
// control socket, not through privileged syscalls. setup-ovn.sh group-owns
// those sockets to `spinifex` (0660), so the service users reach them as
// themselves and escalating buys nothing.
//
// Escalating actively costs: a sudoers rule for these takes unrestricted args,
// and every one of them accepts --log-file=PATH, which writes a root-owned file
// wherever the caller points it. A NOPASSWD grant for any of them is therefore a
// root-equivalent grant handed out to read a status.
//
// ovs-ofctl is deliberately absent: it talks to a per-bridge
// /var/run/openvswitch/<bridge>.mgmt socket created by ovs-vswitchd when the
// bridge appears — including bridges spinifex creates at runtime, long after
// the provisioning sweep — so those sockets cannot be group-owned up front.
var socketClients = map[string]bool{
	"ovs-vsctl":  true,
	"ovs-appctl": true,
	"ovn-nbctl":  true,
	"ovn-sbctl":  true,
	"ovn-appctl": true,
	// systemctl is-active is a read of the system bus, allowed unprivileged.
	"systemctl": true,
}

// NeedsPrivilege reports whether a command has to be escalated. False for the
// OVS/OVN socket clients and for anything already running as root.
func NeedsPrivilege(name string) bool {
	if os.Getuid() == 0 {
		return false
	}
	return !socketClients[name]
}

// sudoCommand is the private runtime implementation; use SetSudoCommandForTest in tests.
var sudoCommand = func(name string, args ...string) *exec.Cmd {
	if !NeedsPrivilege(name) {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

// SudoCommand wraps exec.Command with sudo when the command genuinely needs it.
// ip/iptables/dhcpcd and friends still do; the OVS/OVN socket clients do not.
func SudoCommand(name string, args ...string) *exec.Cmd {
	return sudoCommand(name, args...)
}

// SudoCommandContext is SudoCommand bound to a context so a wedged subprocess is
// killed when the context is cancelled or its deadline elapses.
func SudoCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !NeedsPrivilege(name) {
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
