package host

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// TestOVSPlumber_SetupTap_AddPortArgs captures the ovs-vsctl invocations
// for both VPC-style (populated ExternalIDs → `set Interface external_ids:k=v`)
// and management-style (empty ExternalIDs → bare `add-port`) calls. This is the
// functional difference that previously lived in two diverged setup functions.
func TestOVSPlumber_SetupTap_AddPortArgs(t *testing.T) {
	cases := []struct {
		name        string
		spec        vm.TapSpec
		wantAddPort []string // expected tail of ovs-vsctl args (after add-port <bridge> <name>)
	}{
		{
			name: "vpc style with external_ids",
			spec: vm.TapSpec{
				Name:   "tapeni-test",
				Bridge: "br-int",
				ExternalIDs: map[string]string{
					"iface-id":     "port-eni-test",
					"attached-mac": "02:00:00:aa:bb:cc",
				},
			},
			wantAddPort: []string{
				"add-port", "br-int", "tapeni-test",
				"--", "set", "Interface", "tapeni-test",
				"external_ids:attached-mac=02:00:00:aa:bb:cc",
				"external_ids:iface-id=port-eni-test",
			},
		},
		{
			name: "mgmt style without external_ids",
			spec: vm.TapSpec{
				Name:   "mg-test",
				Bridge: "br-mgmt",
			},
			wantAddPort: []string{"add-port", "br-mgmt", "mg-test"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
				call := append([]string{name}, args...)
				calls = append(calls, call)
				return exec.Command("/bin/true")
			}))

			p := NewOVSPlumber()
			if err := p.SetupTap(tc.spec); err != nil {
				t.Fatalf("SetupTap: %v", err)
			}

			// Find the add-port invocation (last ovs-vsctl call).
			var addPort []string
			for _, c := range calls {
				if len(c) >= 2 && c[0] == "ovs-vsctl" && c[1] != "--if-exists" {
					addPort = c[1:]
				}
			}
			if addPort == nil {
				t.Fatalf("no add-port invocation captured; calls=%v", calls)
			}
			if !slices.Equal(addPort, tc.wantAddPort) {
				t.Errorf("add-port args = %v, want %v", addPort, tc.wantAddPort)
			}
		})
	}
}

// TestOVSPlumber_SetupTap_PreExistingKernelTap exercises the
// pre-create kernel-side cleanup branch (lines guarded by /sys/class/net/<name>
// stat). Uses "lo" as a name that's always present so os.Stat returns success.
func TestOVSPlumber_SetupTap_PreExistingKernelTap(t *testing.T) {
	failTuntapDel := true
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ip" && len(args) >= 2 && args[0] == "tuntap" && args[1] == "del" && failTuntapDel {
			failTuntapDel = false
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	if err := p.SetupTap(vm.TapSpec{Name: "lo", Bridge: "br-int"}); err != nil {
		t.Fatalf("SetupTap with stubs should succeed: %v", err)
	}
}

// TestOVSPlumber_SetupTap_ErrorBranches stubs SudoCommand to fail at
// each step in turn and asserts the corresponding error is returned. Covers
// the cleanup-on-failure paths that the success-path tests miss.
func TestOVSPlumber_SetupTap_ErrorBranches(t *testing.T) {
	cases := []struct {
		name      string
		failMatch func(name string, args []string) bool
		wantErr   string
	}{
		{
			name: "ovs del-port failure is logged-warn (continues)",
			failMatch: func(name string, args []string) bool {
				return name == "ovs-vsctl" && len(args) >= 1 && args[0] == "--if-exists"
			},
			wantErr: "", // pre-clear failure is swallowed; SetupTap returns nil
		},
		{
			name: "tuntap add failure surfaces",
			failMatch: func(name string, args []string) bool {
				return name == "ip" && len(args) >= 2 && args[0] == "tuntap" && args[1] == "add"
			},
			wantErr: "create tap",
		},
		{
			name: "ip link set up failure surfaces and triggers tap cleanup",
			failMatch: func(name string, args []string) bool {
				return name == "ip" && len(args) >= 1 && args[0] == "link"
			},
			wantErr: "bring up tap",
		},
		{
			name: "ovs add-port failure surfaces and triggers tap cleanup",
			failMatch: func(name string, args []string) bool {
				return name == "ovs-vsctl" && len(args) >= 1 && args[0] == "add-port"
			},
			wantErr: "add tap",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
				if tc.failMatch(name, args) {
					return exec.Command("/bin/false")
				}
				return exec.Command("/bin/true")
			}))

			p := NewOVSPlumber()
			err := p.SetupTap(vm.TapSpec{Name: "tap-test-noexist", Bridge: "br-int"})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestOVSPlumber_CleanupTap_PresentKernelTap exercises the success
// path where the kernel device is present (uses "lo" so /sys/class/net/lo
// stat succeeds). Asserts no error and that ip tuntap del was attempted.
func TestOVSPlumber_CleanupTap_PresentKernelTap(t *testing.T) {
	var sawTuntapDel bool
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ip" && len(args) >= 2 && args[0] == "tuntap" && args[1] == "del" {
			sawTuntapDel = true
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	if err := p.CleanupTap("lo"); err != nil {
		t.Fatalf("CleanupTap: %v", err)
	}
	if !sawTuntapDel {
		t.Errorf("expected ip tuntap del to be invoked, got no call")
	}
}

// TestOVSPlumber_CleanupTap_DelPortLoggedWarn covers the OVS del-port
// failure branch (logged warn, continues to kernel-side cleanup).
func TestOVSPlumber_CleanupTap_DelPortLoggedWarn(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ovs-vsctl" {
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	// Tap doesn't exist on this test system → kernel-presence gate short-circuits
	// after the OVS warn, so CleanupTap returns nil.
	if err := p.CleanupTap("tap-test-noexist"); err != nil {
		t.Fatalf("CleanupTap: %v", err)
	}
}

// TestOVSPlumber_CleanupTap_TuntapDelFailure exercises the kernel-side
// failure branch (real kernel device present + ip tuntap del returns nonzero).
func TestOVSPlumber_CleanupTap_TuntapDelFailure(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ip" && len(args) >= 2 && args[0] == "tuntap" && args[1] == "del" {
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	err := p.CleanupTap("lo") // /sys/class/net/lo is present, so we reach tuntap del
	if err == nil || !strings.Contains(err.Error(), "delete tap") {
		t.Fatalf("expected 'delete tap' error, got %v", err)
	}
}

// TestOVSPlumber_CleanupTap_MissingKernelTap verifies the nil-safe branch:
// callers may invoke CleanupTap on a name that has neither an OVS port nor a
// kernel tap (e.g. a terminate that races mid-launch, or an instance that
// never reached SetupTap) without producing a misleading "Device does not
// exist" error from `ip tuntap del`.
func TestOVSPlumber_CleanupTap_MissingKernelTap(t *testing.T) {
	// 15-char IFNAMSIZ-compliant name that does not exist in /sys/class/net.
	// The OVS del-port call may fail on CI without ovs-vsctl, but is
	// best-effort + logged-warn; the kernel-presence gate short-circuits
	// before `ip tuntap del` runs.
	p := NewOVSPlumber()
	if err := p.CleanupTap("mg-test-noexist"); err != nil {
		t.Fatalf("expected nil for missing kernel tap, got: %v", err)
	}
}
