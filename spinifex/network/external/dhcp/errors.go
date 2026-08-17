package dhcp

import "errors"

// ReleaseCodeLeaseNotTracked is the releaseWireReply.Code for a release whose
// IP has no lease in the store. It travels on the wire so callers can branch on
// the condition without matching the human-readable error text.
const ReleaseCodeLeaseNotTracked = "lease_not_tracked"

// ErrLeaseNotTracked reports that a release named an IP the lease store does not
// hold, so nothing was sent upstream and the upstream lease may be stranded.
//
// Terminal, not transient: no retry can make a deleted lease reappear. Callers
// driving a teardown must stop rather than re-drive, and should record the
// address as leaked.
var ErrLeaseNotTracked = errors.New("dhcp: no tracked lease for ip; upstream lease may be stranded")
