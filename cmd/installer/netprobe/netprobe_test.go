package netprobe

import "testing"

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

func TestLabelFallsBackWhenHardwareUnknown(t *testing.T) {
	got := NIC{Name: "eth0", State: "off"}.Label()
	want := "eth0  —  off  unknown"
	if got != want {
		t.Errorf("Label() = %q, want %q", got, want)
	}
}
