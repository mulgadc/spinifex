package vm

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
)

// IsMicrovm reports whether machineType is the QEMU microvm machine, with or
// without a PCIe bus.
func IsMicrovm(machineType string) bool {
	return strings.HasPrefix(machineType, "microvm")
}

// IsMMIO reports whether machineType attaches virtio devices over memory-mapped
// I/O. A microvm without pcie=on has no PCI bus, so it uses virtio-*-device
// transports; with pcie=on devices are virtio-*-pci like any other machine.
func IsMMIO(machineType string) bool {
	return IsMicrovm(machineType) && !slices.Contains(strings.Split(machineType, ","), "pcie=on")
}

// MaxNICQueues caps the queue pairs on a guest NIC. vhost-net spawns a kernel
// thread per queue, so an unbounded count would put 64 of them behind a large
// instance for throughput a handful already saturates.
const MaxNICQueues = 8

// NICQueues returns the queue-pair count for a NIC on a guest with vcpus
// vCPUs, clamped to [1, MaxNICQueues]. The guest's virtio_net negotiates
// min(vcpus, queues), so matching vCPUs lets every core drive its own queue.
//
// Whether multiqueue helps depends on what is downstream, and the sign flips.
// Behind IPsec it costs ~12%: ESP funnels a node pair onto one core, so extra
// queues only reorder packets, which TCP reads as loss. Without it the sending
// host's single vhost thread saturates first and queues are worth +54%.
func NICQueues(vcpus int, multiqueue bool) int {
	if !multiqueue || vcpus < 1 {
		return 1
	}
	return min(vcpus, MaxNICQueues)
}

// TapNetDev returns the QEMU -netdev argument for a tap-backed NIC.
//
// vhost=on moves the datapath into a kernel vhost-net thread; without it QEMU
// copies every packet on its main loop, measured at 60% of a core under load
// against 3% with it. queues > 1 needs a tap created multi_queue.
func TapNetDev(id, ifname string, queues int) NetDev {
	var b strings.Builder
	fmt.Fprintf(&b, "tap,id=%s,ifname=%s,script=no,downscript=no,vhost=on", id, ifname)
	if queues > 1 {
		fmt.Fprintf(&b, ",queues=%d", queues)
	}
	return NetDev{Value: b.String()}
}

// NICRxQueueSize is the virtio-net receive ring depth, in descriptors. QEMU's
// default of 256 is a packet-rate ceiling rather than a memory saving: a vCPU
// preempted for a fraction of a millisecond fills all 256 slots and the
// overflow is dropped. 1024 is the maximum and what RHEV and Proxmox ship.
//
// There is deliberately no transmit equivalent. QEMU caps tx_queue_size at 256
// for every backend except vhost-user and vhost-vdpa, and rejects a larger
// value outright rather than clamping it, which fails the launch.
const NICRxQueueSize = 1024

// NetDevice returns the appropriate QEMU virtio-net device string for
// machineType, wiring netdev and mac. mac is omitted when empty.
//
// mtu > 0 sets VIRTIO_NET_F_MTU, giving the guest a device-level ceiling that
// does not depend on it taking a DHCP lease. It complements the DHCP option
// rather than replacing it — statically addressed and IPv6-only guests never
// see the lease — and is immutable while the device is live, so a cluster MTU
// change still needs a stop/start to reach a running guest.
//
// queues > 1 enables multiqueue, which needs one MSI-X vector per rx and tx
// queue plus one for config and one for control — 2N+2. Understating vectors
// silently drops the NIC back to fewer queues, so it is derived here rather
// than passed in. MMIO machines have no MSI-X and are left single-queue.
func NetDevice(machineType, netdev, mac string, queues, mtu int) Device {
	var b strings.Builder
	if IsMMIO(machineType) {
		b.WriteString("virtio-net-device")
	} else {
		b.WriteString("virtio-net-pci")
	}
	fmt.Fprintf(&b, ",netdev=%s", netdev)
	if mac != "" {
		fmt.Fprintf(&b, ",mac=%s", mac)
	}
	if mtu > 0 {
		fmt.Fprintf(&b, ",host_mtu=%d", mtu)
	}
	if !IsMMIO(machineType) {
		fmt.Fprintf(&b, ",rx_queue_size=%d", NICRxQueueSize)
		if queues > 1 {
			fmt.Fprintf(&b, ",mq=on,vectors=%d", 2*queues+2)
		}
	}
	return Device{Value: b.String()}
}

// Blk IOThread pool sizing. One IOThread per virtio-blk device services every
// virtqueue on it, so a guest with several queues still funnels through a
// single host thread. QEMU 9.0+ can map queues across a pool instead.
//
// The pool is deliberately much smaller than the vCPU count. Red Hat's RHEL 9.4
// measurements used 4 IOThreads against 192 vCPUs and 96 queues, and OpenShift
// Virtualization documents 4 as the starting point with 8-16 reserved for very
// fast local storage — which an NBD export backed by object storage is not.
const (
	// BlkIOThreadsPerVCPU divides the vCPU count. The threshold lands at 4
	// vCPUs, matching the guest size below which neither source expects the
	// feature to do anything.
	BlkIOThreadsPerVCPU = 2

	// MaxBlkIOThreads caps the pool. These are real host threads charged to
	// the node, and a per-device pool multiplies by the volume count.
	MaxBlkIOThreads = 4
)

// BlkIOThreadPoolSize returns how many IOThreads to create for one virtio-blk
// device on a guest with vcpus processors: vcpus/2, clamped to 1..4. A result
// of 1 reproduces the single-IOThread command line exactly.
func BlkIOThreadPoolSize(vcpus int) int {
	n := vcpus / BlkIOThreadsPerVCPU
	if n < 1 {
		return 1
	}
	if n > MaxBlkIOThreads {
		return MaxBlkIOThreads
	}
	return n
}

// BlkIOThreadID names the i'th IOThread in a device's pool. The single-thread
// case keeps the original unsuffixed name so an existing guest's command line
// is unchanged.
func BlkIOThreadID(base string, i, poolSize int) string {
	if poolSize <= 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, i)
}

// iothreadVQMapping is one entry of virtio-blk's iothread-vq-mapping list.
// Marshalled rather than concatenated: the property is a QAPI struct list and
// the value has to be valid JSON, so hand-built strings are a silent-corruption
// risk for no gain.
type iothreadVQMapping struct {
	IOThread string `json:"iothread"`
	VQs      []int  `json:"vqs"`
}

// blkDeviceJSON is the -device value for a virtio-blk device carrying an
// iothread-vq-mapping. QEMU accepts a JSON object as one argv value, which is
// the only command-line form that can express a nested list.
type blkDeviceJSON struct {
	Driver    string              `json:"driver"`
	Drive     string              `json:"drive"`
	NumQueues int                 `json:"num-queues,omitempty"`
	BootIndex int                 `json:"bootindex,omitempty"`
	Mapping   []iothreadVQMapping `json:"iothread-vq-mapping"`
	Werror    string              `json:"werror"`
	Rerror    string              `json:"rerror"`
}

// BlkDeviceMapped returns a virtio-blk -device that spreads its virtqueues
// across a pool of poolSize IOThreads, round-robin. Falls back to BlkDevice's
// scalar iothread= form when poolSize <= 1, when there is one queue, or on an
// MMIO machine.
//
// Every queue is mapped exactly once and the scalar iothread= property is
// absent: QEMU treats the two as alternative assignments and rejects both.
func BlkDeviceMapped(machineType, drive, iothreadBase string, queues, bootIdx, poolSize int) (Device, error) {
	if poolSize <= 1 || queues <= 1 || IsMMIO(machineType) {
		return BlkDevice(machineType, drive, iothreadBase, queues, bootIdx), nil
	}
	if poolSize > queues {
		poolSize = queues
	}

	vqs := make([][]int, poolSize)
	for q := range queues {
		vqs[q%poolSize] = append(vqs[q%poolSize], q)
	}
	mapping := make([]iothreadVQMapping, 0, poolSize)
	for i := range vqs {
		mapping = append(mapping, iothreadVQMapping{
			IOThread: BlkIOThreadID(iothreadBase, i, poolSize),
			VQs:      vqs[i],
		})
	}

	b, err := json.Marshal(blkDeviceJSON{
		Driver:    "virtio-blk-pci",
		Drive:     drive,
		NumQueues: queues,
		BootIndex: bootIdx,
		Mapping:   mapping,
		Werror:    "stop",
		Rerror:    "stop",
	})
	if err != nil {
		return Device{}, fmt.Errorf("marshal virtio-blk iothread-vq-mapping: %w", err)
	}
	return Device{Value: string(b)}, nil
}

// BlkDevice returns the virtio-blk device string for machineType, omitting an
// empty iothread or zero queues; bootIdx is PCI-only. It sets werror/rerror=stop
// (see VolumeBlkDeviceArgs), as a -blockdev node has no BlockBackend to hold it.
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
	b.WriteString(",werror=stop,rerror=stop")
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
//
// werror=stop,rerror=stop pauses the VM and holds a failed request, so a
// transient backend loss is resumed with cont instead of reaching the guest as
// EIO. A -blockdev node has no BlockBackend to carry it, so the device sets it.
func VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, bus string) []string {
	return []string{
		fmt.Sprintf("id=%s", VolumeDeviceID(volumeID)),
		fmt.Sprintf("drive=%s", nodeName),
		fmt.Sprintf("iothread=%s", iothreadID),
		fmt.Sprintf("serial=%s", VolumeSerial(volumeID)),
		fmt.Sprintf("bus=%s", bus),
		"werror=stop",
		"rerror=stop",
	}
}

// VolumeBlkDevice renders a QEMU -device argument for buildDrives' cold-boot
// path: the virtio-blk-pci driver name followed by VolumeBlkDeviceArgs.
func VolumeBlkDevice(volumeID, nodeName, iothreadID, bus string) Device {
	args := append([]string{"virtio-blk-pci"}, VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, bus)...)
	return Device{Value: strings.Join(args, ",")}
}

// volumeMMIOBlkDevice renders a microvm data volume as virtio-blk-device with
// VolumeBlkDeviceArgs' identity and error policy; MMIO has no PCI bus to name.
func volumeMMIOBlkDevice(volumeID, nodeName, iothreadID string) Device {
	args := []string{"virtio-blk-device"}
	for _, arg := range VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, "") {
		if !strings.HasPrefix(arg, "bus=") {
			args = append(args, arg)
		}
	}
	return Device{Value: strings.Join(args, ",")}
}

// volumeMicrovmPCIBlkDevice renders a data volume on a microvm with a PCIe bus
// as virtio-blk-pci with VolumeBlkDeviceArgs' identity and error policy. The
// microvm has no hot-plug root ports, so the device sits on the default bus.
func volumeMicrovmPCIBlkDevice(volumeID, nodeName, iothreadID string) Device {
	args := []string{"virtio-blk-pci"}
	for _, arg := range VolumeBlkDeviceArgs(volumeID, nodeName, iothreadID, "") {
		if !strings.HasPrefix(arg, "bus=") {
			args = append(args, arg)
		}
	}
	return Device{Value: strings.Join(args, ",")}
}

// VolumeBlkDeviceQMPArgs renders the same virtio-blk-pci arguments as a
// device_add argument map for AttachVolume's QMP hot-attach path. See
// VolumeBlkDeviceArgs for why werror/rerror are set here.
func VolumeBlkDeviceQMPArgs(volumeID, nodeName, iothreadID, bus string) map[string]any {
	return map[string]any{
		"driver":   "virtio-blk-pci",
		"id":       VolumeDeviceID(volumeID),
		"drive":    nodeName,
		"iothread": iothreadID,
		"serial":   VolumeSerial(volumeID),
		"bus":      bus,
		"werror":   "stop",
		"rerror":   "stop",
	}
}

// NBDReconnectDelaySeconds is how long an NBD disconnect pauses requests before
// they start failing. It covers a storage process restarting on the same socket;
// past it the error surfaces and werror=stop pauses the VM instead.
const NBDReconnectDelaySeconds = 30

// NBDServerOpts holds the server.* fields QEMU's nbd blockdev driver needs,
// broken out of a parsed NBD URI (see nbd.ParseNBDURI) so both the QMP
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
	opts := append([]string{"driver=nbd", fmt.Sprintf("node-name=%s", nodeName)}, nbdNodeOpts("", server)...)
	return Blockdev{Value: strings.Join(opts, ",")}
}

// nbdNodeOpts returns an nbd node's server, export and reconnect-delay options,
// each prefixed with prefix: "" on the nbd node itself, "file." under raw.
func nbdNodeOpts(prefix string, server NBDServerOpts) []string {
	var opts []string
	for _, arg := range server.CommandLineArgs() {
		opts = append(opts, prefix+arg)
	}
	return append(opts, prefix+"export=", fmt.Sprintf("%sreconnect-delay=%d", prefix, NBDReconnectDelaySeconds))
}

// bootNodeName is the boot volume's block-node name, which its -device names
// with drive=.
const bootNodeName = "os"

// nbdServerOpts parses an NBD URI into the server options a blockdev takes.
func nbdServerOpts(uri string) (NBDServerOpts, error) {
	serverType, socketPath, host, port, err := nbd.ParseNBDURI(uri)
	if err != nil {
		return NBDServerOpts{}, err
	}
	return NBDServerOpts{Type: serverType, Path: socketPath, Host: host, Port: port}, nil
}

// bootNBDBlockdev renders the boot volume's -blockdev for an NBD export: a raw
// node over an nbd node, the graph -drive format=raw,cache=none built, with the
// same O_DIRECT setting and reconnect delay.
func bootNBDBlockdev(nodeName string, server NBDServerOpts) Blockdev {
	opts := append([]string{"driver=raw", fmt.Sprintf("node-name=%s", nodeName), "cache.direct=on", "file.driver=nbd"}, nbdNodeOpts("file.", server)...)
	return Blockdev{Value: strings.Join(opts, ",")}
}

// exportBlockdev renders v's -blockdev as nodeName: a host_device node for a
// block export, otherwise the NBD node nbdBlockdev builds from its URI.
func exportBlockdev(v EBSRequest, kind volumeExportKind, nodeName string, nbdBlockdev func(string, NBDServerOpts) Blockdev) (Blockdev, error) {
	if kind == volumeExportBlock {
		return VolumeHostDeviceBlockdev(nodeName, HostDeviceOpts{Path: v.BlockDevice, SizeBytes: v.BlockSizeBytes}), nil
	}
	server, err := nbdServerOpts(v.NBDURI)
	if err != nil {
		return Blockdev{}, fmt.Errorf("parse NBDURI for volume %s: %w", v.Name, err)
	}
	return nbdBlockdev(nodeName, server), nil
}

// HostDeviceOpts is a host block device export. QEMU opens Path through the
// host_device protocol driver with O_DIRECT and native AIO, and a raw format
// node over it exposes only the first SizeBytes to the guest.
type HostDeviceOpts struct {
	Path      string
	SizeBytes uint64
}

// QMPArgs renders the blockdev-add arguments for the raw node and its nested
// host_device file node.
func (o HostDeviceOpts) QMPArgs(nodeName string) map[string]any {
	return map[string]any{
		"node-name": nodeName,
		"driver":    "raw",
		"size":      o.SizeBytes,
		"file": map[string]any{
			"driver":   "host_device",
			"filename": o.Path,
			"aio":      "native",
			"cache":    map[string]any{"direct": true},
		},
	}
}

// VolumeHostDeviceBlockdev renders the -blockdev equivalent of QMPArgs for a
// volume backed by a host block device.
func VolumeHostDeviceBlockdev(nodeName string, dev HostDeviceOpts) Blockdev {
	return Blockdev{Value: fmt.Sprintf(
		"driver=raw,node-name=%s,size=%d,file.driver=host_device,file.filename=%s,file.aio=native,file.cache.direct=on",
		nodeName, dev.SizeBytes, dev.Path)}
}
