package vm

import "errors"

// ErrIMDSServingDegraded marks an AttachIMDSDatapath failure after the
// connectivity-critical datapath (patch + forward flows) was installed — only the
// IMDS demux/egress/reply stage failed, so the guest keeps full VPC connectivity and
// the caller logs and continues. Any other error means connectivity was not set up.
var ErrIMDSServingDegraded = errors.New("imds: per-tap serving install failed (guest connectivity intact)")

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
}
