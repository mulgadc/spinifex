package vm

import (
	"fmt"
	"strings"
)

// IsMMIO reports whether machineType is a memory-mapped I/O bus machine (e.g.
// microvm). MMIO machines use virtio-*-device transports instead of
// virtio-*-pci, because they have no PCI bus.
func IsMMIO(machineType string) bool {
	return strings.HasPrefix(machineType, "microvm")
}

// NetDevice returns the appropriate QEMU virtio-net device string for
// machineType, wiring netdev and mac. mac is omitted when empty.
func NetDevice(machineType, netdev, mac string) Device {
	var transport string
	if IsMMIO(machineType) {
		transport = "virtio-net-device"
	} else {
		transport = "virtio-net-pci"
	}
	if mac != "" {
		return Device{Value: fmt.Sprintf("%s,netdev=%s,mac=%s", transport, netdev, mac)}
	}
	return Device{Value: fmt.Sprintf("%s,netdev=%s", transport, netdev)}
}

// BlkDevice returns the appropriate QEMU virtio-blk device string for
// machineType. iothread and queues are omitted when empty/zero. bootIdx is
// only emitted for PCI machines (MMIO has no bootindex concept).
func BlkDevice(machineType, drive, iothread string, queues int, bootIdx int) Device {
	var b strings.Builder
	if IsMMIO(machineType) {
		b.WriteString("virtio-blk-device")
	} else {
		b.WriteString("virtio-blk-pci")
	}
	fmt.Fprintf(&b, ",drive=%s", drive)
	if iothread != "" {
		fmt.Fprintf(&b, ",iothread=%s", iothread)
	}
	if queues > 0 {
		fmt.Fprintf(&b, ",num-queues=%d", queues)
	}
	if !IsMMIO(machineType) && bootIdx > 0 {
		fmt.Fprintf(&b, ",bootindex=%d", bootIdx)
	}
	return Device{Value: b.String()}
}

// RngDevice returns the appropriate QEMU virtio-rng device string for
// machineType.
func RngDevice(machineType string) Device {
	if IsMMIO(machineType) {
		return Device{Value: "virtio-rng-device"}
	}
	return Device{Value: "virtio-rng-pci"}
}
