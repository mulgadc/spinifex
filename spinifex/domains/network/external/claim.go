package external

import "context"

// ClaimResult reports what an affinity pass moved, so a caller can log it and a
// test can assert on it without reading a fake's internals.
type ClaimResult struct {
	// Claimed is the public addresses this node took over.
	Claimed []string
	// Local is how many bindings this node already owned, moved or not.
	Local int
	// Stalled is every address whose guest runs here and which the provider is
	// not delivering here, after the pass has tried to move it and re-read the
	// answer. Empty is the healthy result.
	Stalled []StalledAddress
}

// StalledAddress is one address a pass could not put where its guest is.
//
// It carries the ENI rather than the instance because that is what this layer
// knows: a caller that wants to name the guest has the port-to-instance map.
type StalledAddress struct {
	PublicIP string
	ENIID    string
	Reason   string
}

// AddressClaimer is a cloud pool whose addresses must be delivered by the
// provider to the node running their owner. Every such pool is driven by the
// same pass, so a guest gets one reachability verdict across all of them.
type AddressClaimer interface {
	ClaimLocalAddresses(ctx context.Context) (ClaimResult, error)
}
