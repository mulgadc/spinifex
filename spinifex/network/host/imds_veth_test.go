package host

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

const testVPCID = "vpc-0123456789abcdef" // short form: "89abcdef"

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

	hostEnd, err := EnsureIMDSVeth(context.Background(), testVPCID)
	if err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}
	if hostEnd != "imds-h-89abcdef" {
		t.Errorf("hostEnd = %q, want imds-h-89abcdef", hostEnd)
	}

	want := [][]string{
		{"ovs-vsctl", "port-to-br", "imds-ovs-89abcdef"},
		{"ip", "link", "add", "imds-ovs-89abcdef", "type", "veth", "peer", "name", "imds-h-89abcdef"},
		{"ip", "link", "set", "imds-ovs-89abcdef", "up"},
		{"ip", "link", "set", "imds-h-89abcdef", "up"},
		{"ovs-vsctl", "add-port", "br-int", "imds-ovs-89abcdef",
			"--", "set", "Interface", "imds-ovs-89abcdef",
			"external_ids:iface-id=imds-port-" + testVPCID},
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
	// Probe reports the OVS end is already on br-int → early return, no mutation.
	calls := recordSudo(t, func(name string, args []string) *exec.Cmd {
		if name == "ovs-vsctl" && len(args) >= 1 && args[0] == "port-to-br" {
			return exec.Command("echo", "br-int")
		}
		return nil
	})

	hostEnd, err := EnsureIMDSVeth(context.Background(), testVPCID)
	if err != nil {
		t.Fatalf("EnsureIMDSVeth: %v", err)
	}
	if hostEnd != "imds-h-89abcdef" {
		t.Errorf("hostEnd = %q, want imds-h-89abcdef", hostEnd)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected only the probe call, got %v", *calls)
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

	_, err := EnsureIMDSVeth(context.Background(), testVPCID)
	if err == nil || !strings.Contains(err.Error(), "add IMDS veth") {
		t.Fatalf("expected add-port error, got %v", err)
	}
	// Cleanup must have run: del-port then link del.
	var sawDelPort, sawLinkDel bool
	for _, c := range *calls {
		if c[0] == "ovs-vsctl" && slices.Contains(c, "del-port") {
			sawDelPort = true
		}
		if c[0] == "ip" && len(c) >= 3 && c[1] == "link" && c[2] == "del" {
			sawLinkDel = true
		}
	}
	if !sawDelPort || !sawLinkDel {
		t.Errorf("expected cleanup (del-port=%v, link del=%v); calls=%v", sawDelPort, sawLinkDel, *calls)
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

	_, err := EnsureIMDSVeth(context.Background(), testVPCID)
	if err == nil || !strings.Contains(err.Error(), "create IMDS veth pair") {
		t.Fatalf("expected create error, got %v", err)
	}
}

func TestRemoveIMDSVeth(t *testing.T) {
	calls := recordSudo(t, nil)

	if err := RemoveIMDSVeth(context.Background(), testVPCID); err != nil {
		t.Fatalf("RemoveIMDSVeth: %v", err)
	}

	want := [][]string{
		{"ovs-vsctl", "--if-exists", "del-port", "imds-ovs-89abcdef"},
		{"ip", "link", "del", "imds-h-89abcdef"},
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
		if name == "ip" && len(args) >= 2 && args[0] == "link" && args[1] == "del" {
			// Emit the kernel's "absent device" message on stderr and fail.
			return exec.Command("sh", "-c", "echo 'Cannot find device \"imds-h-89abcdef\"' >&2; exit 1")
		}
		return nil
	})

	if err := RemoveIMDSVeth(context.Background(), testVPCID); err != nil {
		t.Fatalf("expected nil for absent device, got %v", err)
	}
}
