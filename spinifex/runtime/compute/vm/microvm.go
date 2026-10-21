package vm

// MicroVMDir is where Spinifex installs the kernel, initramfs and firmware
// that system instances boot on the microvm machine.
const MicroVMDir = "/usr/share/spinifex/microvm"

// MicroVMFirmware is the qboot build Spinifex ships in MicroVMDir and passes
// with -bios on every host OS.
const MicroVMFirmware = "qboot.rom"
