package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMicroVMDirectBoot asserts that a microvm Config with direct-boot fields
// set produces the expected QEMU args, including the explicit firmware, and
// does not include PCI-specific devices such as pcie-root-port.
func TestMicroVMDirectBoot(t *testing.T) {
	cfg := Config{
		CPUCount:      2,
		Memory:        512,
		Architecture:  "x86_64",
		MachineType:   "microvm,x-option-roms=off,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on",
		BIOS:          "/path/qboot.rom",
		KernelImage:   "/path/vmlinuz",
		Initrd:        "/path/initramfs.cpio.gz",
		KernelCmdline: "console=ttyS0 panic=1",
		FwCfg: []FwCfgEntry{
			{Name: "opt/spinifex/netcfg", File: "/tmp/nc.tmp"},
		},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)
	require.NotNil(t, cmd)

	args := cmd.Args[1:]

	// Machine type must be set
	assert.Equal(t, "microvm,x-option-roms=off,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on", argValue(args, "-M"))

	// Direct-boot flags must be present
	assert.Equal(t, "/path/qboot.rom", argValue(args, "-bios"))
	assert.Equal(t, "/path/vmlinuz", argValue(args, "-kernel"))
	assert.Equal(t, "/path/initramfs.cpio.gz", argValue(args, "-initrd"))
	assert.Equal(t, "console=ttyS0 panic=1", argValue(args, "-append"))
	assert.Equal(t, "name=opt/spinifex/netcfg,file=/tmp/nc.tmp", argValue(args, "-fw_cfg"))

	// PCI-specific devices must NOT appear
	for _, a := range args {
		assert.NotContains(t, a, "pcie-root-port", "pcie-root-port must not appear for microvm")
	}
}

func TestNewMicroVMConfig_MitigationsOnByDefault(t *testing.T) {
	cfg := NewMicroVMConfig(MicroVMSpec{Name: "i-default", LogLevel: 3})
	assert.NotContains(t, cfg.KernelCmdline, "mitigations")
}

func TestNewMicroVMConfig_MitigationsOffOnlyWhenNamed(t *testing.T) {
	cfg := NewMicroVMConfig(MicroVMSpec{Name: "i-optout", LogLevel: 3, MitigationsOff: true})
	assert.Contains(t, cfg.KernelCmdline, " mitigations=off ")
}
