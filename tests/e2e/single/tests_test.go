//go:build e2e

package single

import "testing"

// Top-level Test* entry points. Each delegates to a runX function in the
// matching <name>_test.go file. Every run* self-bootstraps prerequisites so
// `go test -run TestX` works in isolation.
//
// Parallel bucket (t.Parallel): read-only or own-everything tests that never
// mutate the singleton instance, default VPC, or default SG.
// Sequential: tests that boot, mutate, or snapshot the singleton VM; IAM tests
// that share package-scoped IAM state.

// --- Bucket #1: parallel-safe ---

func TestEnvironment(t *testing.T) {
	t.Parallel()
	runEnvironment(t, requireSingleNodeFixture(t))
}

func TestDiscovery(t *testing.T) {
	t.Parallel()
	runDiscovery(t, requireSingleNodeFixture(t))
}

func TestAccountScoping(t *testing.T) {
	t.Parallel()
	runAccountScoping(t, requireSingleNodeFixture(t))
}

func TestVPCSubnetE2E(t *testing.T) {
	t.Parallel()
	runVPCSubnetE2E(t, requireSingleNodeFixture(t))
}

// --- Sequential: singleton VM lifecycle ---

// TestClusterStatsCLI exercises the spx cluster CLI; its get-vms baseline is
// concurrency-tolerant (other suites may have VMs on the node).
func TestClusterStatsCLI(t *testing.T) {
	runClusterStatsCLI(t, requireSingleNodeFixture(t))
}

// TestDefaultSGReachabilityBaseline, TestNewVPCEgressBaseline, and
// TestGuestDNSResolution own all mutable resources and run before the
// singleton launch.
func TestDefaultSGReachabilityBaseline(t *testing.T) {
	runDefaultSGReachabilityBaseline(t, requireSingleNodeFixture(t))
}

func TestNewVPCEgressBaseline(t *testing.T) {
	runNewVPCEgressBaseline(t, requireSingleNodeFixture(t))
}

func TestGuestDNSResolution(t *testing.T) {
	runGuestDNSResolution(t, requireSingleNodeFixture(t))
}

func TestInstanceClusterStats(t *testing.T) {
	runInstanceClusterStats(t, requireSingleNodeFixture(t))
}

func TestInstanceMetadata(t *testing.T) {
	runInstanceMetadata(t, requireSingleNodeFixture(t))
}

func TestSSHProbe(t *testing.T) {
	runSSHProbe(t, requireSingleNodeFixture(t))
}

func TestConsoleOutput(t *testing.T) {
	runConsoleOutput(t, requireSingleNodeFixture(t))
}

// TestENIHotplug hot-plugs a secondary ENI onto the freshly-booted singleton
// and asserts the NIC reaches the guest, then restores it. Placed before the
// stop/start churn so the singleton is known running + SSH-healthy.
func TestENIHotplug(t *testing.T) {
	runENIHotplug(t, requireSingleNodeFixture(t))
}

// TestENIHotplugReconcile attaches a secondary ENI, restarts spinifex-daemon,
// and asserts the hot-plug reconciler keeps the ENI attached + manageable
// (detach succeeds) across the restart. Runs after TestENIHotplug so the
// singleton is known running + SSH-healthy.
func TestENIHotplugReconcile(t *testing.T) {
	runENIHotplugReconcile(t, requireSingleNodeFixture(t))
}

func TestVolumeLifecycle(t *testing.T) {
	runVolumeLifecycle(t, requireSingleNodeFixture(t))
}

func TestVolumeStatus(t *testing.T) {
	runVolumeStatus(t, requireSingleNodeFixture(t))
}

// TestVolumeDurability stops/starts the singleton; sequential, leaves it running.
func TestVolumeDurability(t *testing.T) {
	runVolumeDurability(t, requireSingleNodeFixture(t))
}

func TestSnapshotLifecycle(t *testing.T) {
	runSnapshotLifecycle(t, requireSingleNodeFixture(t))
}

func TestSnapshotBackedLaunch(t *testing.T) {
	runSnapshotBackedLaunch(t, requireSingleNodeFixture(t))
}

func TestCreateImage(t *testing.T) {
	runCreateImage(t, requireSingleNodeFixture(t))
}

func TestSecurityGroupEgress(t *testing.T) {
	runSecurityGroupEgress(t, requireSingleNodeFixture(t))
}

func TestStopStart(t *testing.T) {
	runStopStart(t, requireSingleNodeFixture(t))
}

func TestAttachToStoppedError(t *testing.T) {
	runAttachToStoppedError(t, requireSingleNodeFixture(t))
}

func TestModifyInstanceAttribute(t *testing.T) {
	runModifyInstanceAttribute(t, requireSingleNodeFixture(t))
}

func TestRebootInstance(t *testing.T) {
	runRebootInstance(t, requireSingleNodeFixture(t))
}

// --- Sequential: shared-state / sub-test-parallel Tests ---

func TestNegativeErrorPaths(t *testing.T) {
	runNegativeErrorPaths(t, requireSingleNodeFixture(t))
}

func TestNATGateway(t *testing.T) {
	runNATGateway(t, requireSingleNodeFixture(t))
}

// TestInstanceEIP allocates an Elastic IP, associates it to a throwaway VM,
// and asserts the EIP datapath flips on/off with the association. Sequential:
// it authorizes ingress on the shared default SG.
func TestInstanceEIP(t *testing.T) {
	runInstanceEIP(t, requireSingleNodeFixture(t))
}

// TestSGPolicyDatapath merges the former TestSameSGComms and
// TestSGToSGDatapath around one shared client/target pair (see
// runSGPolicyDatapath for the stage breakdown and gating rationale).
func TestSGPolicyDatapath(t *testing.T) {
	runSGPolicyDatapath(t, requireSingleNodeFixture(t))
}
