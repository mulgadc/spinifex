package vm

import "sync"

// ENIRequests tracks PCIe hot-plug slot allocation for a VM's hot-plugged ENIs.
// AvailableSlots is the free-list; AttachedByENIID maps eniID → slot for detach
// and post-restart reconciliation. Boot-time ENIs use fixed slots outside this pool.
type ENIRequests struct {
	Mu              sync.Mutex     `json:"-"`
	AvailableSlots  []int          `json:"available_slots"`
	AttachedByENIID map[string]int `json:"attached_by_eni_id"`
}

// SlotHeld reports whether a hot-plugged ENI holds slot, which is also the
// device index the ENI's record reports.
func (r *ENIRequests) SlotHeld(slot int64) bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	for _, held := range r.AttachedByENIID {
		if int64(held) == slot {
			return true
		}
	}
	return false
}
