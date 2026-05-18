package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc")
	assert.Equal(t, "virtio-net-pci,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestNetDevice_PCI_NoMAC(t *testing.T) {
	d := NetDevice("q35", "net0", "")
	assert.Equal(t, "virtio-net-pci,netdev=net0", d.Value)
}

func TestNetDevice_MMIO(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc")
	assert.Equal(t, "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestNetDevice_MMIO_NoMAC(t *testing.T) {
	d := NetDevice("microvm,x-option-roms=off", "net0", "")
	assert.Equal(t, "virtio-net-device,netdev=net0", d.Value)
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
