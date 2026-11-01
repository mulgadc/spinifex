package nodefacts

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures are dmidecode, ethtool and networkctl output captured from
// prod node1, dev-prod bottlebrush and a QEMU dev box, serials replaced.
func fixture(t *testing.T, host, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", host, name))
	require.NoError(t, err)
	return string(data)
}

func TestMemoryGroupsIdenticalDIMMsAndCountsEmptySlots(t *testing.T) {
	m := parseMemory(fixture(t, "bottlebrush", "dmi-memory.txt"))

	assert.Equal(t, 32, m.SlotsTotal)
	assert.Equal(t, 16, m.SlotsUsed)
	assert.Equal(t, uint64(256)<<30, m.TotalBytes)
	assert.Equal(t, "8 TB", m.MaxCapacity)
	assert.Equal(t, "Multi-bit ECC", m.ECC)
	require.Len(t, m.Groups, 1)
	g := m.Groups[0]
	assert.Equal(t, 16, g.Count)
	assert.Equal(t, uint64(16)<<30, g.SizeBytes)
	assert.Equal(t, "DDR4", g.Type)
	assert.Equal(t, 2400, g.SpeedMTs)
	assert.Equal(t, 2400, g.ConfiguredMTs)
	assert.Equal(t, "SK Hynix", g.Manufacturer)
	assert.Equal(t, "HMA82GR7AFR8N-UH", g.PartNumber)
	assert.Len(t, g.Serials, 16)
}

func TestMemorySplitsDIMMsThatDifferInAnyShownField(t *testing.T) {
	dimm := func(size, speed, part string) string {
		return "Memory Device\n\tSize: " + size + "\n\tType: DDR5\n\tSpeed: " + speed +
			"\n\tManufacturer: Samsung\n\tPart Number: " + part + "\n\tConfigured Memory Speed: 4800 MT/s\n\n"
	}
	m := parseMemory(dimm("32 GB", "5600 MT/s", "A") + dimm("32 GB", "5600 MT/s", "A") +
		dimm("64 GB", "5600 MT/s", "B") + "Memory Device\n\tSize: No Module Installed\n\n")

	assert.Equal(t, 4, m.SlotsTotal)
	assert.Equal(t, 3, m.SlotsUsed)
	require.Len(t, m.Groups, 2)
	assert.Equal(t, 2, m.Groups[0].Count, "largest group first")
	assert.Equal(t, 5600, m.Groups[0].SpeedMTs)
	assert.Equal(t, 4800, m.Groups[0].ConfiguredMTs, "running below rated speed stays visible")
	assert.Equal(t, uint64(64)<<30, m.Groups[1].SizeBytes)
}

func TestProcessorsSummariseEverySocket(t *testing.T) {
	c := parseProcessors(fixture(t, "node1", "dmi-processor.txt"))

	assert.Equal(t, "AMD EPYC 7513 32-Core Processor", c.Model)
	assert.Equal(t, 2, c.Sockets)
	assert.Equal(t, 32, c.CoresPerSocket)
	assert.Equal(t, 64, c.Cores)
	assert.Equal(t, 128, c.Threads)
	assert.Equal(t, 3675, c.MaxMHz)
}

func TestIdentityDropsFirmwarePlaceholders(t *testing.T) {
	s := parseIdentity(fixture(t, "node1", "dmi-identity.txt"))
	assert.Equal(t, "GIGABYTE", s.Vendor)
	assert.Equal(t, "H262-Z62-ZB", s.Product)
	assert.Equal(t, "Rack Mount Chassis", s.ChassisType)
	assert.Equal(t, "M30", s.BIOSVersion)
	assert.Equal(t, "06/23/2026", s.BIOSDate)

	vm := parseIdentity(fixture(t, "devbox", "dmi-identity.txt"))
	assert.Equal(t, "SeaBIOS", vm.BIOSVendor)
	assert.Equal(t, "QEMU", vm.Vendor)
	assert.Empty(t, vm.Serial, "QEMU leaves the serial unset")

	assert.Empty(t, cleanDMI("To Be Filled By O.E.M."))
	assert.Empty(t, cleanDMI("DIMM_P0_A0_AssetTag"))
	assert.Empty(t, cleanDMI("00000000"))
}

func TestFormFactor(t *testing.T) {
	cases := []struct {
		name string
		virt string
		sys  clusterv1.SystemFacts
		want string
	}{
		{"hypervisor wins over the chassis it fakes", "kvm", clusterv1.SystemFacts{ChassisType: "Rack Mount Chassis"}, "virtual-machine"},
		{"rack server", "none", clusterv1.SystemFacts{ChassisType: "Rack Mount Chassis"}, "rack-server"},
		{"tower", "none", clusterv1.SystemFacts{ChassisType: "Tower"}, "tower"},
		{"raspberry pi from the device tree", "none", clusterv1.SystemFacts{DeviceModel: "Raspberry Pi 5 Model B Rev 1.0"}, "sbc"},
		{"jetson from the device tree", "none", clusterv1.SystemFacts{DeviceModel: "NVIDIA Jetson AGX Orin Developer Kit"}, "jetson"},
		{"nothing to go on", "", clusterv1.SystemFacts{}, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, formFactor(c.virt, c.sys))
		})
	}
}

func TestEthtoolReadsLinkAndDriver(t *testing.T) {
	text := fixture(t, "bottlebrush", "nic-enp193s0f1np1.txt")
	e := parseEthtool(text)

	assert.Equal(t, 25000, e.speedMbps)
	assert.Equal(t, 25000, e.maxMbps)
	assert.Equal(t, "Full", e.duplex)
	assert.Equal(t, "Direct Attach Copper", e.port)
	assert.Equal(t, "mlx5_core", e.driver)
	assert.Equal(t, "0000:c1:00.1", e.busInfo)
	assert.Contains(t, parseUdevModel(text), "BlueField-2")

	copper := parseEthtool(fixture(t, "bottlebrush", "nic-eno1.txt"))
	assert.Equal(t, 1000, copper.maxMbps)
	assert.Equal(t, "Twisted Pair", copper.port)
	assert.Equal(t, "igb", copper.driver)
}

func TestLLDPNamesTheSwitchPortPerNIC(t *testing.T) {
	n, err := parseLLDP([]byte(fixture(t, "node1", "lldp.json")))
	require.NoError(t, err)

	require.Contains(t, n, "eno1")
	assert.Equal(t, "MikroTik 1gbe", n["eno1"].SystemName)
	assert.Equal(t, "ether18", n["eno1"].PortID)
	assert.Equal(t, []string{"bridge", "router"}, n["eno1"].Capabilities)
	assert.Equal(t, n["enp33s0f0np0"].ChassisID, n["enp33s0f1np1"].ChassisID, "both 100G ports land on one switch")
	assert.Equal(t, "qsfp28-7-1", n["enp33s0f0np0"].PortID)
}

// fakeHost lays out a sysfs tree with two NICs under bridges, as dev-prod has:
// no br-vpc, so the overlay rides br-lan.
func fakeHost(t *testing.T) Env {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys")
	nic := func(name, master, speed string) {
		dir := filepath.Join(sys, "class", "net", name)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "device"), 0o755))
		for file, value := range map[string]string{"speed": speed, "operstate": "up", "mtu": "9000", "address": "02:00:00:00:00:01"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(value+"\n"), 0o644))
		}
		require.NoError(t, os.Symlink("../"+master, filepath.Join(dir, "master")))
	}
	bridge := func(name string) {
		dir := filepath.Join(sys, "class", "net", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "mtu"), []byte("9000\n"), 0o644))
	}
	bridge("br-wan")
	bridge("br-lan")
	nic("eno1", "br-wan", "1000")
	nic("enp193s0f1np1", "br-lan", "25000")

	dir := filepath.Join(root, "facts")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for name, src := range map[string]string{
		fileIdentity: "dmi-identity.txt", fileMemory: "dmi-memory.txt", fileProcessor: "dmi-processor.txt",
		"nic-enp193s0f1np1.txt": "nic-enp193s0f1np1.txt", "nic-eno1.txt": "nic-eno1.txt",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(fixture(t, "bottlebrush", src)), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileVirt), []byte("none\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fileEncapIP), []byte("10.10.8.4"), 0o644))

	addrs := map[string][]net.IPNet{
		"br-wan": {{IP: net.IPv4(192, 168, 1, 14), Mask: net.CIDRMask(23, 32)}},
		"br-lan": {{IP: net.IPv4(10, 10, 8, 4), Mask: net.CIDRMask(24, 32)}},
	}
	return Env{
		Dir: dir, SysRoot: sys, ProcRoot: filepath.Join(root, "proc"), EtcRoot: filepath.Join(root, "etc"),
		Node: "bottlebrush",
		LiveLLDP: func(context.Context) ([]byte, error) {
			return nil, errors.New("systemd-networkd is not running")
		},
		Addrs: func(name string) []net.IPNet { return addrs[name] },
	}
}

func TestBuildMapsPlanesToNICsWhenTheOverlayRidesTheLANBridge(t *testing.T) {
	f := Build(context.Background(), fakeHost(t))

	assert.Equal(t, "rack-server", f.FormFactor)
	assert.Equal(t, "R182-Z92-00", f.System.Product)
	assert.Equal(t, sourceUnavailable, f.Sources["lldp"], "no networkd and nothing collected")

	planes := map[string]clusterv1.PlaneFacts{}
	for _, p := range f.Planes {
		planes[p.Plane] = p
	}
	assert.Equal(t, "192.168.1.14", planes["wan"].Address)
	assert.Equal(t, []string{"eno1"}, planes["wan"].NICs)
	assert.Equal(t, "br-lan", planes["vpc"].Bridge, "encap address is on br-lan")
	assert.Equal(t, "10.10.8.4", planes["vpc"].Address)
	assert.Equal(t, []string{"enp193s0f1np1"}, planes["lan"].NICs)

	nics := map[string]clusterv1.NICFacts{}
	for _, n := range f.NICs {
		nics[n.Name] = n
	}
	assert.Equal(t, []string{"lan", "vpc"}, nics["enp193s0f1np1"].Planes)
	assert.Equal(t, 25000, nics["enp193s0f1np1"].SpeedMbps)
	assert.Equal(t, "mlx5_core", nics["enp193s0f1np1"].Driver)
	assert.Nil(t, nics["eno1"].Neighbor)
}

func TestBuildFallsBackToTheCollectedLLDP(t *testing.T) {
	env := fakeHost(t)
	require.NoError(t, os.WriteFile(filepath.Join(env.Dir, fileLLDP),
		[]byte(`{"Neighbors":[{"InterfaceName":"eno1","Neighbors":[{"ChassisID":"aa:bb","PortID":"ether3","SystemName":"core","EnabledCapabilities":4}]}]}`), 0o644))

	f := Build(context.Background(), env)

	assert.Equal(t, sourceOK, f.Sources["lldp"])
	for _, n := range f.NICs {
		if n.Name == "eno1" {
			require.NotNil(t, n.Neighbor)
			assert.Equal(t, "ether3", n.Neighbor.PortID)
		}
	}
}
