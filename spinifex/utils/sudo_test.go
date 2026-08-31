package utils

import (
	"os"
	"testing"
)

// withAmbientCaps pins the ambient capability set for one test, so the result
// does not depend on how the test binary itself was launched.
func withAmbientCaps(t *testing.T, caps uint64) {
	t.Helper()
	orig := ambientCaps
	ambientCaps = func() uint64 { return caps }
	t.Cleanup(func() { ambientCaps = orig })
}

// A tool that needs real privilege is wrapped in sudo when we are not root, and
// invoked directly when we are.
func TestSudoCommand_PrivilegedTool(t *testing.T) {
	withAmbientCaps(t, 0)
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
	withAmbientCaps(t, 0)
	for _, tool := range []string{"ip", "iptables", "dhcpcd", "sysctl", "arping", "ping", "ovs-ofctl"} {
		if !NeedsPrivilege(tool, "net.ipv4.neigh.x=1") {
			t.Errorf("%s is not escalated, but it needs root or an ambient capability", tool)
		}
		if args := SudoCommand(tool, "--version").Args; len(args) == 0 || args[0] != "sudo" {
			t.Errorf("SudoCommand(%s) did not build a sudo invocation: %v", tool, args)
		}
	}
}

// vpcd holds CAP_NET_ADMIN and CAP_NET_RAW ambiently, so the kernel hands them
// to each child on exec and these tools work as the service user. This is what
// lets spinifex-vpcd carry no sudoers rules at all.
func TestAmbientCapabilitiesReplaceSudo(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: nothing is escalated, so the policy is not exercised")
	}
	withAmbientCaps(t, 1<<capNetAdmin|1<<capNetRaw)

	unescalated := [][]string{
		{"ip", "addr", "replace", "10.0.0.1/24", "dev", "eth0"},
		{"iptables", "-A", "FORWARD", "-j", "ACCEPT"},
		{"arping", "-U", "-c", "2", "-I", "br-wan", "10.0.0.1"},
		// The nexthop-MAC seed primes the neigh table with this. vpcd holds no
		// sudoers rule for ping, so escalating it kills the fallback outright.
		{"ping", "-c", "1", "-W", "1", "100.127.0.1"},
		{"sysctl", "-w", "net.ipv4.neigh.br-wan.proxy_delay=0"},
		{EndpointSysctlHelper, "ime-12345678", "rp_filter", "0"},
	}
	for _, argv := range unescalated {
		if NeedsPrivilege(argv[0], argv[1:]...) {
			t.Errorf("%v is escalated despite the ambient capability", argv)
		}
	}

	// The capability is per-tool, and for sysctl per-key: CAP_NET_ADMIN governs
	// the net.* trees only, and nothing makes ovs-ofctl's root-owned
	// <bridge>.mgmt socket readable.
	escalated := [][]string{
		{"sysctl", "-w", "vm.swappiness=10"},
		{"ovs-ofctl", "dump-flows", "br-int"},
		{"dhcpcd", "-q", "br-wan"},
	}
	for _, argv := range escalated {
		if !NeedsPrivilege(argv[0], argv[1:]...) {
			t.Errorf("%v is not escalated, but CAP_NET_ADMIN/CAP_NET_RAW do not cover it", argv)
		}
	}
}

// The daemon runs the same spx binary as vpcd but its unit grants no
// CAP_NET_ADMIN, so the decision has to be made from the live capability set
// rather than from the tool name.
func TestDaemonAmbientSetStillEscalatesIP(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: nothing is escalated, so the policy is not exercised")
	}
	const capSysAdmin, capDACOverride = 21, 1
	withAmbientCaps(t, 1<<capSysAdmin|1<<capDACOverride)

	for _, argv := range [][]string{
		{"ip", "tuntap", "add", "tap0", "mode", "tap"},
		{"sysctl", "-qw", "net.ipv4.conf.tap0.rp_filter=0"},
		{EndpointSysctlHelper, "tap0", "rp_filter", "0"},
	} {
		if !NeedsPrivilege(argv[0], argv[1:]...) {
			t.Errorf("%v is not escalated, but the daemon holds no CAP_NET_ADMIN", argv)
		}
	}
}
