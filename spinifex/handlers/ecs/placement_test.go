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

func TestPlaceTask_Binpack_PicksTightest(t *testing.T) {
	// remaining mem: a=900, b=300, c=600 → binpack picks b (least remaining).
	instances := []InstanceRecord{
		inst("a", 1000, 100),
		inst("b", 1000, 700),
		inst("c", 1000, 400),
	}
	got, err := placeTask(instances, 100, 200, "binpack:memory")
	require.NoError(t, err)
	assert.Equal(t, "b", got.InstanceID)
}

func TestPlaceTask_Spread_PicksWidest(t *testing.T) {
	instances := []InstanceRecord{
		inst("a", 1000, 100), // 900
		inst("b", 1000, 700), // 300
		inst("c", 1000, 400), // 600
	}
	got, err := placeTask(instances, 100, 200, StrategySpread)
	require.NoError(t, err)
	assert.Equal(t, "a", got.InstanceID)
}

func TestPlaceTask_Random_StableByID(t *testing.T) {
	instances := []InstanceRecord{inst("z", 1000, 0), inst("a", 1000, 0), inst("m", 1000, 0)}
	got, err := placeTask(instances, 100, 100, StrategyRandom)
	require.NoError(t, err)
	assert.Equal(t, "a", got.InstanceID)
}

func TestPlaceTask_NoCapacity(t *testing.T) {
	instances := []InstanceRecord{inst("a", 100, 90)}
	_, err := placeTask(instances, 0, 50, StrategyBinpack)
	assert.ErrorIs(t, err, ErrNoCapacity)
}

func TestPlaceTask_SkipsDraining(t *testing.T) {
	d := inst("drain", 1000, 0)
	d.Status = InstanceStatusDraining
	_, err := placeTask([]InstanceRecord{d}, 0, 100, StrategyBinpack)
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
	assert.True(t, r.fits(100, 400))
	assert.False(t, r.fits(100, 401))
	r.Status = InstanceStatusDraining
	assert.False(t, r.fits(0, 0))
}
