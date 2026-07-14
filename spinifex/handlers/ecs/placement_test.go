package handlers_ecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inst(id string, total, reserved int) InstanceRecord {
	return InstanceRecord{
		InstanceID:        id,
		Status:            InstanceStatusActive,
		TotalCPU:          10000,
		ReservedCPU:       0,
		TotalMemoryMiB:    total,
		ReservedMemoryMiB: reserved,
	}
}

// instGPU builds an instance with ample CPU/memory but a fixed GPU capacity, for
// tests exercising the GPU placement dimension in isolation.
func instGPU(id string, totalGPU, reservedGPU int) InstanceRecord {
	r := inst(id, 100000, 0)
	r.TotalGPU = totalGPU
	r.ReservedGPU = reservedGPU
	return r
}

func TestPlaceTask_Binpack_PicksTightest(t *testing.T) {
	// remaining mem: a=900, b=300, c=600 → binpack picks b (least remaining).
	instances := []InstanceRecord{
		inst("a", 1000, 100),
		inst("b", 1000, 700),
		inst("c", 1000, 400),
	}
	got, err := placeTask(instances, 100, 200, 0, "binpack:memory")
	require.NoError(t, err)
	assert.Equal(t, "b", got.InstanceID)
}

func TestPlaceTask_Spread_PicksWidest(t *testing.T) {
	instances := []InstanceRecord{
		inst("a", 1000, 100), // 900
		inst("b", 1000, 700), // 300
		inst("c", 1000, 400), // 600
	}
	got, err := placeTask(instances, 100, 200, 0, StrategySpread)
	require.NoError(t, err)
	assert.Equal(t, "a", got.InstanceID)
}

func TestPlaceTask_Random_StableByID(t *testing.T) {
	instances := []InstanceRecord{inst("z", 1000, 0), inst("a", 1000, 0), inst("m", 1000, 0)}
	got, err := placeTask(instances, 100, 100, 0, StrategyRandom)
	require.NoError(t, err)
	assert.Equal(t, "a", got.InstanceID)
}

func TestPlaceTask_NoCapacity(t *testing.T) {
	instances := []InstanceRecord{inst("a", 100, 90)}
	_, err := placeTask(instances, 0, 50, 0, StrategyBinpack)
	assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestPlaceTask_SkipsDraining(t *testing.T) {
	d := inst("drain", 1000, 0)
	d.Status = InstanceStatusDraining
	_, err := placeTask([]InstanceRecord{d}, 0, 100, 0, StrategyBinpack)
	assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestNormalizeStrategy(t *testing.T) {
	assert.Equal(t, StrategyBinpack, normalizeStrategy(""))
	assert.Equal(t, StrategyBinpack, normalizeStrategy("binpack:memory"))
	assert.Equal(t, StrategyBinpack, normalizeStrategy("BinPack:cpu"))
	assert.Equal(t, StrategySpread, normalizeStrategy("spread"))
	assert.Equal(t, StrategyRandom, normalizeStrategy("RANDOM"))
	assert.Equal(t, StrategyBinpack, normalizeStrategy("bogus"))
}

func TestInstanceRecord_Fits(t *testing.T) {
	r := inst("a", 1000, 600) // remaining mem 400, cpu 10000
	assert.True(t, r.fits(100, 400, 0))
	assert.False(t, r.fits(100, 401, 0))
	r.Status = InstanceStatusDraining
	assert.False(t, r.fits(0, 0, 0))
}

// --- GPU placement dimension (Epic C2) ---

func TestInstanceRecord_Fits_GPU(t *testing.T) {
	r := instGPU("a", 2, 1) // 1 GPU free
	assert.True(t, r.fits(0, 0, 1))
	assert.False(t, r.fits(0, 0, 2))
}

func TestInstanceRecord_Fits_GPU_IgnoredForNonGPUTasks(t *testing.T) {
	// An instance with zero free GPU still fits a task that requests none.
	r := instGPU("a", 1, 1)
	assert.True(t, r.fits(0, 0, 0))
}

func TestPlaceTask_GPU_RejectsInsufficientInstances(t *testing.T) {
	instances := []InstanceRecord{instGPU("a", 1, 1), instGPU("b", 2, 2)}
	_, err := placeTask(instances, 0, 0, 1, StrategyBinpack)
	assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestPlaceTask_GPU_PicksInstanceWithCapacity(t *testing.T) {
	instances := []InstanceRecord{instGPU("a", 1, 1), instGPU("b", 2, 0)}
	got, err := placeTask(instances, 0, 0, 2, StrategyBinpack)
	require.NoError(t, err)
	assert.Equal(t, "b", got.InstanceID)
}

func TestPlaceTask_GPU_NonGPUTaskUnaffectedByExhaustedGPU(t *testing.T) {
	// A task requesting no GPU still places on an instance with zero free GPU.
	instances := []InstanceRecord{instGPU("a", 1, 1)}
	got, err := placeTask(instances, 0, 0, 0, StrategyBinpack)
	require.NoError(t, err)
	assert.Equal(t, "a", got.InstanceID)
}

func TestInstanceRecord_RemainingGPUIDs(t *testing.T) {
	r := InstanceRecord{TotalGPU: 3, ReservedGPU: 1, GPUIDs: []string{"gpu-0", "gpu-1", "gpu-2"}}
	assert.Equal(t, []string{"gpu-1", "gpu-2"}, r.remainingGPUIDs())

	r.ReservedGPU = 3
	assert.Nil(t, r.remainingGPUIDs())

	// Empty GPUIDs (UUIDs not yet reported by the agent) is a valid placeholder.
	r2 := InstanceRecord{TotalGPU: 2, ReservedGPU: 1}
	assert.Nil(t, r2.remainingGPUIDs())
}
