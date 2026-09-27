package vm

import "time"

// SetUnreachableAddresses replaces this node's record of which guests the cloud
// underneath is not delivering a public address to, keyed by ENI.
//
// It takes the whole set rather than one guest at a time, because that is what
// clears a guest whose address has since arrived: a marker that only ever gets
// set would outlive the fault and report a healthy instance as impaired forever.
// Callers that learned nothing this pass must not call it at all — an empty map
// means "every guest is reachable", not "no information".
//
// Returns the IDs of the instances now marked, so a caller can say which guests
// are running and answering on nothing.
func (m *Manager) SetUnreachableAddresses(byENI map[string]string) []string {
	var marked []string
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.vms {
		reason, unreachable := unreachableReason(v, byENI)
		if !unreachable {
			v.Health.AddressUnreachableSince = time.Time{}
			v.Health.AddressUnreachableReason = ""
			continue
		}
		// The timestamp is when the fault started, not when it was last
		// observed, so a pass that re-confirms it must not push it forward.
		if v.Health.AddressUnreachableSince.IsZero() {
			v.Health.AddressUnreachableSince = now
		}
		v.Health.AddressUnreachableReason = reason
		marked = append(marked, v.ID)
	}
	return marked
}

// unreachableReason reports whether any of v's ENIs is in byENI, and why.
func unreachableReason(v *VM, byENI map[string]string) (string, bool) {
	if len(byENI) == 0 {
		return "", false
	}
	if reason, ok := byENI[v.ENIId]; ok {
		return reason, true
	}
	for _, eni := range v.ExtraENIs {
		if reason, ok := byENI[eni.ENIID]; ok {
			return reason, true
		}
	}
	return "", false
}
