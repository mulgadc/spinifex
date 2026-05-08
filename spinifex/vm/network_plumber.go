package vm

// NetworkPlumber handles tap device and OVS bridge operations. Defined here
// so the manager can hold the collaborator without importing the daemon
// package; the daemon's OVSNetworkPlumber type satisfies it structurally.
// Both methods must be idempotent — callers rely on safe reinvocation across
// host reboots and terminate-during-pending races.
type NetworkPlumber interface {
	SetupTap(spec TapSpec) error
	CleanupTap(name string) error
}
