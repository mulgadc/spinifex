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
//   - TestClusterStatsCLI                 : asserts baseline cluster state
//     (0/ CPU, "No VMs found"); must run before any launches.
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

// TestClusterStatsCLI runs before TestInstanceLaunch because it asserts
// baseline cluster state (0/ CPU used, "No VMs found") that doesn't hold
// once the singleton instance is up.
func TestClusterStatsCLI(t *testing.T) {
	runClusterStatsCLI(t, requireSingleNodeFixture(t))
}

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

// TestIAMRolesAndProfiles + TestIAMInstanceProfileAssociation form the
// companion-plan suite (docs/development/archive/feature/iam-roles-v1.md and
// docs/development/feature/iam-roles-v1-ec2.md). Sequential: the second test
// recreates the same role/profile names the first tore down, so concurrent
// runs would race the EntityAlreadyExists / NoSuchEntity boundaries.
func TestIAMRolesAndProfiles(t *testing.T) {
	runIAMRolesAndProfiles(t, requireSingleNodeFixture(t))
}

// TestIAMInstanceProfileAssociation also briefly mutates the singleton VM
// (Associate/Disassociate), so it can't share the parallel bucket with
// instance-mutation tests.
func TestIAMInstanceProfileAssociation(t *testing.T) {
	runIAMInstanceProfileAssociation(t, requireSingleNodeFixture(t))
}

// TestSTSAssumeRoleAndGetCallerIdentity exercises the STS v1 surface. Owns
// its own role, so safe alongside the IAM Roles tests above, but sequential
// so trust-policy mutations don't race a parallel AssumeRole.
func TestSTSAssumeRoleAndGetCallerIdentity(t *testing.T) {
	runSTS(t, requireSingleNodeFixture(t))
}

// TestIMDS exercises the host-served IMDSv2 surface end-to-end (imds-v1.md
// Step 11): token issuance, the v2-only stance, the metadata surface, the
// instance-role credential path + wire round-trip, DHCP option 121, the
// per-VPC localport datapath, and cross-VPC source-IP isolation. Owns its own
// role/profile + a second VPC, but sequential: it launches profile-bound VMs
// and a fresh VPC, so it must not race the singleton-mutation tests above.
func TestIMDS(t *testing.T) {
	runIMDS(t, requireSingleNodeFixture(t))
}

// TestFinalClusterStats runs as the last sequential test (parallel bucket
// runs afterwards, but those Tests are read-only / own-everything so a
// later cluster-stats snapshot would still be valid).
func TestFinalClusterStats(t *testing.T) {
	runFinalClusterStats(t, requireSingleNodeFixture(t))
}
