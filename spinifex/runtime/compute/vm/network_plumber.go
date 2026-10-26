package vm

import (
	"context"
	"errors"
)

// ErrIMDSServingDegraded marks an AttachIMDSDatapath failure after the
// connectivity-critical datapath (patch + forward flows) was installed — only the
// IMDS demux/egress/reply stage failed, so the guest keeps VPC connectivity but
// cannot reach IMDS. Since guest bootstrap (SSH key, user-data, network, hostname)
// now comes from IMDS, this is launch-fatal: the caller aborts rather than boot a
// guest that would silently come up unconfigured. Any other error means connectivity
// was not set up and is equally fatal.
var ErrIMDSServingDegraded = errors.New("imds: per-tap serving install failed (guest connectivity intact)")

// ErrIMDSTapDetached is RepairIMDSDatapath declining a tap no longer on br-imds:
// its guest is being torn down, and a repair would re-create ports it just lost.
var ErrIMDSTapDetached = errors.New("imds: tap is no longer on the IMDS bridge")

// IMDSDatapathState is what survives of a primary ENI's IMDS datapath: its
// br-imds flows against the number installed, and whether its endpoint still
// carries the settings the install gave it.
type IMDSDatapathState struct {
	FlowsFound    int
	FlowsExpected int
	EndpointReady bool
}

// Intact reports whether nothing the install put in place has been lost.
func (s IMDSDatapathState) Intact() bool {
	return s.FlowsFound >= s.FlowsExpected && s.EndpointReady
}

// NetworkPlumber handles tap device and OVS bridge operations. Defined in vm
// so the manager avoids importing network/host. All methods must be idempotent.
type NetworkPlumber interface {
	SetupTap(spec TapSpec) error
	CleanupTap(name string) error

	// EnsureIMDSDatapathBridge idempotently creates the dedicated IMDS bridge.
	// The primary-ENI tap is placed on it by SetupTap, so it must exist first.
	EnsureIMDSDatapathBridge() error

	// AttachIMDSDatapath installs the per-tap IMDS datapath (patch, endpoint,
	// demux/egress/forward flows, reply routing) for a primary-ENI tap already on
	// br-imds. subnetID supplies the gateway MAC the guest sends .254/.253 to. This
	// realises only the host datapath — serving is wired separately by the responder.
	AttachIMDSDatapath(eniID, mac, subnetID string) error

	// DetachIMDSDatapath tears down the per-tap IMDS datapath (reply routing, flows,
	// patch pair, endpoint), leaving the shared br-imds bridge in place. Idempotent,
	// so safe for an ENI that never had a datapath installed.
	DetachIMDSDatapath(eniID string) error

	// IMDSDatapathStates reports, per primary ENI, what survives of its IMDS
	// datapath. One pass covers every ENI; an error means nothing was read.
	IMDSDatapathStates(ctx context.Context, eniIDs []string) (map[string]IMDSDatapathState, error)

	// RepairIMDSDatapath re-installs the IMDS datapath of a running guest whose
	// state was lost. Returns ErrIMDSTapDetached, changing nothing, when the tap
	// is no longer on br-imds. Gives up when ctx ends.
	RepairIMDSDatapath(ctx context.Context, eniID, mac, subnetID string) error

	// EnsureVPCHostPort gives the host a routed presence in a managed VPC subnet
	// by binding an ENI the daemon itself owns to an internal port on br-int.
	// addr is CIDR notation carrying the ENI address at the SUBNET's prefix
	// length, which is what installs the connected route for the rest of it.
	EnsureVPCHostPort(eniID, mac, addr string) error

	// RemoveVPCHostPort tears down an EnsureVPCHostPort port. Idempotent, so it
	// is safe for an ENI that never had one.
	RemoveVPCHostPort(eniID string) error
}
