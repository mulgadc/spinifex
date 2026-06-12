package host

import (
	"context"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

const testSubnetID = "subnet-0123456789abcdef" // short form: "89abcdef"

var testSubnetCIDR = netip.MustParsePrefix("10.211.0.0/16")

func recordSudo(t *testing.T, fail func(name string, args []string) *exec.Cmd) *[][]string {
	t.Helper()
	var calls [][]string
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		if fail != nil {
			if c := fail(name, args); c != nil {
				return c
			}
		}
		return exec.Command("/bin/true")
	}))
	return &calls
}

func TestEnsureIMDSVeth_HappyPath(t *testing.T) {
	// Probe must report "not a port" so creation proceeds.
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		return nil
	})

	netns, hostEnd, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR)
	if err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}
	if netns != "imds-89abcdef" {
		t.Errorf("netns = %q, want imds-89abcdef", netns)
	}
	if hostEnd != "imds-h-89abcdef" {
		t.Errorf("hostEnd = %q, want imds-h-89abcdef", hostEnd)
	}

	want := [][]string{
		{"ovs-vsctl", "port-to-br", "imds-o-89abcdef"},
		{"ip", "netns", "add", "imds-89abcdef"},
		{"ip", "link", "add", "imds-o-89abcdef", "type", "veth", "peer", "name", "imds-h-89abcdef"},
		{"ip", "link", "set", "imds-o-89abcdef", "up"},
		{"ip", "link", "set", "imds-h-89abcdef", "netns", "imds-89abcdef"},
		{"ip", "-n", "imds-89abcdef", "link", "set", "imds-h-89abcdef", "address", utils.HashMAC("imds-" + testSubnetID)},
		{"ip", "-n", "imds-89abcdef", "link", "set", "lo", "up"},
		{"ip", "-n", "imds-89abcdef", "link", "set", "imds-h-89abcdef", "up"},
		{"ip", "-n", "imds-89abcdef", "addr", "add", "169.254.169.254/30", "dev", "imds-h-89abcdef"},
		{"ip", "-n", "imds-89abcdef", "route", "add", "10.211.0.0/16", "dev", "imds-h-89abcdef"},
		{"ovs-vsctl", "add-port", "br-int", "imds-o-89abcdef",
			"--", "set", "Interface", "imds-o-89abcdef",
			"external_ids:iface-id=imds-port-" + testSubnetID},
	}
	if len(*calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(*calls), len(want), *calls)
	}
	for i := range want {
		if !slices.Equal((*calls)[i], want[i]) {
			t.Errorf("call %d = %v, want %v", i, (*calls)[i], want[i])
		}
	}
}

func TestEnsureIMDSVeth_Idempotent(t *testing.T) {
	// Probe reports the OVS end is already on br-int and the netns is enterable
	// (default mock = /bin/true) → early return, no mutation.
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("echo", "br-int")
		}
		return nil
	})

	netns, hostEnd, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR)
	if err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}
	if netns != "imds-89abcdef" {
		t.Errorf("netns = %q, want imds-89abcdef", netns)
	}
	if hostEnd != "imds-h-89abcdef" {
		t.Errorf("hostEnd = %q, want imds-h-89abcdef", hostEnd)
	}
	want := [][]string{
		{"ovs-vsctl", "port-to-br", "imds-o-89abcdef"},
		{"ip", "-n", "imds-89abcdef", "link", "show", "lo"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("expected only the two probe calls, got %v", *calls)
	}
	for i := range want {
		if !slices.Equal((*calls)[i], want[i]) {
			t.Errorf("call %d = %v, want %v", i, (*calls)[i], want[i])
		}
	}
}

// TestEnsureIMDSVeth_StaleNetnsBehindLivePort_Rebuilds guards against a live OVS
// port with an unenterable netns (setns EINVAL): plumbing must be torn down
// and rebuilt so the IMDS listener is bindable rather than wedging forever.
func TestEnsureIMDSVeth_StaleNetnsBehindLivePort_Rebuilds(t *testing.T) {
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("echo", "br-int")
		}
		// Netns enterability probe fails with the kernel's EINVAL message.
		if name == "ip" && len(args) >= 5 && args[0] == "-n" && args[2] == "link" && args[3] == "show" {
			return exec.Command("sh", "-c", `echo 'setting the network namespace "imds-89abcdef" failed: Invalid argument' >&2; exit 1`)
		}
		return nil
	})

	if _, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR); err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}

	// Teardown (removeIMDSPlumbing) must run before the rebuild, and the rebuild
	// must re-add the OVS port — proving we did not short-circuit.
	var sawDelPort, sawNetnsDel, sawAddPort bool
	for _, c := range *calls {
		if c[0] == "ovs-vsctl" && slices.Contains(c, "del-port") {
			sawDelPort = true
		}
		if c[0] == "ip" && len(c) >= 3 && c[1] == "netns" && c[2] == "del" {
			sawNetnsDel = true
		}
		if c[0] == "ovs-vsctl" && slices.Contains(c, "add-port") {
			sawAddPort = true
		}
	}
	if !sawDelPort || !sawNetnsDel || !sawAddPort {
		t.Errorf("expected stale plumbing torn down and rebuilt (del-port=%v, netns del=%v, add-port=%v); calls=%v",
			sawDelPort, sawNetnsDel, sawAddPort, *calls)
	}
}

// TestEnsureIMDSVeth_StaleNetnsRecreated guards Fix #1: when `ip netns add`
// reports "File exists" but the handle is unenterable, ensureNetns deletes the
// stale handle and recreates it, then plumbing proceeds to completion.
func TestEnsureIMDSVeth_StaleNetnsRecreated(t *testing.T) {
	netnsAdds := 0
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		if name == "ip" && len(args) >= 2 && args[0] == "netns" && args[1] == "add" {
			netnsAdds++
			if netnsAdds == 1 {
				// First add: name already present as a stale handle.
				return exec.Command("sh", "-c", "echo 'Cannot create namespace file: File exists' >&2; exit 1")
			}
			return nil // recreate succeeds
		}
		// Enterability probe fails → stale handle detected.
		if name == "ip" && len(args) >= 5 && args[0] == "-n" && args[2] == "link" && args[3] == "show" {
			return exec.Command("sh", "-c", `echo 'setting the network namespace "imds-89abcdef" failed: Invalid argument' >&2; exit 1`)
		}
		return nil
	})

	if _, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR); err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}

	// Stale handle deleted then re-added, and the rebuild ran to the add-port.
	var sawNetnsDel, sawAddPort bool
	for _, c := range *calls {
		if c[0] == "ip" && len(c) >= 3 && c[1] == "netns" && c[2] == "del" {
			sawNetnsDel = true
		}
		if c[0] == "ovs-vsctl" && slices.Contains(c, "add-port") {
			sawAddPort = true
		}
	}
	if !sawNetnsDel {
		t.Errorf("expected stale netns to be deleted before recreate; calls=%v", *calls)
	}
	if !sawAddPort {
		t.Errorf("expected plumbing to complete after recreate; calls=%v", *calls)
	}
	if netnsAdds != 2 {
		t.Errorf("expected two netns add attempts (stale + recreate), got %d", netnsAdds)
	}
}

// TestEnsureIMDSVeth_NetnsExistsTolerated guards idempotent re-runs after a
// partial prior attempt: `ip netns add` reporting "File exists" must not abort.
func TestEnsureIMDSVeth_NetnsExistsTolerated(t *testing.T) {
	recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		if name == "ip" && len(args) >= 2 && args[0] == "netns" && args[1] == "add" {
			return exec.Command("sh", "-c", "echo 'Cannot create namespace file: File exists' >&2; exit 1")
		}
		return nil
	})

	if _, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR); err != nil {
		t.Fatalf("expected nil for pre-existing netns, got %v", err)
	}
}

func TestEnsureIMDSVeth_AddPortFailureCleansUp(t *testing.T) {
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "add-port" {
			return exec.Command("/bin/false")
		}
		return nil
	})

	_, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR)
	if err == nil || !strings.Contains(err.Error(), "add IMDS veth") {
		t.Fatalf("expected add-port error, got %v", err)
	}
	// Cleanup must have run: del-port, netns del, then link del.
	var sawDelPort, sawNetnsDel, sawLinkDel bool
	for _, c := range *calls {
		if c[0] == "ovs-vsctl" && slices.Contains(c, "del-port") {
			sawDelPort = true
		}
		if c[0] == "ip" && len(c) >= 3 && c[1] == "netns" && c[2] == "del" {
			sawNetnsDel = true
		}
		if c[0] == "ip" && len(c) >= 3 && c[1] == "link" && c[2] == "del" {
			sawLinkDel = true
		}
	}
	if !sawDelPort || !sawNetnsDel || !sawLinkDel {
		t.Errorf("expected cleanup (del-port=%v, netns del=%v, link del=%v); calls=%v", sawDelPort, sawNetnsDel, sawLinkDel, *calls)
	}
}

func TestEnsureIMDSVeth_LinkAddFailureSurfaces(t *testing.T) {
	recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		if name == "ip" && len(args) >= 3 && args[0] == "link" && args[1] == "add" {
			return exec.Command("/bin/false")
		}
		return nil
	})

	_, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR)
	if err == nil || !strings.Contains(err.Error(), "create IMDS veth pair") {
		t.Fatalf("expected create error, got %v", err)
	}
}

// TestEnsureIMDSVeth_AddrFailureCleansUp asserts a failure assigning the IMDS
// address inside the netns surfaces and tears the half-built plumbing down.
func TestEnsureIMDSVeth_AddrFailureCleansUp(t *testing.T) {
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("/bin/false")
		}
		if name == "ip" && len(args) >= 4 && args[0] == "-n" && args[2] == "addr" && args[3] == "add" {
			return exec.Command("/bin/false")
		}
		return nil
	})

	_, _, err := EnsureIMDSVeth(context.Background(), testSubnetID, testSubnetCIDR)
	if err == nil || !strings.Contains(err.Error(), "addr add") {
		t.Fatalf("expected addr-add error, got %v", err)
	}
	var sawNetnsDel bool
	for _, c := range *calls {
		if c[0] == "ip" && len(c) >= 3 && c[1] == "netns" && c[2] == "del" {
			sawNetnsDel = true
		}
	}
	if !sawNetnsDel {
		t.Errorf("expected netns cleanup after addr failure; calls=%v", *calls)
	}
}

func TestRemoveIMDSVeth(t *testing.T) {
	calls := recordSudo(t, nil)

	if err := RemoveIMDSVeth(context.Background(), testSubnetID); err != nil {
		t.Fatalf("RemoveIMDSVeth: %v", err)
	}

	want := [][]string{
		{"ovs-vsctl", "--if-exists", "del-port", "imds-o-89abcdef"},
		{"ip", "netns", "del", "imds-89abcdef"},
		{"ip", "link", "del", "imds-o-89abcdef"},
	}
	if len(*calls) != len(want) {
		t.Fatalf("got %d calls, want %d: %v", len(*calls), len(want), *calls)
	}
	for i := range want {
		if !slices.Equal((*calls)[i], want[i]) {
			t.Errorf("call %d = %v, want %v", i, (*calls)[i], want[i])
		}
	}
}

func TestRemoveIMDSVeth_MissingDeviceIsNotError(t *testing.T) {
	recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ip" && len(args) >= 2 && args[0] == "netns" && args[1] == "del" {
			return exec.Command("sh", "-c", "echo 'Cannot remove namespace file \"/var/run/netns/imds-89abcdef\": No such file or directory' >&2; exit 1")
		}
		if name == "ip" && len(args) >= 2 && args[0] == "link" && args[1] == "del" {
			// Emit the kernel's "absent device" message on stderr and fail.
			return exec.Command("sh", "-c", "echo 'Cannot find device \"imds-o-89abcdef\"' >&2; exit 1")
		}
		return nil
	})

	if err := RemoveIMDSVeth(context.Background(), testSubnetID); err != nil {
		t.Fatalf("expected nil for absent device, got %v", err)
	}
}

// TestIMDSVethNamesWithinIFNAMSIZ guards that both veth-end names fit IFNAMSIZ-1
// (15 chars). A longer name causes `ip link add` to fail cluster-wide, silently
// taking IMDS offline in a way the SudoCommand mock would never catch.
func TestIMDSVethNamesWithinIFNAMSIZ(t *testing.T) {
	const ifnamsizMax = 15
	for _, subnetID := range []string{
		"subnet-0123456789abcdef",
		"subnet-0a1b2c3d4e5f6a7b8",
		"subnet-deadbeef",
		"short",
	} {
		for _, name := range []string{IMDSOVSPortName(subnetID), IMDSHostVethName(subnetID)} {
			if len(name) > ifnamsizMax {
				t.Errorf("veth name %q (%d chars) exceeds IFNAMSIZ-1 (%d) for subnet %q", name, len(name), ifnamsizMax, subnetID)
			}
		}
	}
}
