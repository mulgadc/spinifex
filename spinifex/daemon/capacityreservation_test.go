package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// microType is a deterministic 2 vCPU / 1 GB instance type for carve-out math.
// With nbdkit charges left at 0 the memory charge equals the catalog figure.
func microType() *ec2.InstanceTypeInfo {
	return &ec2.InstanceTypeInfo{
		InstanceType: aws.String("t3.micro"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(1024)},
	}
}

// newReservationTestRM builds an 8 vCPU / 16 GB node with no host reserve, so a
// fresh node fits exactly 4 t3.micro (CPU-bound: 8/2).
func newReservationTestRM() *ResourceManager {
	return &ResourceManager{
		hostVCPU:  8,
		hostMemGB: 16.0,
		instanceTypes: map[string]*ec2.InstanceTypeInfo{
			"t3.micro": microType(),
		},
		reservations: make(map[string]*capacityReservation),
	}
}

func microReservation(id, accountID string, count int) *capacityReservation {
	return &capacityReservation{
		ID:                    id,
		AccountID:             accountID,
		InstanceType:          "t3.micro",
		AvailabilityZone:      "ap-southeast-2a",
		TotalInstanceCount:    count,
		VCPUPerInstance:       2,
		MemGBPerInstance:      1.0,
		InstanceMatchCriteria: "open",
		Tenancy:               "default",
		InstancePlatform:      "Linux/UNIX",
		CreateDate:            time.Now(),
	}
}

// A reservation lowers both canAllocate and the GetResourceStats Available count
// by its headline compute, then cancelling restores them.
func TestCapacityReservation_CarveOutAndRestore(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()

	require.Equal(t, 4, rm.canAllocate(mt, 100), "fresh node fits 4 t3.micro")

	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 2)))

	assert.Equal(t, 4, rm.reservedCRVCPU)
	assert.Equal(t, 2.0, rm.reservedCRMem)
	assert.Equal(t, 2, rm.canAllocate(mt, 100), "carve-out of 2 leaves room for 2 more")

	// GetResourceStats folds the carve-out into the reported reserve and Available.
	_, _, reservedVCPU, reservedMem, _, _, caps := rm.GetResourceStats()
	assert.Equal(t, 4, reservedVCPU)
	assert.Equal(t, 2.0, reservedMem)
	var available int
	for _, c := range caps {
		if c.Name == "t3.micro" {
			available = c.Available
		}
	}
	assert.Equal(t, 2, available, "GetResourceStats Available reflects the carve-out")

	rec, ok := rm.CancelReservation("cr-1", "acct-a")
	require.True(t, ok)
	assert.Equal(t, "cr-1", rec.ID)
	assert.Equal(t, 0, rm.reservedCRVCPU)
	assert.Equal(t, 0.0, rm.reservedCRMem)
	assert.Equal(t, 4, rm.canAllocate(mt, 100), "cancel restores full capacity")
}

// A reservation larger than remaining schedulable capacity is rejected and
// leaves the carve-out untouched.
func TestCapacityReservation_OverReserveRejected(t *testing.T) {
	rm := newReservationTestRM()

	err := rm.CreateReservation(microReservation("cr-big", "acct-a", 5)) // 10 vCPU > 8
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
	assert.Equal(t, 0, rm.reservedCRVCPU)
	assert.Equal(t, 0.0, rm.reservedCRMem)
	assert.Empty(t, rm.reservations)
}

// Cancel is account-scoped: a foreign account cannot release another's hold, and
// an unknown id is reported missing.
func TestCapacityReservation_CancelScoping(t *testing.T) {
	rm := newReservationTestRM()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 1)))

	_, ok := rm.CancelReservation("cr-1", "acct-b")
	assert.False(t, ok, "another account cannot cancel the reservation")
	assert.Equal(t, 2, rm.reservedCRVCPU, "carve-out unchanged after foreign cancel")

	_, ok = rm.CancelReservation("cr-unknown", "acct-a")
	assert.False(t, ok, "unknown id reports not found")

	_, ok = rm.CancelReservation("cr-1", "acct-a")
	assert.True(t, ok)
}

// ListReservations returns only the account's reservations.
func TestCapacityReservation_ListScoping(t *testing.T) {
	rm := newReservationTestRM()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 1)))
	require.NoError(t, rm.CreateReservation(microReservation("cr-2", "acct-b", 1)))

	a := rm.ListReservations("acct-a")
	require.Len(t, a, 1)
	assert.Equal(t, "cr-1", a[0].ID)

	assert.Empty(t, rm.ListReservations("acct-c"))
}

// Concurrent CreateReservation and allocate share the same rm.mu re-check gate,
// so the combined committed compute can never exceed the host's schedulable pool.
func TestCapacityReservation_ConcurrentNoOvercommit(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = rm.CreateReservation(microReservation(fmt.Sprintf("cr-%d", i), "acct-a", 1))
			} else {
				_ = rm.allocate(mt)
			}
		}(i)
	}
	wg.Wait()

	rm.mu.RLock()
	defer rm.mu.RUnlock()

	committedVCPU := rm.reservedCRVCPU + rm.allocatedVCPU
	committedMem := rm.reservedCRMem + rm.allocatedMem
	assert.LessOrEqual(t, committedVCPU, rm.hostVCPU, "vCPU never overcommitted")
	assert.LessOrEqual(t, committedMem, rm.hostMemGB, "memory never overcommitted")

	// reservedCR* stays consistent with the surviving reservation records.
	var sumVCPU int
	var sumMem float64
	for _, rec := range rm.reservations {
		v, m := rec.reservationCompute()
		sumVCPU += v
		sumMem += m
	}
	assert.Equal(t, sumVCPU, rm.reservedCRVCPU)
	assert.Equal(t, sumMem, rm.reservedCRMem)
}

// AllocateFromReservation moves a slot reservedCR*->allocated* without touching
// schedulable capacity, bumps ConsumedCount, and drops AvailableInstanceCount.
func TestAllocateFromReservation_NetZeroSwap(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	rec := microReservation("cr-1", "acct-a", 2)
	require.NoError(t, rm.CreateReservation(rec))

	before := rm.canAllocate(mt, 100)
	require.Equal(t, 2, before, "carve-out of 2 leaves room for 2 general launches")

	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt))

	rm.mu.RLock()
	assert.Equal(t, 2, rm.reservedCRVCPU, "one slot left reservedCR*")
	assert.Equal(t, 1.0, rm.reservedCRMem)
	assert.Equal(t, 2, rm.allocatedVCPU, "consumed slot now in allocated*")
	assert.Equal(t, 1.0, rm.allocatedMem)
	assert.Equal(t, 1, rec.ConsumedCount)
	rm.mu.RUnlock()

	assert.Equal(t, before, rm.canAllocate(mt, 100), "schedulable capacity unchanged by the swap")
	assert.Equal(t, int64(1), aws.Int64Value(rec.toAWSCapacityReservation().AvailableInstanceCount))
}

// The semantic checks reject unknown/foreign ids, type mismatch, and over-consume
// with their mapped AWS error codes, leaving accounting untouched.
func TestAllocateFromReservation_Errors(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 1)))

	err := rm.AllocateFromReservation("cr-missing", "acct-a", mt)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, err.Error())

	err = rm.AllocateFromReservation("cr-1", "acct-b", mt)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, err.Error(), "foreign account is not found")

	wrongType := &ec2.InstanceTypeInfo{
		InstanceType: aws.String("t3.large"),
		VCpuInfo:     &ec2.VCpuInfo{DefaultVCpus: aws.Int64(2)},
		MemoryInfo:   &ec2.MemoryInfo{SizeInMiB: aws.Int64(8192)},
	}
	err = rm.AllocateFromReservation("cr-1", "acct-a", wrongType)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt), "first slot consumes")
	err = rm.AllocateFromReservation("cr-1", "acct-a", mt)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorReservationCapacityExceeded, err.Error(), "second exceeds Total=1")
}

// ReleaseToReservation is the inverse swap when the reservation still exists:
// allocated*->reservedCR*, ConsumedCount--, AvailableInstanceCount restored.
func TestReleaseToReservation_RestoresSlot(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	rec := microReservation("cr-1", "acct-a", 2)
	require.NoError(t, rm.CreateReservation(rec))
	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt))

	rm.ReleaseToReservation("cr-1", mt)

	rm.mu.RLock()
	assert.Equal(t, 4, rm.reservedCRVCPU, "slot returned to reservedCR*")
	assert.Equal(t, 2.0, rm.reservedCRMem)
	assert.Equal(t, 0, rm.allocatedVCPU)
	assert.Equal(t, 0.0, rm.allocatedMem)
	assert.Equal(t, 0, rec.ConsumedCount)
	rm.mu.RUnlock()

	assert.Equal(t, int64(2), aws.Int64Value(rec.toAWSCapacityReservation().AvailableInstanceCount))
}

// After the reservation is cancelled, a still-running consumer's release frees to
// the general pool (catalog charge) instead of a now-gone reservedCR*, never
// overcounting. Cancel itself frees only the unconsumed remainder.
func TestReleaseToReservation_AfterCancelFreesToGeneralPool(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 2)))
	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt))

	_, ok := rm.CancelReservation("cr-1", "acct-a")
	require.True(t, ok)

	rm.mu.RLock()
	assert.Equal(t, 0, rm.reservedCRVCPU, "cancel freed only the unconsumed slot")
	assert.Equal(t, 0.0, rm.reservedCRMem)
	assert.Equal(t, 2, rm.allocatedVCPU, "consumed slot keeps running on general capacity")
	rm.mu.RUnlock()

	rm.ReleaseToReservation("cr-1", mt)

	rm.mu.RLock()
	assert.Equal(t, 0, rm.allocatedVCPU, "release frees the dangling consumer to general pool")
	assert.Equal(t, 0.0, rm.allocatedMem)
	assert.Equal(t, 0, rm.reservedCRVCPU, "no phantom reservedCR* re-added")
	rm.mu.RUnlock()

	assert.Equal(t, 4, rm.canAllocate(mt, 100), "general capacity fully restored")
}

// ReservationAvailable reports Total-Consumed, and 0 for unknown/foreign-account
// /type-mismatch so the count site degrades to the daemon's up-front error.
func TestReservationAvailable(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 2)))

	assert.Equal(t, 2, rm.ReservationAvailable("cr-1", "acct-a", mt))
	assert.Equal(t, 0, rm.ReservationAvailable("cr-missing", "acct-a", mt))
	assert.Equal(t, 0, rm.ReservationAvailable("cr-1", "acct-b", mt), "foreign account sees nothing")

	wrongType := &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.large")}
	assert.Equal(t, 0, rm.ReservationAvailable("cr-1", "acct-a", wrongType), "type mismatch sees nothing")

	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt))
	assert.Equal(t, 1, rm.ReservationAvailable("cr-1", "acct-a", mt), "consume drops availability")
}

// ValidateReservationTarget is the up-front semantic check: unknown/foreign ids
// map to NotFound, a type mismatch to InvalidParameterValue, and a matching
// reservation passes (the full case is left to the launch-side count gate).
func TestValidateReservationTarget(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	require.NoError(t, rm.CreateReservation(microReservation("cr-1", "acct-a", 1)))

	require.NoError(t, rm.ValidateReservationTarget("cr-1", "acct-a", mt))

	err := rm.ValidateReservationTarget("cr-missing", "acct-a", mt)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, err.Error())

	err = rm.ValidateReservationTarget("cr-1", "acct-b", mt)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, err.Error(), "foreign account is not found")

	wrongType := &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.large")}
	err = rm.ValidateReservationTarget("cr-1", "acct-a", wrongType)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	// A full reservation still validates — the count gate handles capacity.
	require.NoError(t, rm.AllocateFromReservation("cr-1", "acct-a", mt))
	require.NoError(t, rm.ValidateReservationTarget("cr-1", "acct-a", mt), "full reservation passes the semantic check")
}

// Concurrent general allocate and AllocateFromReservation share rm.mu; the swap is
// net-zero on reservedCR*+allocated*, so committed compute never exceeds the host
// and ConsumedCount never passes Total.
func TestAllocateFromReservation_ConcurrentNoOvercommit(t *testing.T) {
	rm := newReservationTestRM()
	mt := microType()
	rec := microReservation("cr-1", "acct-a", 2)
	require.NoError(t, rm.CreateReservation(rec))

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = rm.AllocateFromReservation("cr-1", "acct-a", mt)
			} else {
				_ = rm.allocate(mt)
			}
		}(i)
	}
	wg.Wait()

	rm.mu.RLock()
	defer rm.mu.RUnlock()
	assert.LessOrEqual(t, rm.reservedCRVCPU+rm.allocatedVCPU, rm.hostVCPU, "vCPU never overcommitted")
	assert.LessOrEqual(t, rm.reservedCRMem+rm.allocatedMem, rm.hostMemGB, "memory never overcommitted")
	assert.LessOrEqual(t, rec.ConsumedCount, rec.TotalInstanceCount, "ConsumedCount never passes Total")
	assert.GreaterOrEqual(t, rec.ConsumedCount, 0)
}
