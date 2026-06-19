package vm

// NetworkPlumber handles tap device and OVS bridge operations. Defined in vm
// so the manager avoids importing network/host. All methods must be idempotent.
type NetworkPlumber interface {
	SetupTap(spec TapSpec) error
	CleanupTap(name string) error

	// AttachIMDSDatapath installs the per-tap IMDS datapath (br-imds endpoint,
	// demux/egress flows, reply routing) for a primary-ENI tap. mac is the guest
	// MAC; subnetID supplies the gateway MAC the guest sends .254/.253 to. This
	// realises only the host datapath — serving is wired separately by the IMDS
	// responder's reconcile-from-taps.
	AttachIMDSDatapath(eniID, mac, subnetID string) error
}
