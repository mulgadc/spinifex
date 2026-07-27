package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Node layouts drawn from the us-west-1 racks, which between them exercise
// every collapse permutation the installer has to express.
func threeNIC() *Config { // hydrogen / radon
	return &Config{
		WAN: NetworkRole{Interface: "eno6", Address: "216.218.163.99", Mask: "255.255.255.224", Gateway: "216.218.163.97"},
		LAN: NetworkRole{Interface: "ens1f0np0", Address: "10.0.0.3", Mask: "255.255.0.0", MTU: 9000},
		VPC: NetworkRole{Interface: "ens1f1np1", Address: "10.1.0.3", Mask: "255.255.0.0", MTU: 9000},
	}
}

func vlanSplit() *Config { // helium — one 100gbe carrying lan and vpc
	return &Config{
		WAN: NetworkRole{Interface: "enx6c1ff7c1475a", DHCPMode: true},
		LAN: NetworkRole{Interface: "enp129s0np0", VLAN: 10, Address: "10.0.0.2", Mask: "255.255.0.0", MTU: 9000},
		VPC: NetworkRole{Interface: "enp129s0np0", VLAN: 20, Address: "10.1.0.2", Mask: "255.255.0.0", MTU: 9000},
	}
}

func dualPortTagged() *Config { // us-west-1-az2 — wan and lan share port 1
	return &Config{
		WAN: NetworkRole{Interface: "enp1s0f0", VLAN: 30, Address: "216.218.163.10", Mask: "255.255.255.224", Gateway: "216.218.163.97"},
		LAN: NetworkRole{Interface: "enp1s0f0", VLAN: 10, Address: "10.0.0.10", Mask: "255.255.0.0", MTU: 9000},
		VPC: NetworkRole{Interface: "enp1s0f1", VLAN: 20, Address: "10.1.0.10", Mask: "255.255.0.0", MTU: 9000},
	}
}

func TestResolveCollapsesUpTheChain(t *testing.T) {
	singleNIC := &Config{WAN: NetworkRole{Interface: "eth0", Address: "192.168.1.10", Mask: "255.255.255.0"}}
	twoNIC := &Config{
		WAN: NetworkRole{Interface: "eth0", Address: "192.168.1.10", Mask: "255.255.255.0"},
		LAN: NetworkRole{Interface: "eth1", Address: "10.0.0.5", Mask: "255.255.0.0"},
	}

	tests := []struct {
		name      string
		cfg       *Config
		plane     Plane
		wantAddr  string
		wantPlane Plane
	}{
		{"1 NIC: lan folds onto wan", singleNIC, PlaneLAN, "192.168.1.10", PlaneWAN},
		{"1 NIC: vpc folds all the way to wan", singleNIC, PlaneVPC, "192.168.1.10", PlaneWAN},
		{"2 NIC: vpc folds onto lan", twoNIC, PlaneVPC, "10.0.0.5", PlaneLAN},
		{"2 NIC: lan stays put", twoNIC, PlaneLAN, "10.0.0.5", PlaneLAN},
		{"3 NIC: vpc is its own plane", threeNIC(), PlaneVPC, "10.1.0.3", PlaneVPC},
		{"VLAN split: vpc is its own plane", vlanSplit(), PlaneVPC, "10.1.0.2", PlaneVPC},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.PlaneAddress(tt.plane); got != tt.wantAddr {
				t.Errorf("PlaneAddress(%s) = %q, want %q", tt.plane, got, tt.wantAddr)
			}
			if _, landed := tt.cfg.Resolve(tt.plane); landed != tt.wantPlane {
				t.Errorf("Resolve(%s) landed on %s, want %s", tt.plane, landed, tt.wantPlane)
			}
		})
	}
}

// A DHCP plane has no address known at install time; setup-ovn.sh auto-detects
// it from the default route at boot instead.
func TestPlaneAddressEmptyUnderDHCP(t *testing.T) {
	if got := vlanSplit().PlaneAddress(PlaneWAN); got != "" {
		t.Errorf("PlaneAddress(wan) = %q, want empty for a DHCP role", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"three physical NICs", threeNIC(), ""},
		{"VLAN split on one NIC", vlanSplit(), ""},
		{"dual port, all tagged", dualPortTagged(), ""},
		{
			"wan cannot be folded",
			&Config{LAN: NetworkRole{Interface: "eth1"}},
			"wan role must be bound",
		},
		{
			"two roles share a NIC untagged",
			&Config{
				WAN: NetworkRole{Interface: "eth0"},
				LAN: NetworkRole{Interface: "eth0"},
			},
			"both bind eth0",
		},
		{
			"duplicate VLAN id on one NIC",
			&Config{
				WAN: NetworkRole{Interface: "eth0", VLAN: 10},
				LAN: NetworkRole{Interface: "eth0", VLAN: 10},
			},
			"both bind eth0.10",
		},
		{
			"VLAN id out of range",
			&Config{WAN: NetworkRole{Interface: "eth0", VLAN: 5000}},
			"out of range",
		},
		{
			"MTU out of range",
			&Config{WAN: NetworkRole{Interface: "eth0", MTU: 70000}},
			"MTU 70000 out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// Two roles sharing one NIC must produce a single parent .network carrying both
// VLAN declarations — not two files racing to configure the same interface.
func TestWriteParentNICTaggedSharesOneFile(t *testing.T) {
	dir := t.TempDir()
	p := &parentNIC{vlans: []string{"enp129s0np0.10", "enp129s0np0.20"}, mtu: 9000}
	if err := writeParentNIC(dir, "enp129s0np0", p); err != nil {
		t.Fatal(err)
	}

	got := readFile(t, dir, "05-spinifex-nic-enp129s0np0.network")
	for _, want := range []string{
		"Name=enp129s0np0",
		"MTUBytes=9000",
		"VLAN=enp129s0np0.10",
		"VLAN=enp129s0np0.20",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("parent .network missing %q:\n%s", want, got)
		}
	}
	// A tagged parent is never itself enslaved — the VLAN devices are.
	if strings.Contains(got, "Bridge=") {
		t.Errorf("tagged parent must not be bridged directly:\n%s", got)
	}
}

func TestWriteNetworkdBridgeVLAN(t *testing.T) {
	dir := t.TempDir()
	cfg := vlanSplit()
	if err := writeNetworkdBridge(dir, PlaneVPC, cfg.VPC, true); err != nil {
		t.Fatal(err)
	}

	netdev := readFile(t, dir, "10-spinifex-vpc-vlan.netdev")
	if !strings.Contains(netdev, "Kind=vlan") || !strings.Contains(netdev, "Id=20") {
		t.Errorf("VLAN netdev wrong:\n%s", netdev)
	}
	if !strings.Contains(netdev, "Name=enp129s0np0.20") {
		t.Errorf("VLAN netdev name wrong:\n%s", netdev)
	}

	vlanNet := readFile(t, dir, "11-spinifex-vpc-vlan.network")
	if !strings.Contains(vlanNet, "Bridge=br-vpc") {
		t.Errorf("VLAN device must be enslaved to br-vpc:\n%s", vlanNet)
	}

	brNet := readFile(t, dir, "20-spinifex-vpc.network")
	if !strings.Contains(brNet, "Address=10.1.0.2/16") {
		t.Errorf("bridge address wrong:\n%s", brNet)
	}
	// Only wan installs a default route; an east-west plane must not.
	if strings.Contains(brNet, "Gateway=") {
		t.Errorf("vpc plane must not install a default route:\n%s", brNet)
	}
	if !strings.Contains(brNet, "ActivationPolicy=manual") {
		t.Errorf("east-west bridge must not auto-activate:\n%s", brNet)
	}
}

// The wan plane is the one that carries a default route and auto-activates.
func TestWriteNetworkdBridgeWANUntagged(t *testing.T) {
	dir := t.TempDir()
	cfg := threeNIC()
	if err := writeNetworkdBridge(dir, PlaneWAN, cfg.WAN, false); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "10-spinifex-wan-vlan.netdev")); !os.IsNotExist(err) {
		t.Error("untagged role must not produce a VLAN netdev")
	}
	brNet := readFile(t, dir, "20-spinifex-wan.network")
	if !strings.Contains(brNet, "Gateway=216.218.163.97") {
		t.Errorf("wan must install its default route:\n%s", brNet)
	}
	if strings.Contains(brNet, "ActivationPolicy=manual") {
		t.Errorf("wan bridge must auto-activate:\n%s", brNet)
	}
}

func TestToFirstbootConfigSourcesPlanes(t *testing.T) {
	cfg := threeNIC()
	cfg.Hostname = "hydrogen"
	fb := cfg.toFirstbootConfig()

	if fb.EncapIP != "10.1.0.3" {
		t.Errorf("EncapIP = %q, want the vpc plane address", fb.EncapIP)
	}
	if fb.LANIP != "10.0.0.3" {
		t.Errorf("LANIP = %q, want the lan plane address", fb.LANIP)
	}
	// The public address has to travel separately: spx echoes a concrete --bind
	// back as the advertise address, so leaving this empty would publish the
	// internal plane as the node's public dial target.
	if fb.WANIP != "216.218.163.99" {
		t.Errorf("WANIP = %q, want the wan plane address", fb.WANIP)
	}

	// On a single-NIC node all three collapse onto wan.
	single := &Config{WAN: NetworkRole{Interface: "eth0", Address: "192.168.1.10", Mask: "255.255.255.0"}}
	fb = single.toFirstbootConfig()
	if fb.EncapIP != "192.168.1.10" || fb.LANIP != "192.168.1.10" || fb.WANIP != "192.168.1.10" {
		t.Errorf("single-NIC collapse wrong: EncapIP=%q LANIP=%q WANIP=%q", fb.EncapIP, fb.LANIP, fb.WANIP)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// Resolvers belong to the plane holding the default route. The east-west
// bridges are link-local and activate manually, so a resolver pinned to one is
// unreachable while resolved still tries it — a timeout in front of every
// lookup. The TUI cannot express this; the kernel cmdline can, so it is caught.
func TestValidateRejectsDNSOffWAN(t *testing.T) {
	cfg := threeNIC()
	cfg.LAN.DNS = []string{"10.0.0.1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("lan must not carry DNS servers")
	}

	cfg = threeNIC()
	cfg.VPC.DNS = []string{"10.1.0.1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("vpc must not carry DNS servers")
	}

	cfg = threeNIC()
	cfg.WAN.DNS = []string{"8.8.8.8", "1.1.1.1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("wan carries the resolvers: %v", err)
	}
}

// Even if a role somehow carries DNS, generation must not write it onto an
// east-west bridge.
func TestWriteNetworkdBridgeKeepsDNSOnWAN(t *testing.T) {
	dir := t.TempDir()
	lan := NetworkRole{Interface: "ens1f0np0", Address: "10.0.0.3", Mask: "255.255.0.0", DNS: []string{"10.0.0.1"}}
	if err := writeNetworkdBridge(dir, PlaneLAN, lan, true); err != nil {
		t.Fatalf("writeNetworkdBridge: %v", err)
	}
	if got := readFile(t, dir, "20-spinifex-lan.network"); strings.Contains(got, "DNS=") {
		t.Errorf("lan bridge must carry no resolver:\n%s", got)
	}

	wan := NetworkRole{Interface: "eno6", Address: "216.218.163.99", Mask: "255.255.255.224", Gateway: "216.218.163.97", DNS: []string{"8.8.8.8"}}
	if err := writeNetworkdBridge(dir, PlaneWAN, wan, false); err != nil {
		t.Fatalf("writeNetworkdBridge: %v", err)
	}
	if got := readFile(t, dir, "20-spinifex-wan.network"); !strings.Contains(got, "DNS=8.8.8.8") {
		t.Errorf("wan bridge must carry the resolver:\n%s", got)
	}
}

func TestSwapSize(t *testing.T) {
	dir := t.TempDir()

	// Without an override, RAM drives the size and the disk caps it.
	ram, err := totalRAM()
	if err != nil {
		t.Fatalf("totalRAM: %v", err)
	}
	got, err := swapSize(dir)
	if err != nil {
		t.Fatalf("swapSize: %v", err)
	}
	want := ram / 2
	if ram >= swapRAMThreshold {
		want = swapLargeBytes
	}
	if got > want {
		t.Errorf("swapSize = %d, must not exceed what RAM implies (%d)", got, want)
	}
	if got != 0 && got < swapMinBytes {
		t.Errorf("swapSize = %d, must be 0 rather than below the %d floor", got, int64(swapMinBytes))
	}

	// An explicit size wins, so a large node can be told to take the full 32G
	// and a constrained one can opt out entirely.
	for _, tt := range []struct {
		env  string
		want int64
	}{
		{"32G", 32 << 30},
		{"512M", 512 << 20},
		{"0", 0},
		{"1073741824", 1 << 30},
	} {
		t.Setenv("SPINIFEX_SWAP_SIZE", tt.env)
		got, err := swapSize(dir)
		if err != nil {
			t.Fatalf("swapSize(%s): %v", tt.env, err)
		}
		if got != tt.want {
			t.Errorf("swapSize(%s) = %d, want %d", tt.env, got, tt.want)
		}
	}

	t.Setenv("SPINIFEX_SWAP_SIZE", "banana")
	if _, err := swapSize(dir); err == nil {
		t.Error("an unparseable size must be reported, not silently ignored")
	}
}

// The swap line must only appear when the file backing it exists — systemd
// fails the boot's swap unit on a missing file rather than skipping it.
func TestFstabSwapLineTracksTheFile(t *testing.T) {
	if _, err := parseSize("8G"); err != nil {
		t.Fatalf("parseSize: %v", err)
	}
	if n, _ := parseSize("2G"); n != 2<<30 {
		t.Errorf("parseSize(2G) = %d, want %d", n, int64(2)<<30)
	}
	if _, err := parseSize("-5G"); err == nil {
		t.Error("negative sizes must be rejected")
	}
}
