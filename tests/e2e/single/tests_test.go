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
// that share package-scoped IAM state; TestFinalClusterStats (last gate).

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

// TestReplaceRouteConvergence owns its own scratch VPCs/IGWs end to end and
// never touches the singleton or default VPC, so it is parallel-safe.
func TestReplaceRouteConvergence(t *testing.T) {
	t.Parallel()
	runReplaceRouteConvergence(t, requireSingleNodeFixture(t))
}

// --- Sequential: singleton VM lifecycle ---

// TestClusterStatsCLI asserts 0-VM baseline state before any instance launches.
func TestClusterStatsCLI(t *testing.T) {
	runClusterStatsCLI(t, requireSingleNodeFixture(t))
}

// TestDefaultSGReachabilityBaseline, TestNewVPCEgressBaseline, and TestSameSGComms
// own all mutable resources and run before the singleton launch.
func TestDefaultSGReachabilityBaseline(t *testing.T) {
	runDefaultSGReachabilityBaseline(t, requireSingleNodeFixture(t))
}

func TestNewVPCEgressBaseline(t *testing.T) {
	runNewVPCEgressBaseline(t, requireSingleNodeFixture(t))
}

func TestSameSGComms(t *testing.T) {
	runSameSGComms(t, requireSingleNodeFixture(t))
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

// TestENIHotplug hot-plugs a secondary ENI onto the freshly-booted singleton
// and asserts the NIC reaches the guest, then restores it. Placed before the
// stop/start churn so the singleton is known running + SSH-healthy.
func TestENIHotplug(t *testing.T) {
	runENIHotplug(t, requireSingleNodeFixture(t))
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

// TestSnapshotRestore writes guest data, snapshots, restores, and re-reads it.
func TestSnapshotRestore(t *testing.T) {
	runSnapshotRestore(t, requireSingleNodeFixture(t))
}

func TestCreateImage(t *testing.T) {
	runCreateImage(t, requireSingleNodeFixture(t))
}

// TestCreateImageData bakes an AMI carrying a root-fs sentinel and verifies it
// on a fresh instance launched from that AMI (costs one extra boot).
func TestCreateImageData(t *testing.T) {
	runCreateImageData(t, requireSingleNodeFixture(t))
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

// TestInstanceEIP allocates an Elastic IP, associates it to a throwaway VM,
// and asserts the EIP datapath flips on/off with the association. Sequential:
// it authorizes ingress on the shared default SG.
func TestInstanceEIP(t *testing.T) {
	runInstanceEIP(t, requireSingleNodeFixture(t))
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

// TestIAMRolesAndProfiles + TestIAMInstanceProfileAssociation are sequential:
// the second recreates role/profile names the first tore down.
func TestIAMRolesAndProfiles(t *testing.T) {
	runIAMRolesAndProfiles(t, requireSingleNodeFixture(t))
}

// TestIAMInstanceProfileAssociation mutates the singleton VM, so it cannot
// share the parallel bucket.
func TestIAMInstanceProfileAssociation(t *testing.T) {
	runIAMInstanceProfileAssociation(t, requireSingleNodeFixture(t))
}

// TestSTSAssumeRoleAndGetCallerIdentity is sequential to avoid racing trust-policy
// mutations against a parallel AssumeRole.
func TestSTSAssumeRoleAndGetCallerIdentity(t *testing.T) {
	runSTS(t, requireSingleNodeFixture(t))
}

// TestAssumedRoleControlPlaneEnforcement verifies a zero-policy assumed-role
// is denied and permitted once a policy is attached. Sequential to avoid
// racing the mid-test grant.
func TestAssumedRoleControlPlaneEnforcement(t *testing.T) {
	runAssumedRoleControlPlaneEnforcement(t, requireSingleNodeFixture(t))
}

// TestIMDS exercises IMDSv2 end-to-end: token issuance, metadata surface,
// instance-role credentials, OVN datapath, and cross-VPC isolation. Sequential
// because it launches profile-bound VMs and creates a fresh VPC.
func TestIMDS(t *testing.T) {
	runIMDS(t, requireSingleNodeFixture(t))
}

// TestFinalClusterStats runs as the last sequential test.
func TestFinalClusterStats(t *testing.T) {
	runFinalClusterStats(t, requireSingleNodeFixture(t))
}
