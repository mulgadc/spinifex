package utils

import (
	"os"
	"testing"
)

// A tool that needs real privilege is wrapped in sudo when we are not root, and
// invoked directly when we are.
func TestSudoCommand_PrivilegedTool(t *testing.T) {
	cmd := SudoCommand("ip", "link", "show")
	args := cmd.Args

	if os.Getuid() == 0 {
		if args[0] != "ip" {
			t.Errorf("as root, expected args[0]='ip', got %q", args[0])
		}
		return
	}
	if args[0] != "sudo" {
		t.Errorf("as non-root, expected args[0]='sudo', got %q", args[0])
	}
	if args[1] != "ip" {
		t.Errorf("as non-root, expected args[1]='ip', got %q", args[1])
	}
	if len(args) != 4 {
		t.Errorf("expected 4 args [sudo ip link show], got %d: %v", len(args), args)
	}
}

// The OVS/OVN socket clients must never be escalated. Each accepts
// --log-file=PATH, so a NOPASSWD sudoers rule for one — which necessarily takes
// unrestricted args — writes a root-owned file wherever the caller points it.
// They reach their daemons over the group-owned control sockets instead.
func TestSocketClientsAreNotEscalated(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: nothing is escalated, so the policy is not exercised")
	}
	for _, tool := range []string{"ovs-vsctl", "ovs-appctl", "ovn-nbctl", "ovn-sbctl", "ovn-appctl", "systemctl"} {
		if NeedsPrivilege(tool) {
			t.Errorf("%s is escalated; it talks to a group-owned socket, and a sudoers grant for it would be root-equivalent", tool)
		}
		if args := SudoCommand(tool, "--version").Args; len(args) > 0 && args[0] == "sudo" {
			t.Errorf("SudoCommand(%s) built a sudo invocation: %v", tool, args)
		}
	}
}

// The tools that genuinely need privilege keep it. ovs-ofctl is in this list on
// purpose: it talks to a per-bridge <bridge>.mgmt socket that ovs-vswitchd
// creates when the bridge appears — including bridges spinifex creates at
// runtime — so those cannot be group-owned by the provisioning sweep.
func TestPrivilegedToolsStillEscalate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: nothing is escalated, so the policy is not exercised")
	}
	for _, tool := range []string{"ip", "iptables", "dhcpcd", "sysctl", "arping", "ovs-ofctl"} {
		if !NeedsPrivilege(tool) {
			t.Errorf("%s is not escalated, but it needs root or an ambient capability", tool)
		}
		if args := SudoCommand(tool, "--version").Args; len(args) == 0 || args[0] != "sudo" {
			t.Errorf("SudoCommand(%s) did not build a sudo invocation: %v", tool, args)
		}
	}
}
