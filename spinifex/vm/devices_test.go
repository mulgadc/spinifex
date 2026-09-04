package vm

import (
	"encoding/json"
	"strings"
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
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 1, 0)
	assert.Equal(t, "virtio-net-pci,netdev=net0,mac=02:00:00:aa:bb:cc,rx_queue_size=1024", d.Value)
}

func TestNetDevice_PCI_NoMAC(t *testing.T) {
	d := NetDevice("q35", "net0", "", 0, 0)
	assert.Equal(t, "virtio-net-pci,netdev=net0,rx_queue_size=1024", d.Value)
}

func TestNetDevice_MMIO(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc", 1, 0)
	assert.Equal(t, "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:cc", d.Value)
}

func TestNetDevice_MMIO_NoMAC(t *testing.T) {
	d := NetDevice("microvm,x-option-roms=off", "net0", "", 0, 0)
	assert.Equal(t, "virtio-net-device,netdev=net0", d.Value)
}

// QEMU caps tx_queue_size at 256 for every backend but vhost-user/vhost-vdpa,
// and rejects a larger value instead of clamping it, so emitting one at all
// fails the launch with "must be a power of 2 between 256 and 256".
func TestNetDevice_NeverSetsTxQueueSize(t *testing.T) {
	for _, machineType := range []string{"q35", "microvm"} {
		for _, queues := range []int{0, 1, 4, 8} {
			d := NetDevice(machineType, "net0", "02:00:00:aa:bb:cc", queues, 1442)
			assert.NotContains(t, d.Value, "tx_queue_size",
				"machineType=%s queues=%d", machineType, queues)
		}
	}
}

// vectors must be 2N+2 (N rx + N tx + config + control). Understating it makes
// QEMU silently fall back to fewer queues, so the arithmetic is pinned here.
func TestNetDevice_PCI_Multiqueue(t *testing.T) {
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 4, 0)
	assert.Equal(t, "virtio-net-pci,netdev=net0,mac=02:00:00:aa:bb:cc,rx_queue_size=1024,mq=on,vectors=10", d.Value)
}

// MMIO has no MSI-X, so a queue count must not produce mq/vectors there.
func TestNetDevice_MMIO_IgnoresQueues(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc", 4, 0)
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

// vhost=on is unconditional: without it QEMU copies every packet on its main
// loop, measured at 60% of a core under load against 3% with it.
func TestTapNetDev_AlwaysEnablesVhost(t *testing.T) {
	for _, queues := range []int{0, 1, 2, 8} {
		nd := TapNetDev("net0", "tapabc", queues)
		assert.Contains(t, nd.Value, "vhost=on", "queues=%d", queues)
	}
}

// Disabled means one queue whatever the vCPU count: behind IPsec, extra queues
// only reorder packets, which TCP reads as loss.
func TestNICQueues_SingleQueueWhenDisabled(t *testing.T) {
	for _, vcpus := range []int{-1, 0, 1, 2, 4, 8, 96} {
		assert.Equal(t, 1, NICQueues(vcpus, false), "vcpus=%d", vcpus)
	}
}

func TestNICQueues_ClampedWhenEnabled(t *testing.T) {
	tests := []struct {
		vcpus int
		want  int
	}{
		{-1, 1}, {0, 1}, {1, 1}, {2, 2}, {4, 4}, {8, 8},
		{16, MaxNICQueues}, {96, MaxNICQueues},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, NICQueues(tt.vcpus, true), "vcpus=%d", tt.vcpus)
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

// TestVolumeBlkDevice_OnErrorStop pins the cold-boot data-volume device string
// to werror=stop,rerror=stop. Reporting the error handed a backend outage to
// the guest as EIO, which aborts its journal; stop holds the request instead.
func TestVolumeBlkDevice_OnErrorStop(t *testing.T) {
	d := VolumeBlkDevice("vol-data-a", "nbd-vol-data-a", "ioth-vol-data-a", "hotplug-ebs3")
	assert.Equal(t, "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs3,werror=stop,rerror=stop", d.Value)
}

// TestVolumeBlkDeviceQMPArgs_OnErrorStop is the hotplug counterpart: the QMP
// device_add argument map AttachVolume sends must carry the same policy.
func TestVolumeBlkDeviceQMPArgs_OnErrorStop(t *testing.T) {
	args := VolumeBlkDeviceQMPArgs("vol-data-a", "nbd-vol-data-a", "ioth-vol-data-a", "hotplug-ebs3")
	assert.Equal(t, "stop", args["werror"])
	assert.Equal(t, "stop", args["rerror"])
}

// host_mtu is how a guest that never takes a DHCP lease — statically addressed,
// or IPv6-only — still learns the overlay ceiling.
func TestNetDevice_HostMTU(t *testing.T) {
	d := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 1, 1442)
	assert.Contains(t, d.Value, "host_mtu=1442")

	off := NetDevice("q35", "net0", "02:00:00:aa:bb:cc", 1, 0)
	assert.NotContains(t, off.Value, "host_mtu", "zero must omit the option, not emit host_mtu=0")
}

// The ring depth is a packet-rate knob, so it must reach an MMIO guest's peer
// too — but MMIO has no MSI-X, and the queue-size options ride the PCI device.
func TestNetDevice_MMIO_NoQueueSizes(t *testing.T) {
	d := NetDevice("microvm", "net0", "02:00:00:aa:bb:cc", 4, 1442)
	assert.NotContains(t, d.Value, "rx_queue_size")
	assert.NotContains(t, d.Value, "mq=on")
	assert.Contains(t, d.Value, "host_mtu=1442")
}

func TestBlkIOThreadPoolSize(t *testing.T) {
	// The threshold sits at 4 vCPUs and the ceiling at 4 threads, matching the
	// only configurations Red Hat's two write-ups actually recommend.
	for _, tc := range []struct{ vcpus, want int }{
		{1, 1}, {2, 1}, {3, 1}, {4, 2}, {6, 3}, {8, 4}, {16, 4}, {192, 4},
	} {
		if got := BlkIOThreadPoolSize(tc.vcpus); got != tc.want {
			t.Errorf("BlkIOThreadPoolSize(%d) = %d, want %d", tc.vcpus, got, tc.want)
		}
	}
}

func TestBlkIOThreadID(t *testing.T) {
	// A pool of one keeps the unsuffixed name, so an existing guest's command
	// line does not change when this code lands.
	if got := BlkIOThreadID("ioth-os", 0, 1); got != "ioth-os" {
		t.Errorf("pool of 1 = %q, want ioth-os", got)
	}
	if got := BlkIOThreadID("ioth-os", 1, 2); got != "ioth-os-1" {
		t.Errorf("pool of 2 index 1 = %q, want ioth-os-1", got)
	}
}

func TestBlkDeviceMapped_SingleThreadUnchanged(t *testing.T) {
	// Byte-for-byte identical to the scalar form: this is the path every guest
	// below 4 vCPUs takes, so a difference here is a silent behaviour change.
	want := BlkDevice("q35", "os", "ioth-os", 2, 1)
	for _, pool := range []int{0, 1} {
		got, err := BlkDeviceMapped("q35", "os", "ioth-os", 2, 1, pool)
		if err != nil {
			t.Fatalf("pool %d: %v", pool, err)
		}
		if got.Value != want.Value {
			t.Errorf("pool %d = %q, want %q", pool, got.Value, want.Value)
		}
	}
}

func TestBlkDeviceMapped_MMIOFallsBack(t *testing.T) {
	// microvm has no PCI bus, so the JSON form names a driver it cannot use.
	got, err := BlkDeviceMapped("microvm", "os", "ioth-os", 4, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Value, "virtio-blk-device") {
		t.Errorf("MMIO = %q, want the virtio-blk-device form", got.Value)
	}
}

func TestBlkDeviceMapped_EveryQueueMappedOnce(t *testing.T) {
	for _, tc := range []struct{ queues, pool int }{{4, 2}, {4, 4}, {8, 4}, {2, 4}, {3, 2}} {
		got, err := BlkDeviceMapped("q35", "os", "ioth-os", tc.queues, 1, tc.pool)
		if err != nil {
			t.Fatalf("queues=%d pool=%d: %v", tc.queues, tc.pool, err)
		}
		var dev struct {
			Driver    string `json:"driver"`
			NumQueues int    `json:"num-queues"`
			IOThread  string `json:"iothread"`
			Mapping   []struct {
				IOThread string `json:"iothread"`
				VQs      []int  `json:"vqs"`
			} `json:"iothread-vq-mapping"`
		}
		if err := json.Unmarshal([]byte(got.Value), &dev); err != nil {
			t.Fatalf("queues=%d pool=%d: not valid JSON: %v (%s)", tc.queues, tc.pool, err, got.Value)
		}
		// The scalar property and the mapping are alternative assignments;
		// QEMU rejects a device carrying both.
		if dev.IOThread != "" {
			t.Errorf("queues=%d pool=%d: scalar iothread= present alongside the mapping", tc.queues, tc.pool)
		}
		if dev.NumQueues != tc.queues {
			t.Errorf("queues=%d pool=%d: num-queues=%d", tc.queues, tc.pool, dev.NumQueues)
		}
		seen := map[int]int{}
		for _, m := range dev.Mapping {
			for _, q := range m.VQs {
				seen[q]++
			}
		}
		for q := 0; q < tc.queues; q++ {
			if seen[q] != 1 {
				t.Errorf("queues=%d pool=%d: vq %d mapped %d times, want exactly 1 (%s)",
					tc.queues, tc.pool, q, seen[q], got.Value)
			}
		}
		if len(seen) != tc.queues {
			t.Errorf("queues=%d pool=%d: %d distinct queues mapped", tc.queues, tc.pool, len(seen))
		}
		// A pool larger than the queue count would declare IOThread objects
		// that no mapping entry references, which QEMU rejects.
		if want := min(tc.pool, tc.queues); len(dev.Mapping) != want {
			t.Errorf("queues=%d pool=%d: %d mapping entries, want %d", tc.queues, tc.pool, len(dev.Mapping), want)
		}
	}
}
