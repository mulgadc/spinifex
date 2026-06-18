package daemon

import (
	"errors"
	"time"

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
