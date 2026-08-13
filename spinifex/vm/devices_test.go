package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVolumeSerial(t *testing.T) {
	tests := []struct {
		volumeID string
		want     string
	}{
		{volumeID: "vol-0123456789abcdef0", want: "vol0123456789abcdef0"},
		{volumeID: "vol-many-dashes-here", want: "volmanydasheshere"},
		{volumeID: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.volumeID, func(t *testing.T) {
			assert.Equal(t, tc.want, VolumeSerial(tc.volumeID))
		})
	}
}

func TestIsMMIO(t *testing.T) {
	tests := []struct {
		machineType string
		want        bool
	}{
		{"microvm", true},
		{"microvm,x-option-roms=off", true},
		{"q35", false},
		{"virt", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.machineType, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMMIO(tt.machineType))
		})
	}
}

func TestNetDevice_PCI(t *testing.T) {
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 1)
	assert.Equal(t, "virtio-net-pci,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestNetDevice_PCI_NoMAC(t *testing.T) {
	d := NetDevice("q35", "net0", "", 0)
	assert.Equal(t, "virtio-net-pci,netdev=net0", d.Value)
}

func TestNetDevice_MMIO(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc", 1)
	assert.Equal(t, "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestNetDevice_MMIO_NoMAC(t *testing.T) {
	d := NetDevice("microvm,x-option-roms=off", "net0", "", 0)
	assert.Equal(t, "virtio-net-device,netdev=net0", d.Value)
}

// vectors must be 2N+2 (N rx + N tx + config + control). Understating it makes
// QEMU silently fall back to fewer queues, so the arithmetic is pinned here.
func TestNetDevice_PCI_Multiqueue(t *testing.T) {
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 4)
	assert.Equal(t, "virtio-net-pci,netdev=net0,mac=02:00:00:aa:bb:cc,mq=on,vectors=10", d.Value)
}

// MMIO has no MSI-X, so a queue count must not produce mq/vectors there.
func TestNetDevice_MMIO_IgnoresQueues(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc", 4)
	assert.Equal(t, "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestTapNetDev_SingleQueue(t *testing.T) {
	nd := TapNetDev("net0", "tapabc", 1)
	assert.Equal(t, "tap,id=net0,ifname=tapabc,script=no,downscript=no,vhost=on", nd.Value)
}

func TestTapNetDev_Multiqueue(t *testing.T) {
	nd := TapNetDev("net0", "tapabc", 4)
	assert.Equal(t, "tap,id=net0,ifname=tapabc,script=no,downscript=no,vhost=on,queues=4", nd.Value)
}

// vhost=on is the whole point of the change: without it QEMU copies every
// packet on its main loop and the guest is capped around 1.7 Gbit/s.
func TestTapNetDev_AlwaysEnablesVhost(t *testing.T) {
	for _, queues := range []int{0, 1, 2, 8} {
		nd := TapNetDev("net0", "tapabc", queues)
		assert.Contains(t, nd.Value, "vhost=on", "queues=%d", queues)
	}
}

func TestNICQueues(t *testing.T) {
	tests := []struct {
		vcpus int
		want  int
	}{
		{-1, 1}, {0, 1}, {1, 1}, {2, 2}, {4, 4}, {8, 8},
		{16, MaxNICQueues}, {96, MaxNICQueues},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, NICQueues(tt.vcpus), "vcpus=%d", tt.vcpus)
	}
}

func TestBlkDevice_PCI(t *testing.T) {
	d := BlkDevice("q35", "os", "ioth-os", 4, 1)
	assert.Equal(t, "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=4,bootindex=1", d.Value)
}

func TestBlkDevice_PCI_NoBootIdx(t *testing.T) {
	d := BlkDevice("q35", "data", "ioth-data", 2, 0)
	assert.Equal(t, "virtio-blk-pci,drive=data,iothread=ioth-data,num-queues=2", d.Value)
}

func TestBlkDevice_PCI_NoIOThread(t *testing.T) {
	d := BlkDevice("q35", "data", "", 0, 0)
	assert.Equal(t, "virtio-blk-pci,drive=data", d.Value)
}

func TestBlkDevice_MMIO(t *testing.T) {
	d := BlkDevice("microvm", "os", "ioth-os", 4, 1)
	// MMIO has no bootindex
	assert.Equal(t, "virtio-blk-device,drive=os,iothread=ioth-os,num-queues=4", d.Value)
}

func TestBlkDevice_MMIO_NoIOThread(t *testing.T) {
	d := BlkDevice("microvm,x-option-roms=off", "data", "", 0, 0)
	assert.Equal(t, "virtio-blk-device,drive=data", d.Value)
}

func TestRngDevice_PCI(t *testing.T) {
	d := RngDevice("q35")
	assert.Equal(t, "virtio-rng-pci", d.Value)
}

func TestRngDevice_MMIO(t *testing.T) {
	d := RngDevice("microvm")
	assert.Equal(t, "virtio-rng-device", d.Value)
}

func TestRngDevice_MMIO_WithOptions(t *testing.T) {
	d := RngDevice("microvm,x-option-roms=off,pic=off")
	assert.Equal(t, "virtio-rng-device", d.Value)
}

// TestVolumeBlkDevice_OnErrorReport pins the cold-boot data-volume device
// string to werror=report,rerror=report, mirroring the boot drive's existing
// on-error policy so a full backend reports ENOSPC to the guest instead of
// pausing the VM.
func TestVolumeBlkDevice_OnErrorReport(t *testing.T) {
	d := VolumeBlkDevice("vol-data-a", "nbd-vol-data-a", "ioth-vol-data-a", "hotplug-ebs3")
	assert.Equal(t, "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs3,werror=report,rerror=report", d.Value)
}

// TestVolumeBlkDeviceQMPArgs_OnErrorReport is the hotplug counterpart: the
// QMP device_add argument map AttachVolume sends must carry the same policy.
func TestVolumeBlkDeviceQMPArgs_OnErrorReport(t *testing.T) {
	args := VolumeBlkDeviceQMPArgs("vol-data-a", "nbd-vol-data-a", "ioth-vol-data-a", "hotplug-ebs3")
	assert.Equal(t, "report", args["werror"])
	assert.Equal(t, "report", args["rerror"])
}
