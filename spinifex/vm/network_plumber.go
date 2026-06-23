package vm

import "errors"

// ErrIMDSServingDegraded marks an AttachIMDSDatapath failure that happened AFTER
// the connectivity-critical datapath (the br-imds<->br-int patch and its forward
// flows) was installed — only the IMDS demux/egress/reply stage failed. The guest
// keeps full VPC connectivity; just IMDS is unavailable, so the caller logs and
// continues. Any other AttachIMDSDatapath error means connectivity itself was not
// established and the primary tap is stranded on the secure br-imds.
var ErrIMDSServingDegraded = errors.New("imds: per-tap serving install failed (guest connectivity intact)")

// NetworkPlumber handles tap device and OVS bridge operations. Defined in vm
// so the manager avoids importing network/host. All methods must be idempotent.
type NetworkPlumber interface {
	SetupTap(spec TapSpec) error
	CleanupTap(name string) error

	// EnsureIMDSDatapathBridge idempotently creates the dedicated IMDS bridge.
	// The primary-ENI tap is placed on it by SetupTap, so it must exist first.
	EnsureIMDSDatapathBridge() error

	// AttachIMDSDatapath installs the per-tap IMDS datapath (br-imds<->br-int
	// patch, endpoint, demux/egress flows, forward flows, reply routing) for a
	// primary-ENI tap whose tap is already on br-imds. mac is the guest MAC;
	// subnetID supplies the gateway MAC the guest sends .254/.253 to. This
	// realises only the host datapath — serving is wired separately by the IMDS
	// responder's reconcile-from-taps.
	AttachIMDSDatapath(eniID, mac, subnetID string) error

	// DetachIMDSDatapath tears down the per-tap IMDS datapath installed by
	// AttachIMDSDatapath (reply routing, flows, patch pair, endpoint), leaving
	// the shared br-imds bridge in place. Called at terminate; idempotent, so
	// safe for an ENI that never had a datapath installed.
	DetachIMDSDatapath(eniID string) error
}
