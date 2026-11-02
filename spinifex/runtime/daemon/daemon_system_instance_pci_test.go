package daemon

import (
	"os"
	"testing"

	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pciTestInput() *elbv2domain.SystemInstanceInput {
	return &elbv2domain.SystemInstanceInput{
		ENIID: "eni-pci0000000000001",
		NICs: []elbv2domain.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: "02:aa:bb:cc:dd:02"},
			{MAC: "02:aa:bb:cc:dd:03"},
		},
	}
}

func assertPCINICs(t *testing.T, cfg vm.Config) {
	t.Helper()
	assert.Contains(t, cfg.MachineType, "microvm,")
	assert.Contains(t, cfg.MachineType, "pcie=on")
	require.Len(t, cfg.Devices, 3)
	for i, mac := range []string{"02:aa:bb:cc:dd:01", "02:aa:bb:cc:dd:02", "02:aa:bb:cc:dd:03"} {
		assert.Contains(t, cfg.Devices[i].Value, "virtio-net-pci,netdev=net", "NIC %d must be a PCI device", i)
		assert.NotContains(t, cfg.Devices[i].Value, "virtio-net-device")
		assert.Contains(t, cfg.Devices[i].Value, "mac="+mac, "MAC order must be unchanged")
	}
	assert.NotContains(t, cfg.KernelCmdline, "virtio_mmio.device=")
}

func TestBuildDirectBootConfig_LBNICsArePCI(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	d := &Daemon{resourceMgr: &ResourceManager{}}

	cfg, err := d.buildDirectBootConfig("i-pci-lb", pciTestInput(), nil)
	require.NoError(t, err)
	assertPCINICs(t, cfg)
	assert.Contains(t, cfg.KernelCmdline, "loglevel=7", "LB guest kernel warnings must reach the console log")
	for _, e := range cfg.FwCfg {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestBuildDirectBootConfig_SQSWorkerNICsArePCI(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	d := &Daemon{resourceMgr: &ResourceManager{}}
	w := &sqsWorkerLaunch{volumeID: "vol-pci", region: "ap-southeast-2", gatewayURL: "https://gw.example:9999", caCert: "ca"}

	cfg, err := d.buildDirectBootConfig("i-pci-sqs", pciTestInput(), w)
	require.NoError(t, err)
	assertPCINICs(t, cfg)
	for _, e := range cfg.FwCfg {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestMicrovmMachineType_HasPCIE(t *testing.T) {
	assert.Contains(t, microvmMachineType(), "pcie=on")
	assert.Contains(t, microvmMachineType(), "isa-serial=on")
}
