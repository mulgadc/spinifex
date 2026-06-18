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
