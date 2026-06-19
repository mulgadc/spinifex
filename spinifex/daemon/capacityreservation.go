package daemon

import (
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// capacityReservation is an in-memory On-Demand Capacity Reservation pinned to
// this node, holding a TotalInstanceCount × per-instance compute carve-out via
// reservedCR*. Present in the map means active; cancel removes it. Lost on restart.
type capacityReservation struct {
	ID                    string
	AccountID             string
	InstanceType          string
	AvailabilityZone      string
	TotalInstanceCount    int
	VCPUPerInstance       int
	MemGBPerInstance      float64
	InstanceMatchCriteria string
	Tenancy               string
	InstancePlatform      string
	CreateDate            time.Time
	ConsumedCount         int // instances launched into this reservation; slots moved reservedCR*->allocated*
}

// reservationCompute returns the full compute carve-out for a reservation:
// TotalInstanceCount × per-instance (vCPU, memory). MemGBPerInstance already
// folds in nbdkit overhead (set at create), so the reservedCR->allocated swap on
// launch is net-zero.
func (r *capacityReservation) reservationCompute() (vcpu int, memGB float64) {
	return r.TotalInstanceCount * r.VCPUPerInstance, float64(r.TotalInstanceCount) * r.MemGBPerInstance
}

// CreateReservation re-checks fit under a single write-lock, then bumps reservedCR*,
// stores the record, and refreshes subscriptions, so a concurrent allocate cannot
// overcommit. Returns InsufficientInstanceCapacity if the carve-out no longer fits.
func (rm *ResourceManager) CreateReservation(rec *capacityReservation) error {
	wantVCPU, wantMem := rec.reservationCompute()

	rm.mu.Lock()
	remVCPU := rm.hostVCPU - rm.reservedVCPU - rm.reservedCRVCPU - rm.allocatedVCPU
	remMem := rm.hostMemGB - rm.reservedMem - rm.reservedCRMem - rm.allocatedMem
	if wantVCPU > remVCPU || wantMem > remMem {
		rm.mu.Unlock()
		return errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}
	rm.reservedCRVCPU += wantVCPU
	rm.reservedCRMem += wantMem
	rm.reservations[rec.ID] = rec
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
	return nil
}

// CancelReservation releases the unconsumed remainder of an account-owned
// reservation's carve-out and removes it; consumed slots already moved to
// allocated* and keep running. Returns the removed record and true on success,
// or (nil, false) if no reservation with that id is owned by the account.
func (rm *ResourceManager) CancelReservation(id, accountID string) (*capacityReservation, bool) {
	rm.mu.Lock()
	rec, ok := rm.reservations[id]
	if !ok || rec.AccountID != accountID {
		rm.mu.Unlock()
		return nil, false
	}
	remaining := rec.TotalInstanceCount - rec.ConsumedCount
	rm.reservedCRVCPU -= remaining * rec.VCPUPerInstance
	rm.reservedCRMem -= float64(remaining) * rec.MemGBPerInstance
	delete(rm.reservations, id)
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
	return rec, true
}

// AllocateFromReservation launches one instance into reservation crID via a
// net-zero swap under rm.mu: the per-instance compute already held in reservedCR*
// at create moves to allocated* and ConsumedCount bumps, so schedulable capacity
// is unchanged by construction. Errors: unknown/foreign id ->
// InvalidCapacityReservationId.NotFound; type mismatch -> InvalidParameterValue;
// already full -> ReservationCapacityExceeded.
func (rm *ResourceManager) AllocateFromReservation(crID, accountID string, it *ec2.InstanceTypeInfo) error {
	rm.mu.Lock()
	rec, ok := rm.reservations[crID]
	if !ok || rec.AccountID != accountID {
		rm.mu.Unlock()
		return errors.New(awserrors.ErrorInvalidCapacityReservationIdNotFound)
	}
	if rec.InstanceType != aws.StringValue(it.InstanceType) {
		rm.mu.Unlock()
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if rec.ConsumedCount >= rec.TotalInstanceCount {
		rm.mu.Unlock()
		return errors.New(awserrors.ErrorReservationCapacityExceeded)
	}
	rm.reservedCRVCPU -= rec.VCPUPerInstance
	rm.reservedCRMem -= rec.MemGBPerInstance
	rm.allocatedVCPU += rec.VCPUPerInstance
	rm.allocatedMem += rec.MemGBPerInstance
	rec.ConsumedCount++
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
	return nil
}

// ReleaseToReservation returns a launched instance's slot. If the reservation
// still exists it is the inverse net-zero swap (allocated* -> reservedCR*,
// ConsumedCount--); if the reservation is gone (cancelled or lost on restart) the
// allocation is freed to the general pool using the catalog charge so it never
// overcounts. Best-effort: a missing reservation is not an error.
func (rm *ResourceManager) ReleaseToReservation(crID string, it *ec2.InstanceTypeInfo) {
	rm.mu.Lock()
	if rec, ok := rm.reservations[crID]; ok && rec.ConsumedCount > 0 {
		rm.allocatedVCPU -= rec.VCPUPerInstance
		rm.allocatedMem -= rec.MemGBPerInstance
		rm.reservedCRVCPU += rec.VCPUPerInstance
		rm.reservedCRMem += rec.MemGBPerInstance
		rec.ConsumedCount--
	} else {
		rm.allocatedVCPU -= int(instanceTypeVCPUs(it))
		rm.allocatedMem -= float64(rm.instanceMemChargeMiB(it)) / 1024.0
	}
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
}

// ReservationAvailable reports how many more instances can launch into crID
// (Total - Consumed), or 0 when the reservation is unknown, owned by another
// account, or for a different instance type. The daemon handler's up-front check
// turns those zero cases into precise errors before the launch loop.
func (rm *ResourceManager) ReservationAvailable(crID, accountID string, it *ec2.InstanceTypeInfo) int {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	rec, ok := rm.reservations[crID]
	if !ok || rec.AccountID != accountID || rec.InstanceType != aws.StringValue(it.InstanceType) {
		return 0
	}
	return rec.TotalInstanceCount - rec.ConsumedCount
}

// ValidateReservationTarget is the owning daemon's up-front semantic check for a
// targeted launch, run before the launch loop. It returns
// InvalidCapacityReservationId.NotFound for an unknown or foreign id and
// InvalidParameterValue on an instance-type mismatch. The "full" case is left to
// the count gate in PrepareRunInstances, which caps the launch at the available
// slots and returns ReservationCapacityExceeded when none remain.
func (rm *ResourceManager) ValidateReservationTarget(crID, accountID string, it *ec2.InstanceTypeInfo) error {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	rec, ok := rm.reservations[crID]
	if !ok || rec.AccountID != accountID {
		return errors.New(awserrors.ErrorInvalidCapacityReservationIdNotFound)
	}
	if rec.InstanceType != aws.StringValue(it.InstanceType) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// ListReservations returns a snapshot of the account's reservations on this node.
// Records are immutable, so the returned pointers are safe to read concurrently.
func (rm *ResourceManager) ListReservations(accountID string) []*capacityReservation {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var out []*capacityReservation
	for _, rec := range rm.reservations {
		if rec.AccountID == accountID {
			out = append(out, rec)
		}
	}
	return out
}
