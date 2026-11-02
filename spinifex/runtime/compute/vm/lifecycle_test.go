package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/gpu"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestBuildBaseVMConfig pins the non-pass-through invariants buildBaseVMConfig
// must enforce: KVM + no-graphic, q35 + host CPU regardless of arch, and
// pre-allocated PCIe root ports for hot-plug (Linux's PCIe hot-plug requires
// pre-allocated ports — removing one silently breaks AttachVolume/AttachENI).
//
// Default instanceType ("") falls through to instancetypes.defaultMaxENIs (4)
// → 3 hot-plug-eni slots, alongside the fixed 11 hot-plug-ebs slots.
func TestBuildBaseVMConfig(t *testing.T) {
	for _, arch := range []string{"x86_64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			cfg := buildBaseVMConfig("i-x", "", "/tmp/x.pid", "/tmp/x.log", "/tmp/x.sock", arch, "", 2, 4096)

			assert.True(t, cfg.EnableKVM)
			assert.True(t, cfg.NoGraphic)
			assert.Equal(t, "q35", cfg.MachineType)
			assert.Equal(t, "host", cfg.CPUType)
			assert.False(t, cfg.UseUEFI, "empty bootMode must default to BIOS")

			// 11 EBS slots + 3 ENI slots (default fallback) = 14 root ports.
			require.Len(t, cfg.Devices, 14)
			for i := range EBSHotPlugSlotCount {
				expected := fmt.Sprintf("pcie-root-port,id=hotplug-ebs%d,chassis=%d,slot=0", i+1, i+1)
				assert.Equal(t, expected, cfg.Devices[i].Value)
			}
			for i := range 3 {
				idx := EBSHotPlugSlotCount + i
				expected := fmt.Sprintf("pcie-root-port,id=hotplug-eni%d,chassis=%d,slot=0", i+1, EBSHotPlugSlotCount+i+1)
				assert.Equal(t, expected, cfg.Devices[idx].Value)
			}
		})
	}
}

// TestBuildBaseVMConfig_ENISlotCountPerType pins the per-instance-type ENI
// hot-plug pool sizing. The slot count must equal MaxENIs - 1; the chassis
// numbers must continue from EBSHotPlugSlotCount+1 without colliding.
func TestBuildBaseVMConfig_ENISlotCountPerType(t *testing.T) {
	tests := []struct {
		instanceType string
		wantENISlots int
	}{
		{"t3.nano", 2},
		{"t3a.2xlarge", 2},
		{"m5.large", 2},
		{"m5.2xlarge", 3},
		{"m5.4xlarge", 7},
		{"m5.8xlarge", 14},
		{"", 3}, // unknown → defaultMaxENIs (4) - 1
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			cfg := buildBaseVMConfig("i-x", tc.instanceType, "/tmp/x.pid", "/tmp/x.log", "/tmp/x.sock", "x86_64", "", 2, 4096)
			require.Len(t, cfg.Devices, EBSHotPlugSlotCount+tc.wantENISlots)
			for i := range tc.wantENISlots {
				idx := EBSHotPlugSlotCount + i
				expected := fmt.Sprintf("pcie-root-port,id=hotplug-eni%d,chassis=%d,slot=0", i+1, EBSHotPlugSlotCount+i+1)
				assert.Equal(t, expected, cfg.Devices[idx].Value)
			}
			assert.Equal(t, tc.instanceType, cfg.InstanceType)
		})
	}
}

// TestBuildBaseVMConfig_BootMode pins the bootMode → UseUEFI mapping.
// "uefi" and AWS's "uefi-preferred" both flip the firmware flag; "bios" and
// any unrecognised value (including "") fall through as BIOS.
func TestBuildBaseVMConfig_BootMode(t *testing.T) {
	tests := []struct {
		bootMode    string
		wantUseUEFI bool
	}{
		{"", false},
		{"bios", false},
		{"uefi", true},
		{"uefi-preferred", true},
		{"garbage", false},
	}
	for _, tc := range tests {
		t.Run(tc.bootMode, func(t *testing.T) {
			cfg := buildBaseVMConfig("i-x", "", "/tmp/x.pid", "/tmp/x.log", "/tmp/x.sock", "x86_64", tc.bootMode, 2, 4096)
			assert.Equal(t, tc.wantUseUEFI, cfg.UseUEFI)
		})
	}
}

func TestBuildDrives(t *testing.T) {
	tests := []struct {
		name          string
		requests      []EBSRequest
		cpuCount      int
		machineType   string
		wantDrives    []Drive
		wantIOThreads []IOThread
		wantDevices   []Device
		wantBlockdevs []Blockdev
		// wantHotplugPorts, when non-nil, asserts requests[i].HotplugPort
		// after the call — pinning both the "already recorded, left alone"
		// and "unset, allocated and written back" behaviours.
		wantHotplugPorts []int
		wantErr          string
	}{
		{
			// 2 vCPUs resolves to a pool of one, which must keep the scalar
			// iothread= form: this is the shape every small guest boots with.
			name: "boot volume, pool of one keeps the scalar iothread",
			requests: []EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
			},
			cpuCount: 2,
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=unix,file.server.path=/tmp/boot.sock,file.export=,file.reconnect-delay=30"},
			},
			wantIOThreads: []IOThread{{ID: "ioth-os"}},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1,werror=stop,rerror=stop"},
			},
		},
		{
			// 4 vCPUs resolves to a pool of two, so the four virtqueues are
			// split across two host threads rather than served by one.
			name: "boot volume, four vCPUs map their queues across a pool of two",
			requests: []EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
			},
			cpuCount: 4,
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=unix,file.server.path=/tmp/boot.sock,file.export=,file.reconnect-delay=30"},
			},
			wantIOThreads: []IOThread{{ID: "ioth-os-0"}, {ID: "ioth-os-1"}},
			wantDevices: []Device{
				{Value: `{"driver":"virtio-blk-pci","drive":"os","num-queues":4,"bootindex":1,"iothread-vq-mapping":[{"iothread":"ioth-os-0","vqs":[0,2]},{"iothread":"ioth-os-1","vqs":[1,3]}],"werror":"stop","rerror":"stop"}`},
			},
		},
		{
			name: "EFI volume emits pflash unit=1",
			requests: []EBSRequest{
				{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true},
			},
			cpuCount: 2,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/efi.sock", Format: "raw", If: "pflash", Unit: 1},
			},
		},
		{
			name: "missing NBDURI returns error",
			requests: []EBSRequest{
				{Name: "vol-bad"},
			},
			cpuCount: 2,
			wantErr:  "NBDURI not set for volume vol-bad",
		},
		{
			name: "missing NBDURI on EFI returns error",
			requests: []EBSRequest{
				{Name: "vol-efi-bad", EFI: true},
			},
			cpuCount: 2,
			wantErr:  "NBDURI not set for volume vol-efi-bad",
		},
		{
			name: "data volume with a recorded HotplugPort keeps it and emits a named blockdev+device",
			requests: []EBSRequest{
				{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock", HotplugPort: 3},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-vol-data-a"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/tmp/data-a.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs3,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{3},
		},
		{
			name: "data volume with HotplugPort unset gets the lowest free port allocated and written back",
			requests: []EBSRequest{
				{Name: "vol-data-b", NBDURI: "nbd:unix:/tmp/data-b.sock"},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-vol-data-b"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=nbd,node-name=nbd-vol-data-b,server.type=unix,server.path=/tmp/data-b.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,id=vdisk-vol-data-b,drive=nbd-vol-data-b,iothread=ioth-vol-data-b,serial=voldatab,bus=hotplug-ebs1,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{1},
		},
		{
			name: "two data volumes with HotplugPort unset in one call get distinct ports",
			requests: []EBSRequest{
				{Name: "vol-data-c", NBDURI: "nbd:unix:/tmp/data-c.sock"},
				{Name: "vol-data-d", NBDURI: "nbd:unix:/tmp/data-d.sock"},
			},
			cpuCount: 2,
			wantIOThreads: []IOThread{
				{ID: "ioth-vol-data-c"},
				{ID: "ioth-vol-data-d"},
			},
			wantBlockdevs: []Blockdev{
				{Value: "driver=nbd,node-name=nbd-vol-data-c,server.type=unix,server.path=/tmp/data-c.sock,export=,reconnect-delay=30"},
				{Value: "driver=nbd,node-name=nbd-vol-data-d,server.type=unix,server.path=/tmp/data-d.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,id=vdisk-vol-data-c,drive=nbd-vol-data-c,iothread=ioth-vol-data-c,serial=voldatac,bus=hotplug-ebs1,werror=stop,rerror=stop"},
				{Value: "virtio-blk-pci,id=vdisk-vol-data-d,drive=nbd-vol-data-d,iothread=ioth-vol-data-d,serial=voldatad,bus=hotplug-ebs2,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{1, 2},
		},
		{
			name: "data volume errors when the hot-plug port pool is exhausted",
			requests: func() []EBSRequest {
				reqs := make([]EBSRequest, 0, EBSHotPlugSlotCount+1)
				for i := 1; i <= EBSHotPlugSlotCount; i++ {
					reqs = append(reqs, EBSRequest{
						Name: fmt.Sprintf("vol-filler-%d", i), NBDURI: "nbd:unix:/tmp/filler.sock", HotplugPort: i,
					})
				}
				reqs = append(reqs, EBSRequest{Name: "vol-overflow", NBDURI: "nbd:unix:/tmp/overflow.sock"})
				return reqs
			}(),
			cpuCount: 2,
			wantErr:  "no free EBS hot-plug port for volume vol-overflow",
		},
		{
			name: "mixed boot + EFI + data: boot is a -blockdev node and EFI keeps the pflash -drive",
			requests: []EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
				{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true},
				{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock", HotplugPort: 3},
			},
			// 2 vCPUs, so the boot disk keeps the legacy scalar shape and this
			// case still asserts what its name says it does. Data volumes are
			// unaffected by the pool either way — they keep one IOThread each.
			cpuCount: 2,
			wantDrives: []Drive{
				{File: "nbd:unix:/tmp/efi.sock", Format: "raw", If: "pflash", Unit: 1},
			},
			wantIOThreads: []IOThread{
				{ID: "ioth-os"},
				{ID: "ioth-vol-data-a"},
			},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=unix,file.server.path=/tmp/boot.sock,file.export=,file.reconnect-delay=30"},
				{Value: "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/tmp/data-a.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1,werror=stop,rerror=stop"},
				{Value: "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs3,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{0, 0, 3},
		},
		{
			name: "block data volume emits a raw node over host_device with the same device wiring",
			requests: []EBSRequest{
				{Name: "vol-blk", BlockDevice: "/dev/sdx", BlockSizeBytes: 10 << 30, HotplugPort: 2},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-vol-blk"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=nbd-vol-blk,size=10737418240,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,id=vdisk-vol-blk,drive=nbd-vol-blk,iothread=ioth-vol-blk,serial=volblk,bus=hotplug-ebs2,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{2},
		},
		{
			// microvm has no PCIe root ports, so an unset port stays unset
			// rather than naming a bus the machine does not have.
			name: "microvm NBD data volume renders an MMIO device and allocates no hot-plug port",
			requests: []EBSRequest{
				{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock"},
			},
			cpuCount:      2,
			machineType:   "microvm,x-option-roms=off,pic=off",
			wantIOThreads: []IOThread{{ID: "ioth-vol-data-a"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/tmp/data-a.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-device,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{0},
		},
		{
			name: "microvm block data volume keeps its recorded port and host_device node on an MMIO device",
			requests: []EBSRequest{
				{Name: "vol-blk", BlockDevice: "/dev/sdx", BlockSizeBytes: 10 << 30, HotplugPort: 2},
			},
			cpuCount:      2,
			machineType:   "microvm",
			wantIOThreads: []IOThread{{ID: "ioth-vol-blk"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=nbd-vol-blk,size=10737418240,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-device,id=vdisk-vol-blk,drive=nbd-vol-blk,iothread=ioth-vol-blk,serial=volblk,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{2},
		},
		{
			// q35 is not a microvm: the boundary must stay on PCI allocation.
			name: "q35 data volume with HotplugPort unset still gets a PCI hot-plug port",
			requests: []EBSRequest{
				{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock"},
			},
			cpuCount:      2,
			machineType:   "q35",
			wantIOThreads: []IOThread{{ID: "ioth-vol-data-a"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/tmp/data-a.sock,export=,reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs1,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{1},
		},
		{
			name: "NBD boot volume alongside a block data volume",
			requests: []EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true},
				{Name: "vol-blk", BlockDevice: "/dev/sdx", BlockSizeBytes: 1 << 20},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-os"}, {ID: "ioth-vol-blk"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=unix,file.server.path=/tmp/boot.sock,file.export=,file.reconnect-delay=30"},
				{Value: "driver=raw,node-name=nbd-vol-blk,size=1048576,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1,werror=stop,rerror=stop"},
				{Value: "virtio-blk-pci,id=vdisk-vol-blk,drive=nbd-vol-blk,iothread=ioth-vol-blk,serial=volblk,bus=hotplug-ebs1,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{0, 1},
		},
		{
			name: "block boot volume is a raw node over host_device named os under the bootindex device",
			requests: []EBSRequest{
				{Name: "vol-boot", BlockDevice: "/dev/sdx", BlockSizeBytes: 8 << 30, Boot: true},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-os"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,size=8589934592,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1,werror=stop,rerror=stop"},
			},
			wantHotplugPorts: []int{0},
		},
		{
			name: "block boot volume with four vCPUs keeps the mapped device and error policy",
			requests: []EBSRequest{
				{Name: "vol-boot", BlockDevice: "/dev/sdx", BlockSizeBytes: 8 << 30, Boot: true},
			},
			cpuCount:      4,
			wantIOThreads: []IOThread{{ID: "ioth-os-0"}, {ID: "ioth-os-1"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,size=8589934592,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
			},
			wantDevices: []Device{
				{Value: `{"driver":"virtio-blk-pci","drive":"os","num-queues":4,"bootindex":1,"iothread-vq-mapping":[{"iothread":"ioth-os-0","vqs":[0,2]},{"iothread":"ioth-os-1","vqs":[1,3]}],"werror":"stop","rerror":"stop"}`},
			},
		},
		{
			name: "NBD boot volume over TCP renders inet server options under file.",
			requests: []EBSRequest{
				{Name: "vol-boot", NBDURI: "nbd://127.0.0.1:44801", Boot: true},
			},
			cpuCount:      2,
			wantIOThreads: []IOThread{{ID: "ioth-os"}},
			wantBlockdevs: []Blockdev{
				{Value: "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=inet,file.server.host=127.0.0.1,file.server.port=44801,file.export=,file.reconnect-delay=30"},
			},
			wantDevices: []Device{
				{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1,werror=stop,rerror=stop"},
			},
		},
		{
			name:     "unparseable NBD boot URI is rejected",
			requests: []EBSRequest{{Name: "vol-boot", NBDURI: "http://example/boot", Boot: true}},
			cpuCount: 2,
			wantErr:  "parse NBDURI for volume vol-boot",
		},
		{
			name:     "block EFI volume is rejected",
			requests: []EBSRequest{{Name: "vol-efi", BlockDevice: "/dev/sdx", BlockSizeBytes: 1 << 20, EFI: true}},
			cpuCount: 2,
			wantErr:  "volume vol-efi: block device export is not supported for an EFI volume",
		},
		{
			name:     "both NBDURI and BlockDevice set is rejected",
			requests: []EBSRequest{{Name: "vol-both", NBDURI: "nbd:unix:/tmp/x.sock", BlockDevice: "/dev/sdx", BlockSizeBytes: 1 << 20}},
			cpuCount: 2,
			wantErr:  "volume vol-both has both NBDURI and BlockDevice set",
		},
		{
			name:     "NBDURI with a stray BlockSizeBytes is rejected",
			requests: []EBSRequest{{Name: "vol-stray", NBDURI: "nbd:unix:/tmp/x.sock", BlockSizeBytes: 1 << 20}},
			cpuCount: 2,
			wantErr:  "volume vol-stray has BlockSizeBytes set without BlockDevice",
		},
		{
			name:     "block device with zero size is rejected",
			requests: []EBSRequest{{Name: "vol-nosize", BlockDevice: "/dev/sdx"}},
			cpuCount: 2,
			wantErr:  `volume vol-nosize BlockDevice "/dev/sdx" has zero BlockSizeBytes`,
		},
		{
			name:     "relative block device path is rejected",
			requests: []EBSRequest{{Name: "vol-rel", BlockDevice: "dev/sdx", BlockSizeBytes: 1 << 20}},
			cpuCount: 2,
			wantErr:  `volume vol-rel BlockDevice "dev/sdx" is not an absolute path`,
		},
		{
			name:     "block device path with a comma is rejected",
			requests: []EBSRequest{{Name: "vol-comma", BlockDevice: "/dev/sdx,file.locking=off", BlockSizeBytes: 1 << 20}},
			cpuCount: 2,
			wantErr:  `volume vol-comma BlockDevice "/dev/sdx,file.locking=off" contains a comma`,
		},
		{
			name:     "empty requests",
			requests: []EBSRequest{},
			cpuCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machineType := tt.machineType
			if machineType == "" {
				machineType = "q35"
			}
			cfg, err := buildDrives(tt.requests, tt.cpuCount, machineType)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantDrives, cfg.Drives)
			assert.Equal(t, tt.wantIOThreads, cfg.IOThreads)
			assert.Equal(t, tt.wantDevices, cfg.Devices)
			assert.Equal(t, tt.wantBlockdevs, cfg.Blockdevs)

			if tt.wantHotplugPorts != nil {
				require.Len(t, tt.requests, len(tt.wantHotplugPorts))
				for i, want := range tt.wantHotplugPorts {
					assert.Equal(t, want, tt.requests[i].HotplugPort,
						"request %d (%s) HotplugPort", i, tt.requests[i].Name)
				}
			}
		})
	}
}

func TestTapDeviceName(t *testing.T) {
	tests := []struct {
		name string
		eni  string
		want string
	}{
		{"short ENI", "eni-abc123", "tapabc123"},
		{"prefix-only", "abc123", "tapabc123"},
		{"truncate to 15", "eni-abc123def456789", "tapabc123def456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TapDeviceName(tt.eni))
		})
	}
}

func TestGenerateDevMAC_Stable(t *testing.T) {
	a := GenerateDevMAC("i-abc123")
	b := GenerateDevMAC("i-abc123")
	assert.Equal(t, a, b, "MAC must be deterministic for the same instance ID")
	assert.NotEqual(t, GenerateDevMAC("i-abc123"), GenerateDevMAC("i-def456"))
}

func TestGenerateMgmtMAC_Stable(t *testing.T) {
	a := GenerateMgmtMAC("i-abc123")
	b := GenerateMgmtMAC("i-abc123")
	assert.Equal(t, a, b, "MAC must be deterministic for the same instance ID")
	assert.NotEqual(t, GenerateMgmtMAC("i-abc123"), GenerateMgmtMAC("i-def456"))
	// Dev and mgmt MACs for the same instance must differ — class separation.
	assert.NotEqual(t, GenerateDevMAC("i-abc123"), GenerateMgmtMAC("i-abc123"))
}

func TestStartReturnsErrorWhenInstanceUnknown(t *testing.T) {
	m := NewManager()
	err := m.Start(t.Context(), "i-missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "i-missing")
}

// launchTestManager wires the minimum deps needed to drive Manager.launch
// far enough to exercise the early-exit paths and the VolumeMounter call
// site. Returns the manager, the mounter (for assertions and onMount
// hooks), and a counter of OnInstanceUp invocations.
//
// The success-path "OnInstanceUp fires exactly once" assertion is not
// driven from a vm-package unit test — the launch happy path requires a
// real QEMU process plus a working QMP socket (startQEMU + AttachQMP),
// and the heartbeat goroutine spawned by AttachQMP blocks on a 30s sleep
// with no cancellation seam, so a hermetic test would either leak the
// heartbeat or stall for 30s. The "exactly once" contract is covered by
// the daemon-side e2e tests that drive the full RunInstances pipeline.
// The counter is retained so the negative direction (no fire on aborted
// or failed launches) can be asserted alongside the existing tests.
func launchTestManager(t *testing.T) (*Manager, *fakeVolumeMounter, *int) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	mounter := &fakeVolumeMounter{}
	upCalls := 0
	m := NewManager()
	m.SetDeps(Deps{
		VolumeMounter: mounter,
		Hooks: ManagerHooks{
			OnInstanceUp: func(*VM) error { upCalls++; return nil },
		},
	})
	return m, mounter, &upCalls
}

// TestRun_LaunchStillValid_FirstCheck covers the first launchStillValid
// race-check (lifecycle.go:77): when a concurrent terminate has flipped
// status out of {Pending, Stopped, Provisioning} before Run starts, Run
// must return nil immediately without mounting volumes or firing the
// OnInstanceUp hook — the terminate goroutine owns cleanup.
func TestRun_LaunchStillValid_FirstCheck(t *testing.T) {
	for _, status := range []InstanceState{
		StateRunning, StateStopping, StateShuttingDown, StateTerminated, StateError,
	} {
		t.Run(string(status), func(t *testing.T) {
			m, mounter, upCalls := launchTestManager(t)
			instance := &VM{ID: "i-" + string(status), Status: status}
			m.Insert(instance)

			err := m.Run(t.Context(), instance)

			require.NoError(t, err)
			assert.Empty(t, mounter.mounted, "Mount must not be called when launch is aborted by first race check")
			assert.Equal(t, 0, *upCalls, "OnInstanceUp must not fire when launch is aborted")
			assert.Equal(t, status, m.Status(instance), "status must not change")
		})
	}
}

// TestRun_LaunchStillValid_SecondCheck covers the second launchStillValid
// race-check (lifecycle.go:102): a terminate races in during Mount (which
// can take 30+s on cold AMIs) and flips status to ShuttingDown. Run must
// return nil after Mount completes, without proceeding to startQEMU or
// firing OnInstanceUp.
func TestRun_LaunchStillValid_SecondCheck(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	instance := &VM{ID: "i-flip", Status: StatePending}
	m.Insert(instance)

	mounter.onMount = func(v *VM) {
		m.UpdateState(v.ID, func(vv *VM) { vv.Status = StateShuttingDown })
	}

	err := m.Run(t.Context(), instance)

	require.NoError(t, err, "Run must return nil when concurrent terminate flips status during Mount")
	assert.Equal(t, []string{"i-flip"}, mounter.mounted, "Mount must run exactly once before the second race check")
	assert.Equal(t, 0, *upCalls, "OnInstanceUp must not fire when the second race check aborts launch")
	assert.Equal(t, StateShuttingDown, m.Status(instance))
}

// TestRun_VolumeMounterError_Propagates covers the Mount-failure branch
// (lifecycle.go:93): the error must bubble up unchanged, no startQEMU, no
// hook fires, and the manager state is unaffected.
func TestRun_VolumeMounterError_Propagates(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	sentinel := errors.New("mount failed")
	mounter.mountErr = sentinel

	instance := &VM{ID: "i-mount-fail", Status: StatePending}
	m.Insert(instance)

	err := m.Run(t.Context(), instance)

	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, []string{"i-mount-fail"}, mounter.mounted)
	assert.Equal(t, 0, *upCalls)
	assert.Equal(t, StatePending, m.Status(instance), "status must be unchanged on Mount failure")
}

// TestRun_VolumeStateFailureRollsBackBeforeQEMU verifies launch does not expose
// a writer when authoritative attachment state cannot be persisted.
func TestRun_VolumeStateFailureRollsBackBeforeQEMU(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	stateErr := errors.New("predastore unavailable")
	mounter := &fakeVolumeMounter{}
	stateUpdater := &fakeVolumeStateUpdater{err: stateErr}
	m := NewManagerWithDeps(Deps{
		VolumeMounter:      mounter,
		VolumeStateUpdater: stateUpdater,
	})
	instance := &VM{
		ID:     "i-state-fail",
		Status: StatePending,
		EBSRequests: EBSRequests{Requests: []EBSRequest{{
			Name: "vol-1", DeviceName: "/dev/sda1", Boot: true,
		}}},
	}
	m.Insert(instance)

	err := m.Run(t.Context(), instance)

	require.Error(t, err)
	assert.ErrorIs(t, err, stateErr)
	assert.Equal(t, []string{instance.ID}, mounter.mounted)
	assert.Equal(t, []string{instance.ID}, mounter.unmounted,
		"failed attachment-state persistence must roll back mounted volumes")
	calls := stateUpdater.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "in-use", calls[0].State)
	assert.Equal(t, instance.ID, calls[0].InstanceID)
	assert.NoFileExists(t, filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), instance.ID+".pid"),
		"startQEMU must not run after attachment-state persistence fails")
}

// TestRun_AlreadyRunningPID_ReturnsError covers the live-PID guard
// (lifecycle.go:81-91): when the PID file refers to a live process, Run
// must return an error before touching volumes or firing hooks.
func TestRun_AlreadyRunningPID_ReturnsError(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)

	instance := &VM{ID: "i-live-pid", Status: StatePending}
	m.Insert(instance)

	pidDir := os.Getenv("XDG_RUNTIME_DIR")
	pidFile := filepath.Join(pidDir, instance.ID+".pid")
	require.NoError(t, os.WriteFile(pidFile, fmt.Appendf(nil, "%d", os.Getpid()), 0o600))

	err := m.Run(t.Context(), instance)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already running")
	assert.Empty(t, mounter.mounted, "Mount must not run when an existing live PID is detected")
	assert.Equal(t, 0, *upCalls)
}

// TestStart_DispatchesThroughLaunch asserts Start (the found-instance
// branch) actually drives launch — not merely returns nil. A regression
// that made Start a no-op after the lookup would still pass
// TestStartReturnsErrorWhenInstanceUnknown; this test catches it by
// requiring the launch-internal Mount call.
func TestStart_DispatchesThroughLaunch(t *testing.T) {
	m, mounter, _ := launchTestManager(t)
	sentinel := errors.New("mount failed via Start")
	mounter.mountErr = sentinel

	instance := &VM{ID: "i-start-dispatch", Status: StateStopped}
	m.Insert(instance)

	err := m.Start(t.Context(), instance.ID)

	require.ErrorIs(t, err, sentinel, "Start must dispatch through launch and surface Mount errors")
	assert.Equal(t, []string{"i-start-dispatch"}, mounter.mounted)
}

// TestStart_AbortedByConcurrentTerminate is the Start-side mirror of the
// first launchStillValid check: a status set outside the launchable
// allowlist must abort the launch with no error and no Mount call.
func TestStart_AbortedByConcurrentTerminate(t *testing.T) {
	m, mounter, upCalls := launchTestManager(t)
	instance := &VM{ID: "i-start-abort", Status: StateShuttingDown}
	m.Insert(instance)

	err := m.Start(t.Context(), instance.ID)

	require.NoError(t, err)
	assert.Empty(t, mounter.mounted)
	assert.Equal(t, 0, *upCalls)
}

// TestLaunchStillValid_Allowlist locks in the launchStillValid allowlist
// across every defined InstanceState plus the zero value. The parent
// A regression in the allowlist would silently break the
// start-stopped path" — e.g. dropping StateStopped would make
// StartInstances on a stopped VM no-op without surfacing an error.
func TestLaunchStillValid_Allowlist(t *testing.T) {
	tests := []struct {
		status InstanceState
		want   bool
		why    string
	}{
		{StateProvisioning, true, "RunInstances entry: launches before status flip to Pending"},
		{StatePending, true, "RunInstances/restore entry: launch must proceed"},
		{StateStopped, true, "StartInstances re-launches a stopped VM through the same pipeline"},
		{StateRunning, false, "already past launch — second Run is a no-op"},
		{StateStopping, false, "stop in flight: stopCleanup owns teardown"},
		{StateShuttingDown, false, "terminate in flight: terminateCleanup owns teardown"},
		{StateTerminated, false, "terminal"},
		{StateError, false, "RestartCrashedInstance drives transitions, not launch"},
		{InstanceState(""), false, "zero value must reject"},
	}

	m := NewManager()
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			instance := &VM{ID: "i-" + string(tt.status), Status: tt.status}
			got := m.launchStillValid(instance)
			assert.Equal(t, tt.want, got, tt.why)
		})
	}
}

// startBrokenQMPListener accepts on a unix socket and immediately closes
// every connection without sending a greeting, so qmp.NewQMPClient's
// greeting decode fails. Returns the socket path and a stop function the
// caller must invoke before goleak.VerifyNone so the accept goroutine
// drains.
func startBrokenQMPListener(t *testing.T) (string, func()) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = ln.Close()
		<-done
	}
	t.Cleanup(stop)
	return sockPath, stop
}

// TestAttachQMP_DialFailure_NoHeartbeatLeak covers the regression the
// A swap that started qmpHeartbeat before checking
// the factory error would leak one goroutine per failed reconnect. With
// no listener at the configured socket, net.Dial fails inside
// newQMPClientWithHandshake and AttachQMP must return without spawning
// the heartbeat or mutating instance.QMPClient.
func TestAttachQMP_DialFailure_NoHeartbeatLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	orig := qmpSocketWaitTimeout
	qmpSocketWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { qmpSocketWaitTimeout = orig })

	m := NewManager()
	instance := &VM{
		ID:     "i-dial-fail",
		Config: Config{QMPSocket: filepath.Join(t.TempDir(), "missing.sock")},
	}

	err := m.AttachQMP(instance)

	require.Error(t, err)
	assert.Nil(t, instance.QMPClient, "QMPClient must remain unset on dial failure")
}

// TestAttachQMP_HandshakeFailure_NoHeartbeatLeak covers the path where
// the unix socket exists but qmp.NewQMPClient's greeting decode fails
// (server closed). The dial succeeded so the connection must be closed
// before AttachQMP returns, the heartbeat must not start, and
// instance.QMPClient must remain nil.
func TestAttachQMP_HandshakeFailure_NoHeartbeatLeak(t *testing.T) {
	sockPath, stopListener := startBrokenQMPListener(t)

	m := NewManager()
	instance := &VM{ID: "i-handshake-fail", Config: Config{QMPSocket: sockPath}}

	err := m.AttachQMP(instance)

	require.Error(t, err)
	assert.Nil(t, instance.QMPClient, "QMPClient must remain unset on handshake failure")

	stopListener()
	goleak.VerifyNone(t)
}

// stubFindFreePort swaps the package-level findFreePort seam with a
// scripted result list. Exhausting the list returns a sentinel error so
// an under-counted test fails fast instead of falling back to the real
// OS allocator. t.Cleanup restores the original.
func stubFindFreePort(t *testing.T, results ...struct {
	addr string
	err  error
}) *int {
	t.Helper()
	orig := findFreePort
	idx := 0
	findFreePort = func() (string, error) {
		if idx >= len(results) {
			idx++
			return "", errors.New("stubFindFreePort: exhausted")
		}
		r := results[idx]
		idx++
		return r.addr, r.err
	}
	t.Cleanup(func() { findFreePort = orig })
	return &idx
}

func newDevNICInstance(id string, guestPorts ...int) *VM {
	return &VM{ID: id, HostfwdPorts: guestPorts}
}

func TestAppendDevHostfwdNIC_SSHPortFails_NICSkipped(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "", err: errors.New("listen failed")})

	m := NewManager()
	m.SetDeps(Deps{BindHost: "10.0.0.1"})
	instance := newDevNICInstance("i-ssh-fail")

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls, "FindFreePort must be invoked exactly once before bailing")
	assert.Empty(t, instance.Config.NetDevs, "no NetDev appended when SSH port lookup fails")
	assert.Empty(t, instance.Config.Devices, "no virtio-net device appended when SSH port lookup fails")
}

func TestAppendDevHostfwdNIC_SSHAddrUnparsable_NICSkipped(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "nocolon", err: nil})

	m := NewManager()
	instance := newDevNICInstance("i-ssh-bad-addr")

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls)
	assert.Empty(t, instance.Config.NetDevs)
	assert.Empty(t, instance.Config.Devices)
}

func TestAppendDevHostfwdNIC_NoExtraPorts_ShortCircuit(t *testing.T) {
	calls := stubFindFreePort(t, struct {
		addr string
		err  error
	}{addr: "127.0.0.1:2222", err: nil})

	m := NewManager()
	m.SetDeps(Deps{BindHost: "0.0.0.0"})
	instance := newDevNICInstance("i-no-extra")

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 1, *calls, "no extra entries means exactly one FindFreePort call")
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value)
	require.Len(t, instance.Config.Devices, 1)
	assert.Equal(t, fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s,rx_queue_size=1024", GenerateDevMAC("i-no-extra")), instance.Config.Devices[0].Value)
}

func TestAppendDevHostfwdNIC_ExtraPortFails_WarningContinues(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "", err: errors.New("listen failed")},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-fail", 8080)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 2, *calls)
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value, "failed extra entry must not appear in netdev value")
	assert.NotContains(t, instance.HostfwdPortMap, 8080, "a port that never bound must be absent, not mapped to zero")
	require.Len(t, instance.Config.Devices, 1)
}

func TestAppendDevHostfwdNIC_ExtraAddrUnparsable_EntrySkipped(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "garbage-no-colon", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:9090", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-split", 8080, 8443)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 3, *calls)
	require.Len(t, instance.Config.NetDevs, 1)

	// Ports are visited in the order requested, so the unparsable address is
	// consumed by 8080 and 8443 gets the good one.
	assert.NotContains(t, instance.HostfwdPortMap, 8080, "skipped entry must be absent")
	assert.Equal(t, map[int]int{8443: 9090}, instance.HostfwdPortMap)
	assert.Contains(t, instance.Config.NetDevs[0].Value, "hostfwd=tcp:127.0.0.1:9090-:8443")
	assert.NotContains(t, instance.Config.NetDevs[0].Value, "garbage")
}

func TestAppendDevHostfwdNIC_ExtraPortAtoiFails_EntrySkipped(t *testing.T) {
	calls := stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:notanumber", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-atoi", 8080)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, 2, *calls)
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22", instance.Config.NetDevs[0].Value, "non-numeric extra port must not be appended to netdev value")
	assert.NotContains(t, instance.HostfwdPortMap, 8080, "skipped entry must be absent")
}

func TestAppendDevHostfwdNIC_ExtraPorts_HappyPath(t *testing.T) {
	stubFindFreePort(t,
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:2222", err: nil},
		struct {
			addr string
			err  error
		}{addr: "127.0.0.1:18080", err: nil},
	)

	m := NewManager()
	instance := newDevNICInstance("i-extra-ok", 8080)

	m.appendDevHostfwdNIC(instance)

	assert.Equal(t, map[int]int{8080: 18080}, instance.HostfwdPortMap)
	require.Len(t, instance.Config.NetDevs, 1)
	assert.Equal(t, "user,id=dev0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:18080-:8080", instance.Config.NetDevs[0].Value)
	require.Len(t, instance.Config.Devices, 1)
	assert.Equal(t, fmt.Sprintf("virtio-net-pci,netdev=dev0,mac=%s,rx_queue_size=1024", GenerateDevMAC("i-extra-ok")), instance.Config.Devices[0].Value)
}

// ---------------------------------------------------------------------------
// startQEMU — DirectBoot branch
//
// We call startQEMU directly (same package) and force early returns via tap
// errors so we cover the DirectBoot config-assignment lines and the tap setup
// paths without spawning a real QEMU process or sleeping 2 s.
// ---------------------------------------------------------------------------

// directBootManager wires the minimum Deps for startQEMU to reach the
// DirectBoot tap-setup block: InstanceTypes resolver + NetworkPlumber.
func directBootManager(t *testing.T, plumber NetworkPlumber) *Manager {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	resolver := fakeInstanceTypeResolver{
		"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512},
	}
	return NewManagerWithDeps(Deps{
		InstanceTypes:  resolver,
		NetworkPlumber: plumber,
	})
}

// TestStartQEMU_DirectBoot_PrimaryTapError verifies that when the
// NetworkPlumber fails to set up the primary ENI tap, startQEMU returns a
// wrapped error. This also drives the DirectBoot config-assignment block
// (PIDFile / ConsoleLogPath / SerialSocket) before the early return.
func TestStartQEMU_DirectBoot_PrimaryTapError(t *testing.T) {
	tapErr := errors.New("tap setup failed")
	plumber := &fakeNetworkPlumber{setupErr: tapErr}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-tap-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		ENIId:        "eni-000000000000aaaa",
		ENIMac:       "02:aa:bb:cc:dd:01",
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "setup tap device")
	// PIDFile and ConsoleLogPath are set before the tap call.
	// SerialSocket is intentionally empty for direct-boot (file chardev used instead).
	assert.NotEmpty(t, instance.Config.PIDFile)
	assert.NotEmpty(t, instance.Config.ConsoleLogPath)
	assert.Empty(t, instance.Config.SerialSocket)
	// Exactly one SetupTap call (the primary ENI tap).
	require.Len(t, plumber.setupCalls, 1)
}

// TestStartQEMU_DirectBoot_BridgeEnsureError verifies that a failure to ensure
// br-imds aborts the launch before the primary tap is created: the tap would
// otherwise be placed on a missing (or wrong) bridge, black-holing the guest.
func TestStartQEMU_DirectBoot_BridgeEnsureError(t *testing.T) {
	plumber := &fakeNetworkPlumber{ensureBridgeErr: errors.New("add-br failed")}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-bridge-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		ENIId:        "eni-000000000000dddd",
		ENIMac:       "02:aa:bb:cc:dd:04",
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure IMDS bridge")
	// The tap must not be created when the bridge could not be ensured.
	assert.Empty(t, plumber.setupCalls)
}

// TestStartQEMU_DirectBoot_ExtraENITapError verifies that a tap failure on a
// secondary ENI returns an error without starting QEMU.
func TestStartQEMU_DirectBoot_ExtraENITapError(t *testing.T) {
	callCount := 0
	plumber := &fakeNetworkPlumber{}
	// Override SetupTap to fail only on the second call (extra ENI).
	realSetupCalls := &plumber.setupCalls
	_ = realSetupCalls

	// Use a scripted plumber: success for primary, error for extra.
	scriptedPlumber := &scriptedNetworkPlumber{
		errs: []error{nil, errors.New("extra tap failed")},
	}
	m := directBootManager(t, scriptedPlumber)
	_ = callCount

	instance := &VM{
		ID:           "i-db-extra-tap-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		ENIId:        "eni-000000000000bbbb",
		ENIMac:       "02:aa:bb:cc:dd:02",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-000000000000cccc", ENIMac: "02:aa:bb:cc:dd:03"},
		},
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "extra ENI")
	assert.Len(t, scriptedPlumber.calls, 2)
}

// TestStartQEMU_DirectBoot_MgmtTapError verifies that a tap failure on the
// management NIC returns a wrapped error.
func TestStartQEMU_DirectBoot_MgmtTapError(t *testing.T) {
	// No primary ENI → skips the ENI tap block; MgmtMAC set → enters mgmt block.
	plumber := &fakeNetworkPlumber{setupErr: errors.New("mgmt tap failed")}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-mgmt-err",
		InstanceType: "t3.nano",
		DirectBoot:   true,
		MgmtMAC:      "02:aa:bb:cc:dd:ff",
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "mgmt tap")
	require.Len(t, plumber.setupCalls, 1)
	assert.Equal(t, MgmtTapName("i-db-mgmt-err"), plumber.setupCalls[0].Name)
	assert.Equal(t, "br-mgmt", plumber.setupCalls[0].Bridge)
}

// TestStartQEMU_DiskBoot_MgmtTapCreated verifies the disk-boot/restart path
// pre-creates the mgmt tap via SetupTap (owned by the daemon euid) before
// handing it to QEMU, mirroring the direct-boot branch. Without this the
// non-root daemon (no CAP_NET_ADMIN) cannot attach /dev/net/tun, breaking every
// stopped-instance restart including EKS control-plane recovery.
func TestStartQEMU_DiskBoot_MgmtTapCreated(t *testing.T) {
	// No primary ENI → dev user-mode net; MgmtMAC set → enters disk-boot mgmt block.
	plumber := &fakeNetworkPlumber{setupErr: errors.New("mgmt tap failed")}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-disk-mgmt",
		InstanceType: "t3.nano",
		MgmtMAC:      "02:aa:bb:cc:dd:ee",
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "mgmt tap")
	require.Len(t, plumber.setupCalls, 1)
	assert.Equal(t, MgmtTapName("i-disk-mgmt"), plumber.setupCalls[0].Name)
	assert.Equal(t, "br-mgmt", plumber.setupCalls[0].Bridge)
}

// TestStartQEMU_DirectBoot_NoENI_NoMgmt_InstanceTypeNotFound verifies that
// startQEMU returns an error when the instance type is absent from the resolver,
// exercising the early-return before any DirectBoot config is written.
func TestStartQEMU_DirectBoot_InstanceTypeNotFound(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := directBootManager(t, plumber)

	instance := &VM{
		ID:           "i-db-unknown-type",
		InstanceType: "x9.enormous",
		DirectBoot:   true,
	}

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "x9.enormous")
	assert.Empty(t, plumber.setupCalls)
}

// TestStartQEMU_BlockDeviceMissing verifies a block volume's launch readiness
// is the device's presence, not an NBD connect.
func TestStartQEMU_BlockDeviceMissing(t *testing.T) {
	m := directBootManager(t, &fakeNetworkPlumber{})
	missing := filepath.Join(t.TempDir(), "missing-dev")

	instance := directBootInstance("i-blk-missing",
		EBSRequest{Name: "vol-blk", BlockDevice: missing, BlockSizeBytes: 1 << 20})

	err := m.startQEMU(instance, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "block device not present for vol-blk")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// scriptedNetworkPlumber returns errors from a pre-loaded slice, one per call.
type scriptedNetworkPlumber struct {
	calls []TapSpec
	errs  []error
	idx   int
}

func (p *scriptedNetworkPlumber) SetupTap(spec TapSpec) error {
	p.calls = append(p.calls, spec)
	if p.idx < len(p.errs) {
		err := p.errs[p.idx]
		p.idx++
		return err
	}
	return nil
}

func (p *scriptedNetworkPlumber) CleanupTap(_ string) error { return nil }

func (p *scriptedNetworkPlumber) AttachIMDSDatapath(_, _, _ string) error { return nil }

func (p *scriptedNetworkPlumber) DetachIMDSDatapath(_ string) error { return nil }

func (p *scriptedNetworkPlumber) IMDSDatapathStates(_ context.Context, _ []string) (map[string]IMDSDatapathState, error) {
	return nil, nil
}

func (p *scriptedNetworkPlumber) RepairIMDSDatapath(_ context.Context, _, _, _ string) error {
	return nil
}

func (p *scriptedNetworkPlumber) EnsureIMDSDatapathBridge() error { return nil }

func (p *scriptedNetworkPlumber) EnsureVPCHostPort(_, _, _ string) error { return nil }

func (p *scriptedNetworkPlumber) RemoveVPCHostPort(_ string) error { return nil }

var _ NetworkPlumber = (*scriptedNetworkPlumber)(nil)

func TestStartupTimeouts(t *testing.T) {
	assert.Equal(t, 5*time.Second, qemuStartupTimeout)
	assert.Equal(t, 5*time.Second, nbdReadyTimeout)
	assert.Equal(t, 3*time.Second, qmpSocketWaitTimeout,
		"production QMP socket wait default must stay 3s outside tests")
}

// TestRG4_GuestOOMTier asserts the RG-4 OOM ladder: a customer guest QEMU
// (ManagedBy == "") is scored +500 so the kernel reaps it first under host
// memory pressure, while a platform-managed system instance (ManagedBy set —
// ELBv2 LB-VM, EKS control-plane node) is scored 0, ranking above user guests
// but below the infra tier. Regressing either tier widens the OOM blast radius.
func TestRG4_GuestOOMTier(t *testing.T) {
	assert.Equal(t, 500, guestOOMScore(""),
		"RG-4: user guest (no ManagedBy) must be reaped first (+500)")
	assert.Equal(t, 0, guestOOMScore("elbv2"),
		"RG-4: ELBv2 system instance must rank above user guests (0)")
	assert.Equal(t, 0, guestOOMScore("eks"),
		"RG-4: EKS control-plane node must rank above user guests (0)")
	assert.Greater(t, guestOOMScore(""), guestOOMScore("elbv2"),
		"RG-4: a user guest must always be killed before a system instance")
}

// startWorkingQMPListener accepts one connection on a unix socket, sends the
// QMP greeting, then echoes a {"return":{}} reply for every command line the
// client writes (qmp_capabilities and any later command). It models a live
// QEMU QMP monitor closely enough to exercise reconnect. Returns the socket
// path and a stop function the caller must invoke before goleak.VerifyNone.
func startWorkingQMPListener(t *testing.T) (string, func()) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				greeting := `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}` + "\n"
				if _, err := c.Write([]byte(greeting)); err != nil {
					return
				}
				dec := json.NewDecoder(c)
				for {
					var cmd map[string]any
					if err := dec.Decode(&cmd); err != nil {
						return
					}
					if _, err := c.Write([]byte(`{"return":{}}` + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = ln.Close()
		<-done
	}
	t.Cleanup(stop)
	return sockPath, stop
}

// TestSendQMPCommand_ReconnectsWedgedClient covers the EBS-attach flake fix: a
// QMPClient flagged Dead (a prior timed-out decode left the shared Decoder
// mid-message) must redial its socket on the next send, complete the command,
// and clear Dead — instead of failing for the VM's lifetime.
func TestSendQMPCommand_ReconnectsWedgedClient(t *testing.T) {
	sockPath, stop := startWorkingQMPListener(t)
	defer stop()

	client, err := qmp.NewQMPClient(sockPath)
	require.NoError(t, err)
	defer func() { _ = client.Conn.Close() }()

	// Simulate the wedge: mark Dead as a timed-out decode would.
	client.Dead = true

	resp, err := sendQMPCommand(t.Context(), client, qmp.QMPCommand{Execute: "query-status"}, "i-wedged")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.False(t, client.Dead, "reconnect must clear Dead so later commands reuse the fresh stream")

	// A second command must succeed over the reconnected stream.
	_, err = sendQMPCommand(t.Context(), client, qmp.QMPCommand{Execute: "query-status"}, "i-wedged")
	require.NoError(t, err)
}

// TestSendQMPCommand_NoPathCannotReconnect asserts a wedged client with no
// retained socket path surfaces a clear error rather than panicking — the
// reconnect needs the path NewQMPClient now stores.
func TestSendQMPCommand_NoPathCannotReconnect(t *testing.T) {
	sockPath, stop := startWorkingQMPListener(t)
	defer stop()

	client, err := qmp.NewQMPClient(sockPath)
	require.NoError(t, err)
	defer func() { _ = client.Conn.Close() }()

	client.Dead = true
	client.Path = ""

	_, err = sendQMPCommand(t.Context(), client, qmp.QMPCommand{Execute: "query-status"}, "i-nopath")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no socket path")
}

// startSingleClientQMPListener models a real `-qmp unix:...,server,nowait`
// QEMU monitor: single-threaded, serving exactly one connection at a time.
// Unlike startWorkingQMPListener (which spawns a goroutine per accepted
// connection and greets all of them concurrently, hiding the
// reconnect-ordering bug), Accept is only called again once the current
// connection's serve loop returns — so a second connection dialed while the
// first is still open sits unaccepted (and ungreeted) in the kernel backlog,
// exactly like a real single-client monitor, with no lock/goroutine race.
func startSingleClientQMPListener(t *testing.T) (sockPath string, stop func()) {
	t.Helper()
	sockPath = filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			serveSingleClientQMPConn(conn)
		}
	}()

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = ln.Close()
		<-done
	}
	t.Cleanup(stop)
	return sockPath, stop
}

// serveSingleClientQMPConn greets conn, then echoes {"return":{}} for every
// command line until the connection errors or closes. It blocks for the
// life of the connection, which is what makes startSingleClientQMPListener's
// accept loop strictly serial.
func serveSingleClientQMPConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	greeting := `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}` + "\n"
	if _, err := conn.Write([]byte(greeting)); err != nil {
		return
	}
	dec := json.NewDecoder(conn)
	for {
		var cmd map[string]any
		if err := dec.Decode(&cmd); err != nil {
			return
		}
		if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
			return
		}
	}
}

// TestSendQMPCommand_ReconnectsAgainstSingleClientServer is the regression
// test for the reconnectQMP ordering bug against a real QEMU
// `server,nowait` QMP monitor, only one client may hold a connection at a
// time. A wedged client's reconnect must close its own stale connection
// BEFORE dialing fresh, freeing the slot — dialing fresh first (the pre-fix
// ordering) leaves the old connection occupying the only slot, so the new
// dial gets no greeting and reconnect fails, leaving q.Dead permanently
// true and every later attach/detach on the VM erroring.
func TestSendQMPCommand_ReconnectsAgainstSingleClientServer(t *testing.T) {
	sockPath, stop := startSingleClientQMPListener(t)
	defer stop()

	client, err := qmp.NewQMPClient(sockPath)
	require.NoError(t, err)
	defer func() { _ = client.Conn.Close() }()

	// The old connection is still open (not yet closed) when Dead is set —
	// exactly the state a wedged decode leaves behind, and exactly what the
	// pre-fix bare-dial-before-close ordering could not recover from against
	// a single-client monitor.
	client.Dead = true

	resp, err := sendQMPCommand(t.Context(), client, qmp.QMPCommand{Execute: "query-status"}, "i-single-client")
	require.NoError(t, err, "reconnect must close the stale connection before redialing so the single-client monitor's slot is free")
	require.NotNil(t, resp)
	assert.False(t, client.Dead, "reconnect must clear Dead so later commands reuse the fresh stream")

	_, err = sendQMPCommand(t.Context(), client, qmp.QMPCommand{Execute: "query-status"}, "i-single-client")
	require.NoError(t, err, "a second command must succeed over the reconnected stream")
}

// TestQMPGreetingTimeout pins the VFIO scaling: a GPU passthrough guest must
// pin all its RAM through the IOMMU before the monitor answers, so its greeting
// deadline sits at the floor for a small guest and grows with RAM up to the cap;
// a plain VM keeps the default.
func TestQMPGreetingTimeout(t *testing.T) {
	plain := &VM{}
	assert.Equal(t, qmp.DefaultGreetingTimeout, qmpGreetingTimeout(plain),
		"a plain VM keeps the default greeting deadline")

	// A guest with a network-backed root volume, which is every real one. QEMU
	// opens the drive across the object store before its monitor answers, and
	// the default deadline is shorter than a cold open costs on a cluster that
	// has just lost a node — which is the only time a recovery launch happens.
	nbd := &VM{EBSRequests: EBSRequests{Requests: []EBSRequest{
		{Name: "vol-boot", NBDURI: "nbd+unix:///?socket=/run/spinifex/nbd/boot.sock", Boot: true},
	}}}
	assert.Equal(t, qmpNBDGreetingTimeout, qmpGreetingTimeout(nbd),
		"a guest with an NBD drive waits for the volume open, not the plain default")
	assert.Greater(t, qmpNBDGreetingTimeout, 35*time.Second,
		"the deadline must exceed the 35s cold open measured with one node of three away")

	block := &VM{EBSRequests: EBSRequests{Requests: []EBSRequest{
		{Name: "vol-data", BlockDevice: "/dev/sdx", BlockSizeBytes: 1 << 20},
	}}}
	assert.Equal(t, qmp.DefaultGreetingTimeout, qmpGreetingTimeout(block),
		"a host block device opens locally, so it keeps the plain default")

	// A small GPU guest (<1 GiB scaling contribution) sits at the floor.
	smallGPU := &VM{
		GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:5e:00.0"}},
		Config:         Config{Memory: 4096},
	}
	assert.Equal(t, qmp.DefaultGreetingTimeout+qmpVFIOGreetingFloor, qmpGreetingTimeout(smallGPU),
		"a small VFIO guest gets the floor on top of the plain startup it still pays")
	assert.Greater(t, qmpVFIOGreetingFloor, qmp.DefaultGreetingTimeout,
		"the VFIO floor must exceed the plain default")

	// A large GPU guest scales above the floor: 64 GiB -> floor + 64*8s.
	largeGPU := &VM{
		GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:5e:00.0"}},
		Config:         Config{Memory: 64 * 1024},
	}
	assert.Equal(t, qmp.DefaultGreetingTimeout+qmpVFIOGreetingBase+64*qmpVFIOGreetingPerGiB, qmpGreetingTimeout(largeGPU),
		"a large VFIO guest scales its deadline with RAM")

	// An oversized guest is clamped to the cap, not left unbounded.
	hugeGPU := &VM{
		GPUAttachments: []gpu.GPUAttachment{{PCIAddress: "0000:5e:00.0"}},
		Config:         Config{Memory: 1024 * 1024},
	}
	assert.Equal(t, qmpGreetingCap, qmpGreetingTimeout(hugeGPU),
		"a huge VFIO guest is clamped to the cap")
}

// TestQMPGreetingTimeoutAddsBothCosts is the case a real GPU instance is: it
// boots from a network-backed root volume and it passes through a GPU, so QEMU
// opens the drive over the object store and then pins the whole of guest RAM,
// one after the other, all before the monitor answers.
//
// Taking whichever cost is larger would give it a deadline that cannot cover
// what it has to do. The symptom would be a GPU instance SIGKILLed on every
// attempt while its plain sibling launches, which reads as a GPU fault.
func TestQMPGreetingTimeoutAddsBothCosts(t *testing.T) {
	// EBSRequests carries a mutex, so each VM gets its own rather than a copy.
	withDrive := func(v *VM) *VM {
		v.EBSRequests.Requests = []EBSRequest{
			{Name: "vol-boot", NBDURI: "nbd+unix:///?socket=/run/spinifex/nbd/boot.sock", Boot: true},
		}
		return v
	}
	attachment := []gpu.GPUAttachment{{PCIAddress: "0000:5e:00.0"}}

	gpuOnly := &VM{GPUAttachments: attachment, Config: Config{Memory: 16 * 1024}}
	driveOnly := withDrive(&VM{})
	both := withDrive(&VM{GPUAttachments: attachment, Config: Config{Memory: 16 * 1024}})

	// 16 GiB scales to 158s, so the VFIO term is its floor.
	assert.Equal(t, qmpNBDGreetingTimeout+qmpVFIOGreetingFloor, qmpGreetingTimeout(both),
		"the two costs are sequential, so the deadline that covers both is their sum")
	assert.Greater(t, qmpGreetingTimeout(both), qmpGreetingTimeout(gpuOnly),
		"a GPU guest that also has to open a volume over the network must be given longer than one that does not")
	assert.Greater(t, qmpGreetingTimeout(both), qmpGreetingTimeout(driveOnly),
		"and longer than a guest that only has the volume to open")

	assert.Greater(t, qmpGreetingCap, qmpVFIOGreetingFloor+qmpNBDGreetingTimeout,
		"the cap has to clear both floors together, or it clips a guest before either cost has been waited out")
}

// TestRemoveStaleQMPSocket covers the SIGKILL-leftover unlink: a stale socket
// inode is removed, and a missing file is a no-op (not an error).
func TestRemoveStaleQMPSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qmp-i-stale.sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	require.NoError(t, removeStaleQMPSocket(path))
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "stale socket inode must be unlinked")

	require.NoError(t, removeStaleQMPSocket(path), "a missing socket is a no-op")
}

// TestIsTransientDialError pins which connect failures the QMP dial retries: a
// listener-not-up-yet (refused), a socket briefly absent (enoent), or a peer
// resetting mid-greeting (reset/broken pipe from a QEMU still initialising) are
// transient; a clean decode/handshake failure is not — it means a settled
// listener answered.
func TestIsTransientDialError(t *testing.T) {
	assert.True(t, isTransientDialError(syscall.ECONNREFUSED), "pre-listen/stale refuses connect")
	assert.True(t, isTransientDialError(syscall.ENOENT), "socket absent between unlink and rebind")
	assert.True(t, isTransientDialError(syscall.ECONNRESET), "fresh QEMU resets mid-greeting while initialising")
	assert.True(t, isTransientDialError(syscall.EPIPE), "peer tears down the greeting connection")
	assert.True(t, isTransientDialError(fmt.Errorf("waiting for QMP greeting: %w", syscall.ECONNRESET)),
		"a wrapped connection-reset from the greeting read is still transient")
	assert.False(t, isTransientDialError(fmt.Errorf("waiting for QMP greeting: eof")),
		"a reached-listener handshake failure must not be retried")
}

// serveQMPConn writes the QMP greeting then echoes {"return":{}} for each command
// line, modelling a live QEMU monitor closely enough for a handshake.
func serveQMPConn(c net.Conn) {
	defer func() { _ = c.Close() }()
	greeting := `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}` + "\n"
	if _, err := c.Write([]byte(greeting)); err != nil {
		return
	}
	dec := json.NewDecoder(c)
	for {
		var cmd map[string]any
		if err := dec.Decode(&cmd); err != nil {
			return
		}
		if _, err := c.Write([]byte(`{"return":{}}` + "\n")); err != nil {
			return
		}
	}
}

// startDelayedQMPListener simulates a relaunched QEMU: after delay it unlinks any
// stale inode at sockPath, binds, and serves. Until then a pre-seeded stale inode
// refuses connects, forcing the dialer to retry. Returns a stop function.
func startDelayedQMPListener(t *testing.T, sockPath string, delay time.Duration) func() {
	t.Helper()
	done := make(chan struct{})
	lnCh := make(chan net.Listener, 1)
	go func() {
		defer close(done)
		time.Sleep(delay)
		_ = os.Remove(sockPath)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			lnCh <- nil
			return
		}
		lnCh <- ln
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go serveQMPConn(conn)
		}
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if ln := <-lnCh; ln != nil {
			_ = ln.Close()
		}
		<-done
	}
	t.Cleanup(stop)
	return stop
}

// TestNewQMPClientWithHandshake_RetriesTransientConnect is the mgo6c regression:
// a stale socket inode from a SIGKILLed predecessor refuses the first connect,
// and the relaunched QEMU only listens after a short delay. The dial must retry
// past the refused window rather than fail the whole restart.
func TestNewQMPClientWithHandshake_RetriesTransientConnect(t *testing.T) {
	defer goleak.VerifyNone(t)

	sockPath := filepath.Join(t.TempDir(), "qmp-i-retry.sock")

	// Seed a stale inode with no listener — a single dial gets ECONNREFUSED.
	stale, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	unixLn, ok := stale.(*net.UnixListener)
	require.True(t, ok, "unix listener expected")
	unixLn.SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())

	var dials atomic.Int32
	orig := qmpDial
	t.Cleanup(func() { qmpDial = orig })
	qmpDial = func(path string, greetingTimeout time.Duration) (*qmp.QMPClient, error) {
		dials.Add(1)
		return orig(path, greetingTimeout)
	}

	stop := startDelayedQMPListener(t, sockPath, 60*time.Millisecond)
	defer stop()

	instance := &VM{ID: "i-retry", Config: Config{QMPSocket: sockPath}}
	client, err := newQMPClientWithHandshake(t.Context(), instance)
	require.NoError(t, err, "dial must retry past the refused window")
	require.NotNil(t, client)
	_ = client.Conn.Close()
	assert.Greater(t, dials.Load(), int32(1),
		"the first dial must hit the refused window, so success needs a retry")
}

// startRecordingQMPListener is startWorkingQMPListener with a tally: it counts
// the commands it was asked to execute, which is how a test says how many times
// the manager tried to resume a paused guest.
func startRecordingQMPListener(t *testing.T) (string, func(string) int) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	var mu sync.Mutex
	seen := map[string]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				greeting := `{"QMP":{"version":{"qemu":{"major":8,"minor":0}},"capabilities":[]}}` + "\n"
				if _, err := c.Write([]byte(greeting)); err != nil {
					return
				}
				dec := json.NewDecoder(c)
				for {
					var cmd struct {
						Execute string `json:"execute"`
					}
					if err := dec.Decode(&cmd); err != nil {
						return
					}
					mu.Lock()
					seen[cmd.Execute]++
					mu.Unlock()
					if _, err := c.Write([]byte(`{"return":{}}` + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})

	return sockPath, func(execute string) int {
		mu.Lock()
		defer mu.Unlock()
		return seen[execute]
	}
}

// pausedGuest wires a manager holding one running VM whose QMP monitor answers,
// which is the state a werror=stop pause leaves behind.
func pausedGuest(t *testing.T) (*Manager, *VM, func(string) int) {
	t.Helper()
	sockPath, count := startRecordingQMPListener(t)
	client, err := qmp.NewQMPClient(sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Conn.Close() })

	instance := &VM{ID: "i-paused", Status: StateRunning, QMPClient: client}
	m := NewManager()
	m.Insert(instance)

	return m, instance, count
}

// The first attempts run at heartbeat rate, because a backend that comes back
// quickly should return the guest quickly.
func TestResumeAfterIOErrorRetriesEveryHeartbeatAtFirst(t *testing.T) {
	m, instance, count := pausedGuest(t)

	for range qmpMaxIOErrorResumes {
		m.resumeAfterIOError(t.Context(), instance)
	}

	assert.Equal(t, qmpMaxIOErrorResumes, count("cont"),
		"every heartbeat inside the budget must re-submit the held request")
	assert.Equal(t, qmpMaxIOErrorResumes, instance.Health.IOErrorResumes)
	assert.False(t, instance.Health.IOErrorSince.IsZero(), "the first pause must be stamped")
	assert.False(t, instance.Health.ImpairedSince.IsZero(),
		"spending the budget is what DescribeInstanceStatus reports as impaired")
}

// Past the budget the guest is reported impaired, and the retries keep running
// at heartbeat rate. They used to drop to one per ten heartbeats, which meant a
// guest could sit paused for five minutes after its backend had come back --
// an outage of our own making, to save one replayed I/O per thirty seconds.
func TestResumeAfterIOErrorKeepsRetryingPastTheBudget(t *testing.T) {
	m, instance, count := pausedGuest(t)

	const beyond = qmpMaxIOErrorResumes + 3*qmpIOErrorLogEvery
	for range beyond {
		m.resumeAfterIOError(t.Context(), instance)
	}

	assert.Equal(t, beyond, count("cont"),
		"every heartbeat must re-submit the held request, however long the outage has run")
	assert.Equal(t, beyond, instance.Health.IOErrorResumes,
		"the count keeps rising, so the impaired report stays anchored to it")
}

// A guest that resumes must start from zero, or a second unrelated outage
// inherits a spent budget and is written off on its first heartbeat.
func TestQMPSuccessClearsTheIOErrorBudget(t *testing.T) {
	m, instance, _ := pausedGuest(t)

	for range qmpMaxIOErrorResumes + 1 {
		m.resumeAfterIOError(t.Context(), instance)
	}
	require.NotZero(t, instance.Health.IOErrorResumes)

	m.recordQMPSuccess(instance)

	assert.Zero(t, instance.Health.IOErrorResumes)
	assert.True(t, instance.Health.IOErrorSince.IsZero())
}

// directBootBase is a launcher-built microvm Config with one NIC and spare
// capacity in every slice, so an append that aliased it would be caught.
func directBootBase() Config {
	devices := make([]Device, 1, 8)
	devices[0] = Device{Value: "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:01"}
	netdevs := make([]NetDev, 1, 8)
	netdevs[0] = NetDev{Value: "tap,id=net0,ifname=tap0,script=no,downscript=no"}
	fwcfg := make([]FwCfgEntry, 1, 8)
	fwcfg[0] = FwCfgEntry{Name: "opt/spinifex/netcfg", File: "/run/netcfg"}
	return Config{
		Name:          "i-sys",
		EnableKVM:     true,
		NoGraphic:     true,
		MachineType:   "microvm,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on",
		CPUType:       "host,-pmu",
		CPUCount:      2,
		Memory:        512,
		BIOS:          "/usr/share/spinifex/microvm/qboot.rom",
		KernelImage:   "/usr/share/spinifex/microvm/vmlinuz",
		Initrd:        "/usr/share/spinifex/microvm/initramfs.cpio.gz",
		KernelCmdline: "console=ttyS0 quiet",
		Drives:        make([]Drive, 0, 8),
		IOThreads:     make([]IOThread, 0, 8),
		Blockdevs:     make([]Blockdev, 0, 8),
		Devices:       devices,
		NetDevs:       netdevs,
		FwCfg:         fwcfg,
	}
}

func directBootRequests() []EBSRequest {
	return []EBSRequest{
		{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock"},
		{Name: "vol-blk", BlockDevice: "/dev/sdx", BlockSizeBytes: 10 << 30, HotplugPort: 2},
	}
}

// deepCopyConfig snapshots every slice so a later write through an alias
// shows up as a difference.
func deepCopyConfig(c Config) Config {
	c.Drives = slices.Clone(c.Drives)
	c.IOThreads = slices.Clone(c.IOThreads)
	c.Blockdevs = slices.Clone(c.Blockdevs)
	c.Devices = slices.Clone(c.Devices)
	c.NetDevs = slices.Clone(c.NetDevs)
	c.FwCfg = slices.Clone(c.FwCfg)
	return c
}

func TestDirectBootLaunchConfig_AppendsDataVolumesAfterExistingDevices(t *testing.T) {
	base := directBootBase()
	got, err := directBootLaunchConfig(base, directBootRequests(), 2)
	require.NoError(t, err)

	assert.Equal(t, []IOThread{{ID: "ioth-vol-data-a"}, {ID: "ioth-vol-blk"}}, got.IOThreads)
	assert.Equal(t, []Blockdev{
		{Value: "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/tmp/data-a.sock,export=,reconnect-delay=30"},
		{Value: "driver=raw,node-name=nbd-vol-blk,size=10737418240,file.driver=host_device,file.filename=/dev/sdx,file.aio=native,file.cache.direct=on"},
	}, got.Blockdevs)
	assert.Equal(t, []Device{
		{Value: "virtio-net-device,netdev=net0,mac=02:00:00:aa:bb:01"},
		{Value: "virtio-blk-device,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,werror=stop,rerror=stop"},
		{Value: "virtio-blk-device,id=vdisk-vol-blk,drive=nbd-vol-blk,iothread=ioth-vol-blk,serial=volblk,werror=stop,rerror=stop"},
	}, got.Devices, "the NIC keeps its MMIO slot ahead of the volumes")
	assert.Empty(t, got.Drives, "data volumes are blockdevs, never -drive")

	// Only the three volume fields differ; the kernel args and every
	// unrelated field are the base's.
	want := deepCopyConfig(base)
	want.IOThreads, want.Blockdevs, want.Devices = got.IOThreads, got.Blockdevs, got.Devices
	assert.Equal(t, want, got)
}

func TestDirectBootLaunchConfig_DoesNotMutateInputs(t *testing.T) {
	base := directBootBase()
	baseBefore := deepCopyConfig(base)
	requests := directBootRequests()
	requestsBefore := append([]EBSRequest(nil), requests...)

	got, err := directBootLaunchConfig(base, requests, 2)
	require.NoError(t, err)
	assert.Equal(t, baseBefore, deepCopyConfig(base), "base fields are unchanged")
	assert.Equal(t, requestsBefore, requests, "requests are unchanged")

	// Spare capacity: the composition must not have written into the base's
	// backing arrays, and neither result may see the other's later appends.
	assert.Equal(t, Device{}, base.Devices[:2][1], "nothing was written past the base's length")
	assert.Equal(t, IOThread{}, base.IOThreads[:1][0])
	assert.Equal(t, Blockdev{}, base.Blockdevs[:1][0])

	base.Devices = append(base.Devices, Device{Value: "virtio-rng-device"})
	base.NetDevs = append(base.NetDevs, NetDev{Value: "tap,id=net9"})
	base.FwCfg = append(base.FwCfg, FwCfgEntry{Name: "x"})
	assert.Equal(t, "virtio-blk-device,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,werror=stop,rerror=stop", got.Devices[1].Value)
	assert.Len(t, got.NetDevs, 1)
	assert.Len(t, got.FwCfg, 1)

	got.Devices[0].Value = "changed"
	got.NetDevs = append(got.NetDevs, NetDev{Value: "tap,id=net8"})
	got.FwCfg[0].File = "changed"
	assert.Equal(t, baseBefore.Devices[0], base.Devices[0])
	assert.Equal(t, NetDev{Value: "tap,id=net9"}, base.NetDevs[1])
	assert.Equal(t, baseBefore.FwCfg[0], base.FwCfg[0])
}

func TestDirectBootLaunchConfig_RepeatedCompositionFromTheSameBase(t *testing.T) {
	base := directBootBase()
	first, err := directBootLaunchConfig(base, directBootRequests(), 2)
	require.NoError(t, err)
	second, err := directBootLaunchConfig(base, directBootRequests(), 2)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Len(t, second.Devices, 3, "one NIC and one device per volume, never accumulated")
	assert.Len(t, second.Blockdevs, 2)
	assert.Len(t, second.IOThreads, 2)
}

func TestDirectBootLaunchConfig_ChangedNBDURIOnALaterLaunch(t *testing.T) {
	base := directBootBase()
	requests := []EBSRequest{{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock"}}
	_, err := directBootLaunchConfig(base, requests, 2)
	require.NoError(t, err)

	// A remount publishes a new export; the next launch must use only it.
	requests[0].NBDURI = "nbd://10.0.0.7:10809"
	got, err := directBootLaunchConfig(base, requests, 2)
	require.NoError(t, err)
	require.Len(t, got.Blockdevs, 1)
	assert.Contains(t, got.Blockdevs[0].Value, "server.type=inet,server.host=10.0.0.7,server.port=10809")
	assert.NotContains(t, got.Blockdevs[0].Value, "/tmp/data-a.sock")
}

func TestDirectBootLaunchConfig_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		base     func() Config
		requests []EBSRequest
		wantErr  string
	}{
		{
			name:     "boot volume",
			base:     directBootBase,
			requests: []EBSRequest{{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true}},
			wantErr:  "vol-boot",
		},
		{
			name:     "EFI volume",
			base:     directBootBase,
			requests: []EBSRequest{{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true}},
			wantErr:  "vol-efi",
		},
		{
			name:     "unmounted data volume",
			base:     directBootBase,
			requests: []EBSRequest{{Name: "vol-bad"}},
			wantErr:  "NBDURI not set for volume vol-bad",
		},
		{
			name:     "both NBD and block exports",
			base:     directBootBase,
			requests: []EBSRequest{{Name: "vol-both", NBDURI: "nbd:unix:/tmp/x.sock", BlockDevice: "/dev/sdx", BlockSizeBytes: 1 << 20}},
			wantErr:  "vol-both",
		},
		{
			name: "non-microvm base",
			base: func() Config {
				c := directBootBase()
				c.MachineType = "q35"
				return c
			},
			requests: []EBSRequest{{Name: "vol-data-a", NBDURI: "nbd:unix:/tmp/data-a.sock"}},
			wantErr:  "microvm",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.base()
			before := deepCopyConfig(base)
			_, err := directBootLaunchConfig(base, tt.requests, 2)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Equal(t, before, deepCopyConfig(base))
		})
	}
}

// fakeQEMUScript is a stand-in QEMU that records each launch's argv, writes the
// pidfile and creates the QMP socket path startQEMU waits on.
const fakeQEMUScript = `#!/bin/sh
pidfile=""
qmp=""
prev=""
for a in "$@"; do
	case "$prev" in
	-pidfile) pidfile="$a" ;;
	-qmp)
		if [ -z "$qmp" ]; then
			q="${a#unix:}"
			qmp="${q%%,*}"
		fi
		;;
	esac
	prev="$a"
done
{
	echo "--- launch"
	for a in "$@"; do printf '%s\n' "$a"; done
} >>"$0.log"
echo $$ >"$pidfile"
: >"$qmp"
# Stay up like a running guest until the socket goes: a relaunch unlinks it,
# and test cleanup removes its directory.
while [ -e "$qmp" ]; do sleep 0.05; done
`

// withFakeQEMU points QEMU resolution at fakeQEMUScript and returns a reader
// for the argv of every launch so far.
func withFakeQEMU(t *testing.T) func() [][]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qemu-system-x86_64")
	require.NoError(t, os.WriteFile(path, []byte(fakeQEMUScript), 0o755))
	withQEMUResolver(t, func(string) (string, error) { return path, nil })
	return func() [][]string {
		data, err := os.ReadFile(path + ".log")
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		var launches [][]string
		for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "--- launch" {
				launches = append(launches, nil)
				continue
			}
			launches[len(launches)-1] = append(launches[len(launches)-1], line)
		}
		return launches
	}
}

// nbdListener stands in for a mounted NBD export: startQEMU only needs it to
// accept a connection.
func nbdListener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "nbd://" + l.Addr().String()
}

// argsContaining returns the argv entries that contain sub.
func argsContaining(args []string, sub string) []string {
	var out []string
	for _, a := range args {
		if strings.Contains(a, sub) {
			out = append(out, a)
		}
	}
	return out
}

func directBootInstance(id string, requests ...EBSRequest) *VM {
	instance := &VM{
		ID:           id,
		InstanceType: "t3.nano",
		DirectBoot:   true,
		Config:       directBootBase(),
	}
	instance.Config.Architecture = "x86_64"
	instance.EBSRequests.Requests = requests
	return instance
}

// assertCleanBase checks the instance's saved Config carries none of the
// launch-only volume entries and no NBD endpoint.
func assertCleanBase(t *testing.T, cfg Config, endpoints ...string) {
	t.Helper()
	base := directBootBase()
	assert.Equal(t, base.Devices, cfg.Devices)
	assert.Equal(t, base.Blockdevs, cfg.Blockdevs)
	assert.Equal(t, base.IOThreads, cfg.IOThreads)
	assert.Equal(t, base.Drives, cfg.Drives)
	saved, err := json.Marshal(cfg)
	require.NoError(t, err)
	for _, ep := range endpoints {
		assert.NotContains(t, string(saved), strings.TrimPrefix(ep, "nbd://"))
	}
}

func TestStartQEMU_DirectBoot_LaunchesMountedDataVolumeWithoutSavingIt(t *testing.T) {
	m := directBootManager(t, &fakeNetworkPlumber{})
	launches := withFakeQEMU(t)
	first := nbdListener(t)
	second := nbdListener(t)

	instance := directBootInstance("i-db-vol", EBSRequest{Name: "vol-data-a", NBDURI: first})
	require.NoError(t, m.startQEMU(instance, false))

	// A later launch from the same saved base, after the volume was remounted
	// on a different endpoint.
	instance.EBSRequests.Requests[0].NBDURI = second
	require.NoError(t, m.startQEMU(instance, false))

	got := launches()
	require.Len(t, got, 2)
	for i, ep := range []string{first, second} {
		args := got[i]
		require.Len(t, argsContaining(args, "virtio-blk-device"), 1, "launch %d", i+1)
		assert.Empty(t, argsContaining(args, "virtio-blk-pci"), "launch %d", i+1)
		assert.Len(t, argsContaining(args, "ioth-vol-data-a"), 2, "launch %d: one iothread object and its device", i+1)
		port := ep[strings.LastIndex(ep, ":")+1:]
		assert.NotEmpty(t, argsContaining(args, port), "launch %d uses its current endpoint", i+1)
		// The NIC stays ahead of the data disk.
		nic := slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, "virtio-net-device") })
		disk := slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, "virtio-blk-device") })
		assert.Less(t, nic, disk, "launch %d", i+1)
	}
	firstPort := first[strings.LastIndex(first, ":")+1:]
	assert.Empty(t, argsContaining(got[1], firstPort), "second launch must not carry the stale endpoint")

	assertCleanBase(t, instance.Config, first, second)
}

// TestStartQEMU_DirectBoot_LaunchesTheEndpointItProbed remounts the volume
// while its endpoint is being probed: the command must carry the endpoint that
// was checked, not one read from the requests afterwards.
func TestStartQEMU_DirectBoot_LaunchesTheEndpointItProbed(t *testing.T) {
	m := directBootManager(t, &fakeNetworkPlumber{})
	launches := withFakeQEMU(t)
	probedEP := nbdListener(t)
	remountedEP := nbdListener(t)

	instance := directBootInstance("i-db-snap", EBSRequest{Name: "vol-data-a", NBDURI: probedEP})
	var probed []string
	orig := waitForNBDReady
	t.Cleanup(func() { waitForNBDReady = orig })
	waitForNBDReady = func(uri string, timeout time.Duration) error {
		probed = append(probed, uri)
		instance.EBSRequests.Mu.Lock()
		instance.EBSRequests.Requests[0].NBDURI = remountedEP
		instance.EBSRequests.Mu.Unlock()
		return orig(uri, timeout)
	}
	require.NoError(t, m.startQEMU(instance, false))

	assert.Equal(t, []string{probedEP}, probed)
	got := launches()
	require.Len(t, got, 1)
	port := func(ep string) string { return ep[strings.LastIndex(ep, ":")+1:] }
	assert.NotEmpty(t, argsContaining(got[0], port(probedEP)), "the probed endpoint is launched")
	assert.Empty(t, argsContaining(got[0], port(remountedEP)), "an endpoint nobody probed is not launched")
}

func TestStartQEMU_DirectBoot_NoVolumesLaunchesTheBase(t *testing.T) {
	m := directBootManager(t, &fakeNetworkPlumber{})
	launches := withFakeQEMU(t)

	instance := directBootInstance("i-db-novol")
	require.NoError(t, m.startQEMU(instance, false))

	got := launches()
	require.Len(t, got, 1)
	assert.Empty(t, argsContaining(got[0], "virtio-blk"))
	assert.Len(t, argsContaining(got[0], "virtio-net-device"), 1)
	assertCleanBase(t, instance.Config)
}

func TestStartQEMU_DirectBoot_InvalidDataRequestIsNotLaunched(t *testing.T) {
	tests := []struct {
		name    string
		req     EBSRequest
		wantErr string
	}{
		{name: "no export", req: EBSRequest{Name: "vol-none"}, wantErr: "vol-none"},
		{name: "boot volume", req: EBSRequest{Name: "vol-boot", NBDURI: "nbd:unix:/tmp/boot.sock", Boot: true}, wantErr: "data volumes only"},
		{name: "efi volume", req: EBSRequest{Name: "vol-efi", NBDURI: "nbd:unix:/tmp/efi.sock", EFI: true}, wantErr: "data volumes only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := directBootManager(t, &fakeNetworkPlumber{})
			launches := withFakeQEMU(t)

			instance := directBootInstance("i-db-bad", tt.req)
			err := m.startQEMU(instance, false)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Empty(t, launches(), "QEMU must not start")
			assertCleanBase(t, instance.Config)
		})
	}
}

func TestStartQEMU_DiskBoot_DataVolumeStaysOnThePCIPath(t *testing.T) {
	m := directBootManager(t, &fakeNetworkPlumber{})
	launches := withFakeQEMU(t)
	ep := nbdListener(t)

	instance := &VM{ID: "i-disk-vol", InstanceType: "t3.nano"}
	instance.EBSRequests.Requests = []EBSRequest{
		{Name: "vol-root", NBDURI: "nbd:unix:" + filepath.Join(t.TempDir(), "root.sock"), Boot: true},
		{Name: "vol-data-a", NBDURI: ep},
	}
	require.NoError(t, os.WriteFile(strings.TrimPrefix(instance.EBSRequests.Requests[0].NBDURI, "nbd:unix:"), nil, 0o600))
	require.NoError(t, m.startQEMU(instance, false))

	got := launches()
	require.Len(t, got, 1)
	data := argsContaining(got[0], "id=vdisk-vol-data-a")
	require.Len(t, data, 1)
	assert.True(t, strings.HasPrefix(data[0], "virtio-blk-pci,"), data[0])
	assert.Empty(t, argsContaining(got[0], "virtio-blk-device"))
	// The disk-boot path keeps building the volume into the instance Config.
	assert.Len(t, argsContaining(deviceValues(instance.Config.Devices), "id=vdisk-vol-data-a"), 1)
	assert.Equal(t, 1, instance.EBSRequests.Requests[1].HotplugPort)
}

// TestStartQEMU_DiskBoot_BootVolumeIsABlockdevNode launches a disk-boot guest
// from each export kind and checks the boot volume reaches QEMU as the "os"
// -blockdev node behind the bootindex=1 device, with no -drive left for it.
func TestStartQEMU_DiskBoot_BootVolumeIsABlockdevNode(t *testing.T) {
	tests := []struct {
		name         string
		req          func(t *testing.T) EBSRequest
		wantBlockdev func(req EBSRequest) string
	}{
		{
			name: "nbd export",
			req: func(t *testing.T) EBSRequest {
				return EBSRequest{Name: "vol-root", NBDURI: nbdListener(t), Boot: true}
			},
			wantBlockdev: func(req EBSRequest) string {
				hostPort := strings.TrimPrefix(req.NBDURI, "nbd://")
				host, port, _ := strings.Cut(hostPort, ":")
				return "driver=raw,node-name=os,cache.direct=on,file.driver=nbd,file.server.type=inet,file.server.host=" + host +
					",file.server.port=" + port + ",file.export=,file.reconnect-delay=30"
			},
		},
		{
			name: "block export",
			req: func(t *testing.T) EBSRequest {
				dev := filepath.Join(t.TempDir(), "sdx")
				require.NoError(t, os.WriteFile(dev, nil, 0o600))
				return EBSRequest{Name: "vol-root", BlockDevice: dev, BlockSizeBytes: 8 << 30, Boot: true}
			},
			wantBlockdev: func(req EBSRequest) string {
				return "driver=raw,node-name=os,size=8589934592,file.driver=host_device,file.filename=" + req.BlockDevice +
					",file.aio=native,file.cache.direct=on"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := directBootManager(t, &fakeNetworkPlumber{})
			launches := withFakeQEMU(t)

			req := tt.req(t)
			instance := &VM{ID: "i-boot-blockdev", InstanceType: "t3.nano"}
			instance.EBSRequests.Requests = []EBSRequest{req}
			require.NoError(t, m.startQEMU(instance, false))

			got := launches()
			require.Len(t, got, 1)
			assert.Equal(t, []string{tt.wantBlockdev(req)}, allArgValues(got[0], "-blockdev"))
			assert.Contains(t, allArgValues(got[0], "-device"), "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=1,bootindex=1,werror=stop,rerror=stop")
			for _, d := range allArgValues(got[0], "-drive") {
				assert.NotContains(t, d, "id=os", "the boot volume must not also be a -drive")
			}
		})
	}
}

func deviceValues(devs []Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.Value
	}
	return out
}

func TestDirectBootLaunchConfig_PCIEDataVolumesKeepSerialAndID(t *testing.T) {
	base := directBootBase()
	base.MachineType = "microvm,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on,pcie=on"
	got, err := directBootLaunchConfig(base, directBootRequests(), 2)
	require.NoError(t, err)

	require.Len(t, got.Devices, 3)
	assert.Equal(t, base.Devices[0], got.Devices[0])
	assert.Equal(t, Device{Value: "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,werror=stop,rerror=stop"}, got.Devices[1])
	assert.Equal(t, Device{Value: "virtio-blk-pci,id=vdisk-vol-blk,drive=nbd-vol-blk,iothread=ioth-vol-blk,serial=volblk,werror=stop,rerror=stop"}, got.Devices[2])
	for _, d := range got.Devices[1:] {
		assert.NotContains(t, d.Value, "bus=", "microvm has no hot-plug root port to name")
	}
	assert.Empty(t, got.Drives)
}
