package daemon

import (
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// capacityReservation is an in-memory On-Demand Capacity Reservation pinned to
// this node. The carve-out it holds (TotalInstanceCount × per-instance compute)
// is subtracted from schedulable capacity via reservedCR*. Records are immutable
// after creation; an active reservation is simply present in the map, and
// cancelling removes it. Lost on daemon restart (no persistence in Phase 1).
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
}

// reservationCompute returns the headline compute carve-out for a reservation:
// InstanceCount × catalog (vCPU, memory), with no nbdkit overhead in Phase 1.
func (r *capacityReservation) reservationCompute() (vcpu int, memGB float64) {
	return r.TotalInstanceCount * r.VCPUPerInstance, float64(r.TotalInstanceCount) * r.MemGBPerInstance
}

// CreateReservation re-checks that the reservation's headline compute fits in
// this node's remaining schedulable capacity, and on success bumps reservedCR*,
// stores the record, and refreshes subscriptions. The check-and-commit run under
// a single write-lock so a concurrent allocate or CreateReservation cannot
// overcommit the host. Returns InsufficientInstanceCapacity if it no longer fits.
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

// CancelReservation releases the carve-out for an account-owned reservation and
// removes it from the map, then refreshes subscriptions. Returns the removed
// record and true on success, or (nil, false) if no reservation with that id is
// owned by the account.
func (rm *ResourceManager) CancelReservation(id, accountID string) (*capacityReservation, bool) {
	rm.mu.Lock()
	rec, ok := rm.reservations[id]
	if !ok || rec.AccountID != accountID {
		rm.mu.Unlock()
		return nil, false
	}
	freedVCPU, freedMem := rec.reservationCompute()
	rm.reservedCRVCPU -= freedVCPU
	rm.reservedCRMem -= freedMem
	delete(rm.reservations, id)
	rm.mu.Unlock()

	rm.updateInstanceSubscriptions()
	return rec, true
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
