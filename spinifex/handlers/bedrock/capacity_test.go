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
			name: "device exists but not available (already claimed)",
			snapshot: []gpu.PoolEntry{
				{Device: gpu.GPUDevice{MemoryMiB: 8192}, Available: false},
			},
			minVRAM: 5120,
			wantErr: true,
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
