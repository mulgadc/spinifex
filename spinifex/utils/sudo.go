package utils

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// Capability bit numbers from linux/capability.h.
const (
	capNetAdmin = 12
	capNetRaw   = 13
)

// capForTool returns the capability that lets this invocation run unescalated.
// The bool is false when no capability substitutes for root, in which case the
// caller must go through sudo.
func capForTool(name string, args []string) (uint, bool) {
	switch name {
	case "ip", "iptables", "ip6tables":
		return capNetAdmin, true
	case "arping":
		return capNetRaw, true
	case "sysctl":
		// Only the net.* trees are governed by CAP_NET_ADMIN in the caller's
		// network namespace; every other key is a root-only write.
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			return capNetAdmin, strings.HasPrefix(a, "net.")
		}
	}
	return 0, false
}

// ambientCaps reports the process's ambient capability set, read once. Tests
// override it.
var ambientCaps = sync.OnceValue(readAmbientCaps)

// readAmbientCaps parses CapAmb from /proc/self/status. The ambient set — not
// the effective one — is what an exec'd child inherits, so it is what decides
// whether `ip` will hold CAP_NET_ADMIN when we run it as ourselves.
func readAmbientCaps() uint64 {
	body, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		hex, ok := strings.CutPrefix(line, "CapAmb:")
		if !ok {
			continue
		}
		caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
		if err != nil {
			return 0
		}
		return caps
	}
	return 0
}

// NeedsPrivilege reports whether a command has to be escalated. False for the
// OVS/OVN socket clients, for anything already running as root, and for tools
// covered by a capability this process holds ambiently.
func NeedsPrivilege(name string, args ...string) bool {
	if os.Getuid() == 0 {
		return false
	}
	if socketClients[name] {
		return false
	}
	// spx is one binary shared by units with different ambient sets: vpcd holds
	// CAP_NET_ADMIN/CAP_NET_RAW and runs these directly, the daemon does not and
	// still needs its sudoers grant. So this is decided at runtime, not by tool.
	if bit, ok := capForTool(name, args); ok && ambientCaps()&(1<<bit) != 0 {
		return false
	}
	return true
}

// sudoCommand is the private runtime implementation; use SetSudoCommandForTest in tests.
var sudoCommand = func(name string, args ...string) *exec.Cmd {
	if !NeedsPrivilege(name, args...) {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

// SudoCommand wraps exec.Command with sudo when the command genuinely needs it.
// The OVS/OVN socket clients never do, and neither does a tool whose capability
// this process already holds.
func SudoCommand(name string, args ...string) *exec.Cmd {
	return sudoCommand(name, args...)
}

// SudoCommandContext is SudoCommand bound to a context so a wedged subprocess is
// killed when the context is cancelled or its deadline elapses.
func SudoCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !NeedsPrivilege(name, args...) {
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
