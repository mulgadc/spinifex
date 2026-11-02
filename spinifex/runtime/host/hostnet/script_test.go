package hostnet

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const setupOVN = "../../../../scripts/setup-ovn.sh"

// shellFunc returns the body of a top-level function in setup-ovn.sh.
func shellFunc(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(setupOVN)
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	start := strings.Index(src, "\n"+name+"() {\n")
	if start < 0 {
		t.Fatalf("%s: no function %s", setupOVN, name)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("%s: function %s is not closed", setupOVN, name)
	}
	return src[start : start+end+3]
}

// TestScriptStateLoads runs setup-ovn.sh's state writer for every WAN mode and
// loads what it wrote, so the script and LoadState cannot drift apart.
func TestScriptStateLoads(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	fn := shellFunc(t, "write_hostnet_state")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgmt := &Mgmt{Bridge: "br-mgmt", CIDR: "10.15.8.1/24", NIC: "eth2"}
	route := &Route{Dst: "10.1.0.0/16", Dev: "eth1", Src: "10.1.0.5", Metric: 50}

	for _, tc := range []struct {
		name string
		env  []string
		want State
	}{
		{"existing", []string{"WAN_BRIDGE_MODE=existing", "WAN_BRIDGE=br-ext"},
			State{WAN: &WAN{Mode: ModeExisting, OVSBridge: "br-ext"}}},
		{"veth", []string{"WAN_BRIDGE_MODE=veth", "WAN_BRIDGE=br-ext", "LINUX_BRIDGE=br-wan"},
			State{WAN: &WAN{Mode: ModeVeth, OVSBridge: "br-ext", LinuxBridge: "br-wan", HostEnd: "veth-wan-br", OVSEnd: "veth-wan-ovs"}}},
		{"direct", []string{"WAN_BRIDGE_MODE=direct", "WAN_BRIDGE=br-ext", "WAN_IFACE=eth3"},
			State{WAN: &WAN{Mode: ModeDirect, OVSBridge: "br-ext", NIC: "eth3"}}},
		{"nat", []string{"WAN_BRIDGE_MODE=nat", "WAN_BRIDGE=br-ext", "NAT_TRANSIT_HOST_MAC=02:00:64:7f:00:01", "NAT_TRANSIT_GW_CIDR=100.127.0.1/24"},
			State{WAN: &WAN{Mode: ModeNAT, OVSBridge: "br-ext", HostEnd: "spx-nat-host", OVSEnd: "spx-nat-ovs", HostMAC: "02:00:64:7f:00:01", HostCIDR: "100.127.0.1/24"}}},
		{"no wan", nil, State{}},
		{"mgmt and route", []string{"MGMT_BRIDGE_ENABLED=true", "MGMT_BRIDGE=br-mgmt", "MGMT_CIDR=10.15.8.1/24", "MGMT_IFACE=eth2",
			"ENCAP_ROUTE_DST=10.1.0.0/16", "ENCAP_ROUTE_DEV=eth1", "ENCAP_IP=10.1.0.5", "ENCAP_ROUTE_METRIC=50"},
			State{Mgmt: mgmt, EncapRoute: route}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "etc", "hostnet.json")
			write := func(pending string) {
				cmd := exec.Command("bash", "-c", fn+`write_hostnet_state "$1"`, "_", pending)
				cmd.Env = append([]string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOSTNET_STATE=" + state}, tc.env...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("write_hostnet_state %s: %v: %s", pending, err, out)
				}
			}
			write("false")
			got, err := LoadState(state)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			want.Version, want.IntBridge = StateVersion, "br-int"
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("loaded %+v\nwant   %+v", *got, want)
			}
			if _, err := os.Stat(state + ".tmp"); !os.IsNotExist(err) {
				t.Errorf("temporary file left behind: %v", err)
			}

			write("true")
			if _, err := LoadState(state); err == nil || !strings.Contains(err.Error(), "is pending") {
				t.Errorf("pending write loaded as %v, want a pending error", err)
			}
		})
	}
}
