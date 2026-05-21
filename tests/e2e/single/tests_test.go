//go:build e2e

package single

import "testing"

// Top-level Test* entry points. Each delegates to a runX function in the
// matching <name>_test.go file. Names are number-free (TestX) so isolated
// runs via `go test -run TestKeyPairs` are stable.
//
// Every run* function self-bootstraps prerequisites via harness.Discover* /
// harness.Ensure* (or the package-local need* / iamEnsure* helpers), so a
// targeted `go test -run TestX` invocation works in isolation without
// relying on a sibling Test* having stashed state.
//
// Parallelism (mulga-siv-128):
//
// Bucket #1 (parallel): read-only or own-everything Tests below call
// t.Parallel(). They share the package fixture but never mutate the
// singleton EC2 instance, default VPC, or default SG.
//
// Sequential (no t.Parallel — would race the singleton VM or shared state):
//   - TestInstanceLaunch                  : first sequential — boots singleton.
//   - TestInstanceClusterStats / Metadata
//     / SSHProbe / ConsoleOutput          : read singleton.
//   - TestVolumeLifecycle / VolumeStatus  : attach to singleton.
//   - TestSnapshotLifecycle /
//     SnapshotBackedLaunch / CreateImage  : snapshot singleton.
//   - TestSecurityGroupEgress             : mutates default SG.
//   - TestStopStart / AttachToStoppedError
//     / ModifyInstanceAttribute /
//     RebootInstance / RunInstancesMultiCount : mutate singleton state.
//   - TestNegativeErrorPaths              : sub-tests already parallel inside.
//   - TestNATGateway / TestSGToSGDatapath : share EIP pool.
//   - All TestIAM*                        : share IAM policy state via the
//     package-scoped IAM fixture; sub-tests inside each already parallel.
//   - TestFinalClusterStats               : final sanity gate.

// --- Bucket #1: parallel-safe ---

func TestEnvironment(t *testing.T) {
	t.Parallel()
	runEnvironment(t, requireSingleNodeFixture(t))
}

func TestClusterStatsCLI(t *testing.T) {
	t.Parallel()
	runClusterStatsCLI(t, requireSingleNodeFixture(t))
}

func TestDiscovery(t *testing.T) {
	t.Parallel()
	runDiscovery(t, requireSingleNodeFixture(t))
}

func TestSerialConsoleAccess(t *testing.T) {
	t.Parallel()
	runSerialConsoleAccess(t, requireSingleNodeFixture(t))
}

func TestKeyPairs(t *testing.T) {
	t.Parallel()
	runKeyPairs(t, requireSingleNodeFixture(t))
}

func TestImage(t *testing.T) {
	t.Parallel()
	runImage(t, requireSingleNodeFixture(t))
}

func TestTagManagement(t *testing.T) {
	t.Parallel()
	runTagManagement(t, requireSingleNodeFixture(t))
}

func TestAccountScoping(t *testing.T) {
	t.Parallel()
	runAccountScoping(t, requireSingleNodeFixture(t))
}

func TestVPCSubnetE2E(t *testing.T) {
	t.Parallel()
	runVPCSubnetE2E(t, requireSingleNodeFixture(t))
}

func TestRouteTableValidation(t *testing.T) {
	t.Parallel()
	runRouteTableValidation(t, requireSingleNodeFixture(t))
}

// --- Sequential: singleton VM lifecycle ---

func TestInstanceLaunch(t *testing.T) {
	runInstanceLaunch(t, requireSingleNodeFixture(t))
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

func TestVolumeLifecycle(t *testing.T) {
	runVolumeLifecycle(t, requireSingleNodeFixture(t))
}

func TestVolumeStatus(t *testing.T) {
	runVolumeStatus(t, requireSingleNodeFixture(t))
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

func TestRunInstancesMultiCount(t *testing.T) {
	runRunInstancesMultiCount(t, requireSingleNodeFixture(t))
}

// --- Sequential: shared-state / sub-test-parallel Tests ---

func TestNegativeErrorPaths(t *testing.T) {
	runNegativeErrorPaths(t, requireSingleNodeFixture(t))
}

func TestNATGateway(t *testing.T) {
	runNATGateway(t, requireSingleNodeFixture(t))
}

func TestSGToSGDatapath(t *testing.T) {
	runSGToSGDatapath(t, requireSingleNodeFixture(t))
}

// IAM Tests share the package-scoped IAM fixture; sub-tests inside each
// runIAM* already use t.Parallel for fan-out.
func TestIAMUserCRUD(t *testing.T) {
	runIAMUserCRUD(t, requireSingleNodeFixture(t))
}

func TestIAMAccessKeyLifecycle(t *testing.T) {
	runIAMAccessKeyLifecycle(t, requireSingleNodeFixture(t))
}

func TestIAMUserAuthentication(t *testing.T) {
	runIAMUserAuthentication(t, requireSingleNodeFixture(t))
}

func TestIAMPolicyCRUD(t *testing.T) {
	runIAMPolicyCRUD(t, requireSingleNodeFixture(t))
}

func TestIAMPolicyAttachmentEnforcement(t *testing.T) {
	runIAMPolicyAttachmentEnforcement(t, requireSingleNodeFixture(t))
}

func TestIAMPolicyLifecycle(t *testing.T) {
	runIAMPolicyLifecycle(t, requireSingleNodeFixture(t))
}

func TestIAMCleanup(t *testing.T) {
	runIAMCleanup(t, requireSingleNodeFixture(t))
}

// TestFinalClusterStats runs as the last sequential test (parallel bucket
// runs afterwards, but those Tests are read-only / own-everything so a
// later cluster-stats snapshot would still be valid).
func TestFinalClusterStats(t *testing.T) {
	runFinalClusterStats(t, requireSingleNodeFixture(t))
}
