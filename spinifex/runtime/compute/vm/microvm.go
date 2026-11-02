package vm

import (
	"fmt"
	"path/filepath"
)

// MicroVMDir is where Spinifex installs the kernel, initramfs and firmware
// that system instances boot on the microvm machine.
const MicroVMDir = "/usr/share/spinifex/microvm"

// MicroVMFirmware is the qboot build Spinifex ships in MicroVMDir and passes
// with -bios on every host OS.
const MicroVMFirmware = "qboot.rom"

// MicroVMMachineType returns the QEMU -M string for the microvm machine.
// pcie=on gives NICs a PCI bus with MSI-X; virtio-mmio's shared level-triggered
// lines stalled a NIC's RX path. isa-serial stays on for per-VM console logs.
func MicroVMMachineType() string {
	return "microvm,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on,pcie=on"
}

// MicroVMSpec is what a caller chooses for a direct-boot microVM. Firmware is
// the qboot path; ImageDir holds vmlinuz and initramfs.cpio.gz.
type MicroVMSpec struct {
	Name         string
	Architecture string
	VCPUs        int
	MemoryMiB    int
	Firmware     string
	ImageDir     string
	LogLevel     int
	// MitigationsOff disables guest CPU vulnerability mitigations. Leave it
	// false for any guest that runs tenant code or holds tenant data.
	MitigationsOff bool
	FwCfg          []FwCfgEntry
	NetDevs        []NetDev
	Devices        []Device
}

// NewMicroVMConfig builds the Config for a direct-boot microvm launch: qboot,
// no UEFI, no drives, no PCIe root ports. Runtime paths (PID file, console
// log, serial socket) are filled in by the launcher.
func NewMicroVMConfig(s MicroVMSpec) Config {
	return Config{
		Name:          s.Name,
		EnableKVM:     true,
		NoGraphic:     true,
		Architecture:  s.Architecture,
		CPUType:       "host,-pmu",
		CPUCount:      s.VCPUs,
		Memory:        s.MemoryMiB,
		MachineType:   MicroVMMachineType(),
		BIOS:          s.Firmware,
		KernelImage:   filepath.Join(s.ImageDir, "vmlinuz"),
		Initrd:        filepath.Join(s.ImageDir, "initramfs.cpio.gz"),
		KernelCmdline: microVMCmdline(s.LogLevel, s.MitigationsOff),
		FwCfg:         s.FwCfg,
		NetDevs:       s.NetDevs,
		Devices:       s.Devices,
	}
}

// microVMCmdline builds the guest kernel command line. Devices sit on the PCIe
// bus, so no virtio_mmio.device= arguments. The fixed flags skip boot probes
// microvm has no hardware for:
//
//	quiet                 suppress info printk on the slow serial console
//	tsc=reliable          skip TSC calibration on KVM
//	no_timer_check        skip the APIC/PIT cross-check; microvm has no PIT
//	reboot=t              triple-fault reboot; skip ACPI and keyboard probes
//	i8042.no{pnp,aux,kbd} microvm has no PS/2; skip i8042 probe timeouts
func microVMCmdline(logLevel int, mitigationsOff bool) string {
	cmdline := fmt.Sprintf("console=ttyS0 quiet loglevel=%d", logLevel)
	if mitigationsOff {
		cmdline += " mitigations=off"
	}
	return cmdline + " tsc=reliable no_timer_check reboot=t i8042.nopnp i8042.noaux i8042.nokbd"
}
