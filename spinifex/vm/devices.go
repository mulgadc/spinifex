package vm

import (
	"fmt"
	"strconv"
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

// The volume ID/bus/serial helpers below are shared by the hot-attach path
// (AttachVolume/DetachVolume, over QMP) and the cold-boot path (buildDrives,
// on the QEMU command line) so a data volume's block-graph names cannot
// drift between the two: a name minted here for a hot-plugged volume must
// still resolve after a stop/start relaunches it with the same volume ID.

// VolumeNodeName returns the block-node name (blockdev-add/-blockdev
// node-name, and the -device drive= reference) for a data volume.
func VolumeNodeName(volumeID string) string {
	return fmt.Sprintf("nbd-%s", volumeID)
}

// VolumeDeviceID returns the virtio-blk-pci device id (device_add/-device id=)
// for a data volume. DetachVolume's device_del addresses this exact id, so a
// cold-booted volume must be given the same one an AttachVolume would use.
func VolumeDeviceID(volumeID string) string {
	return fmt.Sprintf("vdisk-%s", volumeID)
}

// VolumeIOThreadID returns the iothread object id (object-add/-object id=)
// backing a data volume's virtio-blk-pci device.
func VolumeIOThreadID(volumeID string) string {
	return fmt.Sprintf("ioth-%s", volumeID)
}

// VolumeSerial returns the virtio-blk serial QEMU exposes in-guest: the
// volume ID with dashes stripped ("vol" + 17 hex = 20 bytes, the virtio-blk
// serial limit). The EBS CSI node plugin matches this via
// /dev/disk/by-id/*<serial>* to locate the device.
func VolumeSerial(volumeID string) string {
	return strings.ReplaceAll(volumeID, "-", "")
}

// HotplugEBSBus returns the PCIe hot-plug root-port bus name for hot-plug
// port. A device on pcie.0 cannot be unplugged, so every data volume —
// hot-attached or cold-booted — must land on one of these ports.
func HotplugEBSBus(port int) string {
	return fmt.Sprintf("hotplug-ebs%d", port)
}

// VolumeBlkDeviceArgs returns the virtio-blk-pci device's "key=value" options
// (everything after the leading driver name) as an ordered slice, shared by
// AttachVolume's QMP device_add (rendered as a map) and buildDrives' -device
// (rendered as a joined command-line string) so the two block-graph shapes
// cannot diverge.
func VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, bus string) []string {
	return []string{
		fmt.Sprintf("id=%s", VolumeDeviceID(volumeID)),
		fmt.Sprintf("drive=%s", nodeName),
		fmt.Sprintf("iothread=%s", iothreadID),
		fmt.Sprintf("serial=%s", VolumeSerial(volumeID)),
		fmt.Sprintf("bus=%s", bus),
	}
}

// VolumeBlkDevice renders a QEMU -device argument for buildDrives' cold-boot
// path: the virtio-blk-pci driver name followed by VolumeBlkDeviceArgs.
func VolumeBlkDevice(volumeID, nodeName, iothreadID, bus string) Device {
	args := append([]string{"virtio-blk-pci"}, VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, bus)...)
	return Device{Value: strings.Join(args, ",")}
}

// VolumeBlkDeviceQMPArgs renders the same virtio-blk-pci arguments as a
// device_add argument map for AttachVolume's QMP hot-attach path.
func VolumeBlkDeviceQMPArgs(volumeID, nodeName, iothreadID, bus string) map[string]any {
	return map[string]any{
		"driver":   "virtio-blk-pci",
		"id":       VolumeDeviceID(volumeID),
		"drive":    nodeName,
		"iothread": iothreadID,
		"serial":   VolumeSerial(volumeID),
		"bus":      bus,
	}
}

// NBDServerOpts holds the server.* fields QEMU's nbd blockdev driver needs,
// broken out of a parsed NBD URI (see utils.ParseNBDURI) so both the QMP
// blockdev-add nested "server" object and the command-line -blockdev
// "server.*" options can be built from the same parse.
type NBDServerOpts struct {
	Type string // "unix" or "inet"
	Path string // set when Type == "unix"
	Host string // set when Type == "inet"
	Port int    // set when Type == "inet"
}

// QMPArg renders the server options as the nested map blockdev-add expects.
func (o NBDServerOpts) QMPArg() map[string]any {
	if o.Type == "unix" {
		return map[string]any{"type": "unix", "path": o.Path}
	}
	return map[string]any{"type": "inet", "host": o.Host, "port": strconv.Itoa(o.Port)}
}

// CommandLineArgs renders the server options as "server.*" command-line
// key=value pairs for -blockdev.
func (o NBDServerOpts) CommandLineArgs() []string {
	if o.Type == "unix" {
		return []string{"server.type=unix", fmt.Sprintf("server.path=%s", o.Path)}
	}
	return []string{"server.type=inet", fmt.Sprintf("server.host=%s", o.Host), fmt.Sprintf("server.port=%d", o.Port)}
}

// VolumeBlockdev renders a -blockdev argument for a data volume's NBD block
// node: the command-line equivalent of blockdev-add, producing a
// monitor-owned node that blockdev-del can later remove (unlike -drive,
// which only creates a BlockBackend with an auto-generated node-name).
func VolumeBlockdev(nodeName string, server NBDServerOpts) Blockdev {
	opts := append([]string{"driver=nbd", fmt.Sprintf("node-name=%s", nodeName)}, server.CommandLineArgs()...)
	opts = append(opts, "export=")
	return Blockdev{Value: strings.Join(opts, ",")}
}
