package netprobe

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Captured from hydrogen (HP DL325 Gen10, dual 25gbe Mellanox) in us-west-1-az1.
const mellanox25g = `● 6: ens1f0np0
                   Link File: /usr/lib/systemd/network/99-default.link
                Network File: /etc/systemd/network/12-spinifex-lan-nic.network
                       State: enslaved (configured)
                Online state: online
                        Type: ether
                        Path: pci-0000:84:00.0
                      Driver: mlx5_core
                      Vendor: Mellanox Technologies
                       Model: MT27710 Family [ConnectX-4 Lx]
           Alternative Names: enp132s0f0np0
                              enx040973e5ce60
            Hardware Address: 04:09:73:e5:ce:60 (Hewlett Packard Enterprise)
                         MTU: 9000 (min: 68, max: 9978)
                       QDisc: mq
                      Master: br-lan
IPv6 Address Generation Mode: none
    Number of Queues (Tx/Rx): 384/48
            Auto negotiation: yes
                       Speed: 25Gbps
                      Duplex: full
           Activation Policy: up
         Required For Online: yes
                Connected To: MikroTik (MikroTik RouterOS 7.11.3 (stable) Sep/27/2023 13:09:44 CRS504-4XQ) on port bridge/qsfp28-3-3
`

// A link with no carrier reports neither Speed nor an online state, which is
// the case Probe brings links up to avoid.
const linkDown = `● 3: eno6
                   Link File: /usr/lib/systemd/network/99-default.link
                       State: off
                Online state: off
                        Type: ether
                      Driver: bnxt_en
                      Vendor: Broadcom Inc. and subsidiaries
                       Model: NetXtreme BCM5720 Gigabit Ethernet PCIe
            Hardware Address: 04:09:73:e5:ce:5f
                         MTU: 1500 (min: 60, max: 9000)
`

func TestParseStatusMellanox(t *testing.T) {
	nic := ParseStatus(mellanox25g)

	if nic.Vendor != "Mellanox Technologies" {
		t.Errorf("Vendor = %q, want %q", nic.Vendor, "Mellanox Technologies")
	}
	if nic.Model != "MT27710 Family [ConnectX-4 Lx]" {
		t.Errorf("Model = %q, want %q", nic.Model, "MT27710 Family [ConnectX-4 Lx]")
	}
	if nic.Speed != "25Gbps" {
		t.Errorf("Speed = %q, want %q", nic.Speed, "25Gbps")
	}
	if nic.State != "online" {
		t.Errorf("State = %q, want %q", nic.State, "online")
	}
	if nic.MTU != 9000 {
		t.Errorf("MTU = %d, want 9000", nic.MTU)
	}
	// Only the first alternative name is taken; enx040973e5ce60 is ignored.
	if nic.AltName != "enp132s0f0np0" {
		t.Errorf("AltName = %q, want %q", nic.AltName, "enp132s0f0np0")
	}
}

func TestParseStatusLinkDown(t *testing.T) {
	nic := ParseStatus(linkDown)

	if nic.Speed != "" {
		t.Errorf("Speed = %q, want empty for a down link", nic.Speed)
	}
	if nic.State != "off" {
		t.Errorf("State = %q, want %q", nic.State, "off")
	}
	if nic.Model != "NetXtreme BCM5720 Gigabit Ethernet PCIe" {
		t.Errorf("Model = %q, want the Broadcom model", nic.Model)
	}
	if nic.MTU != 1500 {
		t.Errorf("MTU = %d, want 1500", nic.MTU)
	}
}

func TestParseStatusEmpty(t *testing.T) {
	// A truncated or unparseable status must not panic or invent values.
	nic := ParseStatus("● 2: eth0\n     Online state:\n     Alternative Names:\n")
	if nic.State != "" || nic.AltName != "" {
		t.Errorf("expected empty fields from valueless keys, got State=%q AltName=%q", nic.State, nic.AltName)
	}
}

func TestLabelFallsBackWhenSpeedUnknown(t *testing.T) {
	got := NIC{Name: "eth0", State: "no cable"}.Label()
	want := "eth0  —  no cable"
	if got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}

// Hardware must never render "unknown": an operator standing at the console
// needs something to match against the card in their hand, and sysfs always has
// at least a driver or a device id.
func TestHardwareDegradesRatherThanUnknown(t *testing.T) {
	tests := []struct {
		name string
		nic  NIC
		want string
	}{
		{
			name: "database name wins",
			nic:  NIC{Vendor: "Mellanox Technologies", Model: "MT27710", Driver: "mlx5_core", DeviceID: "15b3:1015"},
			want: "Mellanox Technologies MT27710",
		},
		{"vendor only", NIC{Vendor: "Intel Corporation"}, "Intel Corporation"},
		{"driver and id", NIC{Driver: "igb", DeviceID: "8086:1533"}, "igb [8086:1533]"},
		{"driver only", NIC{Driver: "virtio_net"}, "virtio_net"},
		{"id only", NIC{DeviceID: "0bda:8153"}, "0bda:8153"},
		{"nothing at all", NIC{Name: "eth0"}, "no hardware detail"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.nic.Hardware(); got != tt.want {
				t.Errorf("Hardware() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatSpeed(t *testing.T) {
	tests := []struct {
		mbps int
		want string
	}{
		{25000, "25 Gbps"},
		{1000, "1 Gbps"},
		{100, "100 Mbps"},
		{2500, "2500 Mbps"},
		{0, ""},
		{-1, ""},
	}

	for _, tt := range tests {
		if got := formatSpeed(tt.mbps); got != tt.want {
			t.Errorf("formatSpeed(%d) = %q, want %q", tt.mbps, got, tt.want)
		}
	}
}

// A live cable outranks everything, then wired over wireless, then speed. This
// is what puts the ports an operator would actually assign at indices 0..2,
// where newModel pre-fills wan, lan and vpc from.
func TestByUsefulnessOrdersPluggedPortsFirst(t *testing.T) {
	nics := []NIC{
		{Name: "eno1", SpeedMbps: 1000},
		{Name: "wlp2s0", Carrier: true, IsWiFi: true, SpeedMbps: 866},
		{Name: "ens1f1np1", SpeedMbps: 25000},
		{Name: "ens1f0np0", Carrier: true, SpeedMbps: 25000},
		{Name: "eno2", Carrier: true, SpeedMbps: 1000},
	}
	slices.SortFunc(nics, byUsefulness)

	want := []string{"ens1f0np0", "eno2", "wlp2s0", "ens1f1np1", "eno1"}
	for i, name := range want {
		if nics[i].Name != name {
			t.Fatalf("order = %v, want %v", names(nics), want)
		}
	}
}

func names(nics []NIC) []string {
	out := make([]string, len(nics))
	for i, n := range nics {
		out[i] = n.Name
	}
	return out
}

func TestProbeReadsSysfsWithoutSystemd(t *testing.T) {
	root := t.TempDir()
	sysClassNet = root
	linkUpTimeout = 0
	t.Cleanup(func() {
		sysClassNet = "/sys/class/net"
		linkUpTimeout = 2 * time.Second
	})

	// ens1f0np0: a 25gbe Mellanox with a live cable, identified by the hardware
	// database. eno1: a 1gbe port that is up but unplugged, and absent from the
	// database, so only its driver and PCI id identify it.
	writeIface(t, root, "ens1f0np0", map[string]string{
		"speed": "25000", "carrier": "1", "mtu": "9000", "flags": "0x1003",
		"device/vendor": "0x15b3", "device/device": "0x1015",
	}, "mlx5_core")
	writeIface(t, root, "eno1", map[string]string{
		"speed": "1000", "carrier": "0", "mtu": "1500", "flags": "0x1003",
		"device/vendor": "0x8086", "device/device": "0x1533",
	}, "igb")

	listInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", Flags: net.FlagLoopback},
			{Name: "eno1", HardwareAddr: net.HardwareAddr{4, 9, 0x73, 0xe5, 0xce, 0x5f}},
			{Name: "ens1f0np0", HardwareAddr: net.HardwareAddr{4, 9, 0x73, 0xe5, 0xce, 0x60}},
			{Name: "veth0"}, // no hardware address, so not a physical port
			{Name: "br-lan", HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1}}, // a bridge, so no device behind it
		}, nil
	}
	t.Cleanup(func() { listInterfaces = net.Interfaces })

	// Only udevadm answers. networkctl fails the way it does in the live
	// installer, where there is no systemd-networkd for it to reach.
	commandOutput = func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "ip":
			return nil, nil
		case name == "udevadm" && strings.HasSuffix(args[len(args)-1], "ens1f0np0"):
			return []byte("ID_VENDOR_FROM_DATABASE=Mellanox Technologies\n" +
				"ID_MODEL_FROM_DATABASE=MT27710 Family [ConnectX-4 Lx]\n" +
				"ID_NET_NAME_PATH=enp132s0f0np0\n"), nil
		case name == "udevadm":
			return []byte("ID_NET_NAME_PATH=enp1s0f0\n"), nil
		default:
			return nil, errors.New("Failed to connect to bus: No such file or directory")
		}
	}
	t.Cleanup(func() {
		commandOutput = func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).Output()
		}
	})

	nics, err := Probe()
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := names(nics); !slices.Equal(got, []string{"ens1f0np0", "eno1"}) {
		t.Fatalf("probed %v, want the two physical ports with the plugged one first", got)
	}

	mlx := nics[0]
	if mlx.Label() != "ens1f0np0  25 Gbps  online" {
		t.Errorf("Label() = %q", mlx.Label())
	}
	if want := "Mellanox Technologies MT27710 Family [ConnectX-4 Lx]"; mlx.Hardware() != want {
		t.Errorf("Hardware() = %q, want %q", mlx.Hardware(), want)
	}
	if mlx.AltName != "enp132s0f0np0" || mlx.MTU != 9000 || !mlx.Carrier {
		t.Errorf("unexpected detail: %+v", mlx)
	}

	// Up but unplugged, and unknown to the database: the driver and PCI id are
	// all the operator gets, which is still enough to identify the card.
	igb := nics[1]
	if igb.Label() != "eno1  1 Gbps  no cable" {
		t.Errorf("Label() = %q", igb.Label())
	}
	if igb.Hardware() != "igb [8086:1533]" {
		t.Errorf("Hardware() = %q", igb.Hardware())
	}
}

// writeIface lays out the sysfs attributes Probe reads for one interface,
// including the driver symlink it resolves the module name from.
func writeIface(t *testing.T, root, name string, attrs map[string]string, driver string) {
	t.Helper()
	for attr, value := range attrs {
		path := filepath.Join(root, name, attr)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../bus/pci/drivers/"+driver, filepath.Join(root, name, "device/driver")); err != nil {
		t.Fatal(err)
	}
}
