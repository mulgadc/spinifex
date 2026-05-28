package host

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

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

// "lo" is always present so os.Stat hits the pre-create cleanup branch.
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

func TestOVSPlumber_CleanupTap_DelPortLoggedWarn(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ovs-vsctl" {
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	if err := p.CleanupTap("tap-test-noexist"); err != nil {
		t.Fatalf("CleanupTap: %v", err)
	}
}

func TestOVSPlumber_CleanupTap_TuntapDelFailure(t *testing.T) {
	t.Cleanup(utils.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		if name == "ip" && len(args) >= 2 && args[0] == "tuntap" && args[1] == "del" {
			return exec.Command("/bin/false")
		}
		return exec.Command("/bin/true")
	}))

	p := NewOVSPlumber()
	err := p.CleanupTap("lo")
	if err == nil || !strings.Contains(err.Error(), "delete tap") {
		t.Fatalf("expected 'delete tap' error, got %v", err)
	}
}

// CleanupTap on a name with neither OVS port nor kernel tap must return nil.
func TestOVSPlumber_CleanupTap_MissingKernelTap(t *testing.T) {
	p := NewOVSPlumber()
	if err := p.CleanupTap("mg-test-noexist"); err != nil {
		t.Fatalf("expected nil for missing kernel tap, got: %v", err)
	}
}
