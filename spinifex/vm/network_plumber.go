package vm

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
}
