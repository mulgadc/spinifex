package vm

// NetworkPlumber handles tap device and OVS bridge operations. Defined in vm
// so the manager avoids importing network/host. Both methods must be idempotent.
type NetworkPlumber interface {
	SetupTap(spec TapSpec) error
	CleanupTap(name string) error
}
