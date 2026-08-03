package handlers_bedrock

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/stretchr/testify/assert"
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
