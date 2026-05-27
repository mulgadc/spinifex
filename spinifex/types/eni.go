package types

import "sync"

// ENIRequests tracks the PCIe hot-plug slot allocator for a single VM's
// hot-plugged ENIs. AvailableSlots is the free-list of slot indices (1-based,
// matching the pcie-root-port id=hotplug-eni{N} pre-allocated at VM start).
// AttachedByENIID maps eniID → assigned slot so the detach path can free the
// slot without re-scanning, and so the post-restart reconciler can rebuild
// the in-memory state from KV truth.
//
// Boot-time ENIs (primary + ExtraENIs) use fixed slot addresses configured
// at VM start and do NOT consume the hot-plug slot pool.
type ENIRequests struct {
	Mu              sync.Mutex     `json:"-"`
	AvailableSlots  []int          `json:"available_slots"`
	AttachedByENIID map[string]int `json:"attached_by_eni_id"`
}
