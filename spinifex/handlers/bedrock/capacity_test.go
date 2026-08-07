package handlers_bedrock

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCapacity(t *testing.T) {
	tests := []struct {
		name     string
		snapshot []gpu.PoolEntry
		minVRAM  int
		wantErr  bool
	}{
		{
			name: "whole device has enough free VRAM",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true},
			},
			minVRAM: 5120,
		},
		{
			name: "MIG slice has enough free VRAM",
			snapshot: []gpu.PoolEntry{
				{MIGInstance: &gpu.MIGInstance{Profile: gpu.MIGProfile{MemoryMiB: 10240}}, Available: true},
			},
			minVRAM: 5120,
		},
		{
			name: "device unhealthy (rebind failed, Available cleared)",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: false},
			},
			minVRAM: 5120,
			wantErr: true,
		},
		{
			// The shape Claim actually leaves behind: it records InstanceID and
			// leaves Available alone, so occupancy is invisible to a check that
			// reads Available on its own.
			name: "whole device claimed by a running instance",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true, InstanceID: "i-abc"},
			},
			minVRAM: 5120,
			wantErr: true,
		},
		{
			name: "MIG slice claimed by a running instance",
			snapshot: []gpu.PoolEntry{
				{MIGInstance: &gpu.MIGInstance{Profile: gpu.MIGProfile{MemoryMiB: 10240}}, Available: true, InstanceID: "i-abc"},
			},
			minVRAM: 5120,
			wantErr: true,
		},
		{
			name: "a claimed device does not hide a free sibling",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 16384}, Available: true, InstanceID: "i-abc"},
				{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true},
			},
			minVRAM: 5120,
		},
		{
			name: "available device too small",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 2048}, Available: true},
			},
			minVRAM: 5120,
			wantErr: true,
		},
		{
			name:     "empty pool",
			snapshot: nil,
			minVRAM:  5120,
			wantErr:  true,
		},
		{
			name: "picks the sufficient device among several",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 2048}, Available: true},
				{Device: gpu.GPUDevice{MemoryMiB: 16384}, Available: true},
			},
			minVRAM: 5120,
		},
		{
			name:     "invalid MinVRAMMiB",
			snapshot: []gpu.PoolEntry{{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true}},
			minVRAM:  0,
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCapacity(tc.snapshot, tc.minVRAM)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestCheckCapacity_ClaimedDeviceIsNotFree pins the defect that kept Ochre's
// LRU eviction dormant: Claim records InstanceID and deliberately leaves
// Available set, because Available tracks device health and Release clears it
// only when a vfio rebind fails. Reading Available alone let a GPU held by a
// running serving VM admit, so Ensure never took its capacity refusal and
// evictForCapacity was never reached.
func TestCheckCapacity_ClaimedDeviceIsNotFree(t *testing.T) {
	pool := []gpu.PoolEntry{
		{Device: gpu.GPUDevice{PCIAddress: "0000:5e:00.0", MemoryMiB: 8188}, Available: true, InstanceID: "i-abc"},
	}
	require.Error(t, checkCapacity(pool, 7168), "a GPU held by a running instance must not admit")

	pool[0].InstanceID = ""
	assert.NoError(t, checkCapacity(pool, 7168), "the same device must admit once released")
}

// TestCheckCapacity_UnknownVRAM_IgnoredWhenClaimed proves occupancy is settled
// before the 0 MiB data-gap branch. A claimed device is not a candidate at all,
// so an operator gets plain exhaustion rather than being sent to chase a
// missing VRAM figure on a card that is merely busy.
func TestCheckCapacity_UnknownVRAM_IgnoredWhenClaimed(t *testing.T) {
	err := checkCapacity([]gpu.PoolEntry{
		{Device: gpu.GPUDevice{PCIAddress: "0000:03:00.0", MemoryMiB: 0}, Available: true, InstanceID: "i-abc"},
	}, 5120)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "undiscoverable")
}

func TestCheckCapacity_ErrorCode(t *testing.T) {
	err := checkCapacity(nil, 5120)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
}

// TestCheckCapacity_UnknownVRAM pins that an available device
// reporting 0 MiB (undiscoverable, not exhausted) must produce a distinct,
// actionable error naming the device — not the generic "no free GPU device
// has N MiB free" exhaustion message, which would mislead an operator into
// thinking the card is genuinely too small or already claimed.
func TestCheckCapacity_UnknownVRAM(t *testing.T) {
	snapshot := []gpu.PoolEntry{
		{
			Device:    gpu.GPUDevice{PCIAddress: "0000:03:00.0", Model: "NVIDIA RTX A1000", VendorID: "10de", DeviceID: "25b0", MemoryMiB: 0},
			Available: true,
		},
	}
	err := checkCapacity(snapshot, 5120)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0000:03:00.0")
	assert.Contains(t, err.Error(), "10de:25b0")
	assert.Contains(t, err.Error(), "gpu model catalog")
	assert.Contains(t, err.Error(), "GPUModelOverride")
	assert.NotContains(t, err.Error(), "no free GPU device has")
}

// TestCheckCapacity_UnknownVRAM_DistinctFromExhaustion proves the two
// failure modes produce different messages for the same minVRAMMiB, so an
// operator (or a caller matching on message text) can tell "data gap" apart
// from "genuinely too small / already claimed".
func TestCheckCapacity_UnknownVRAM_DistinctFromExhaustion(t *testing.T) {
	unknownErr := checkCapacity([]gpu.PoolEntry{
		{Device: gpu.GPUDevice{MemoryMiB: 0}, Available: true},
	}, 5120)
	exhaustedErr := checkCapacity([]gpu.PoolEntry{
		{Device: gpu.GPUDevice{MemoryMiB: 2048}, Available: true},
	}, 5120)
	require.Error(t, unknownErr)
	require.Error(t, exhaustedErr)
	assert.NotEqual(t, unknownErr.Error(), exhaustedErr.Error())
}

// TestCheckCapacity_UnknownVRAM_SkippedWhenAnotherDeviceSuffices ensures an
// unknown-VRAM device never blocks admission when a different available
// device in the same snapshot already satisfies minVRAMMiB.
func TestCheckCapacity_UnknownVRAM_SkippedWhenAnotherDeviceSuffices(t *testing.T) {
	snapshot := []gpu.PoolEntry{
		{Device: gpu.GPUDevice{MemoryMiB: 0}, Available: true},
		{Device: gpu.GPUDevice{MemoryMiB: 16384}, Available: true},
	}
	assert.NoError(t, checkCapacity(snapshot, 5120))
}

type stubSnapshotter struct {
	entries []gpu.PoolEntry
}

func (s *stubSnapshotter) Snapshot() []gpu.PoolEntry { return s.entries }

func TestAdmitCapacity_NilSnapshotter(t *testing.T) {
	err := admitCapacity(nil, 5120)
	assert.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, awserrors.ValidErrorCodeFromError(err))
}

func TestAdmitCapacity_DelegatesToCheckCapacity(t *testing.T) {
	snap := &stubSnapshotter{entries: []gpu.PoolEntry{{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: true}}}
	assert.NoError(t, admitCapacity(snap, 5120))
	assert.Error(t, admitCapacity(snap, 16384))
}
