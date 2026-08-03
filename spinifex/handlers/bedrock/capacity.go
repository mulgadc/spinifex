package handlers_bedrock

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/gpu"
)

// gpuSnapshotter reports the current GPU/MIG pool. Narrowed to Snapshot so
// this package depends on neither Claim nor Release — those happen inside the
// shared system-instance launch path, not here.
type gpuSnapshotter interface {
	Snapshot() []gpu.PoolEntry
}

// checkCapacity reports whether the free pool (whole devices or MIG slices
// with Available=true) has at least one entry with minVRAMMiB free. It is a
// fast pre-admission check only: the deeper claim/allocate decision still
// happens inside the shared RunInstances path when the VM actually launches,
// so a device can still be lost to a racing launch between this check and
// that claim — the guest fails to boot in that rare case, not silently OOMs.
func checkCapacity(snapshot []gpu.PoolEntry, minVRAMMiB int) error {
	if minVRAMMiB <= 0 {
		return fmt.Errorf("bedrock: invalid MinVRAMMiB %d", minVRAMMiB)
	}
	for _, entry := range snapshot {
		if !entry.Available {
			continue
		}
		memMiB := entry.Device.MemoryMiB
		if entry.MIGInstance != nil {
			memMiB = entry.MIGInstance.Profile.MemoryMiB
		}
		if memMiB >= int64(minVRAMMiB) {
			return nil
		}
	}
	return awserrors.Errorf(awserrors.ErrorModelNotReadyException,
		"bedrock: no free GPU device has %d MiB of VRAM free", minVRAMMiB)
}

// admitCapacity checks minVRAMMiB against snapshotter's current free pool.
// A nil snapshotter (no GPU manager: passthrough disabled or no GPUs present)
// admits nothing, since every self-host model in the catalog requires a GPU.
func admitCapacity(snapshotter gpuSnapshotter, minVRAMMiB int) error {
	if snapshotter == nil {
		return awserrors.Errorf(awserrors.ErrorModelNotReadyException, "bedrock: no GPU manager on this node")
	}
	return checkCapacity(snapshotter.Snapshot(), minVRAMMiB)
}
