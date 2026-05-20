//go:build e2e

package single

import "testing"

// Top-level Test* entry points. Each delegates to a phaseN_X function in
// this package, passing the package-scoped Fixture singleton.
//
// Every phase function self-bootstraps any prerequisite resource it needs
// via harness.Discover* / harness.Ensure* (or the package-local need* /
// iamEnsure* helpers), so a targeted `go test -run TestX` run works in
// isolation without depending on a sibling Test* having stashed state.

// Discovery + environment.
func TestEnvironment(t *testing.T)         { phase1_Environment(t, requireSingleNodeFixture(t)) }
func TestClusterStatsCLI(t *testing.T)     { phase1b_ClusterStats(t, requireSingleNodeFixture(t)) }
func TestDiscovery(t *testing.T)           { phase2_Discovery(t, requireSingleNodeFixture(t)) }
func TestSerialConsoleAccess(t *testing.T) { phase2b_SerialConsole(t, requireSingleNodeFixture(t)) }

// Key pairs + image.
func TestKeyPairs(t *testing.T) { phase3_KeyPairs(t, requireSingleNodeFixture(t)) }
func TestImage(t *testing.T)    { phase4_Image(t, requireSingleNodeFixture(t)) }

// Instance lifecycle.
func TestInstanceLaunch(t *testing.T) {
	phase5_LaunchInstance(t, requireSingleNodeFixture(t))
}
func TestInstanceClusterStats(t *testing.T) {
	phase5aPre_ClusterStats(t, requireSingleNodeFixture(t))
}
func TestInstanceMetadata(t *testing.T) {
	phase5a_Metadata(t, requireSingleNodeFixture(t))
}
func TestSSHProbe(t *testing.T) {
	phase5aii_SSHProbe(t, requireSingleNodeFixture(t))
}
func TestConsoleOutput(t *testing.T) {
	phase5aiii_ConsoleOutput(t, requireSingleNodeFixture(t))
}

// Volume + snapshot + AMI.
func TestVolumeLifecycle(t *testing.T) {
	phase5b_VolumeLifecycle(t, requireSingleNodeFixture(t))
}
func TestVolumeStatus(t *testing.T) {
	phase5bii_VolumeStatus(t, requireSingleNodeFixture(t))
}
func TestSnapshotLifecycle(t *testing.T) {
	phase5c_SnapshotLifecycle(t, requireSingleNodeFixture(t))
}
func TestSnapshotBackedLaunch(t *testing.T) {
	phase5d_SnapshotBackedLaunch(t, requireSingleNodeFixture(t))
}
func TestCreateImage(t *testing.T) {
	phase5e_CreateImage(t, requireSingleNodeFixture(t))
}
func TestSecurityGroupEgress(t *testing.T) {
	phase5f_SecurityGroupEgress(t, requireSingleNodeFixture(t))
}

// Tags + lifecycle transitions.
func TestTagManagement(t *testing.T) { phase6_TagManagement(t, requireSingleNodeFixture(t)) }
func TestStopStart(t *testing.T)     { phase7_StopStart(t, requireSingleNodeFixture(t)) }
func TestAttachToStoppedError(t *testing.T) {
	phase7a_AttachToStoppedError(t, requireSingleNodeFixture(t))
}
func TestModifyInstanceAttribute(t *testing.T) {
	phase7b_ModifyInstanceAttribute(t, requireSingleNodeFixture(t))
}
func TestRebootInstance(t *testing.T) {
	phase7cPre_Reboot(t, requireSingleNodeFixture(t))
}
func TestRunInstancesMultiCount(t *testing.T) {
	phase7c_RunInstancesMultiCount(t, requireSingleNodeFixture(t))
}

// Negative / error paths.
func TestNegativeErrorPaths(t *testing.T) {
	phase8_NegativeErrorPaths(t, requireSingleNodeFixture(t))
}

// IAM 1–7.
func TestIAM1_UserCRUD(t *testing.T) {
	phaseIAM1_UserCRUD(t, requireSingleNodeFixture(t))
}
func TestIAM2_AccessKeyLifecycle(t *testing.T) {
	phaseIAM2_AccessKeyLifecycle(t, requireSingleNodeFixture(t))
}
func TestIAM3_UserAuthentication(t *testing.T) {
	phaseIAM3_UserAuthentication(t, requireSingleNodeFixture(t))
}
func TestIAM4_PolicyCRUD(t *testing.T) {
	phaseIAM4_PolicyCRUD(t, requireSingleNodeFixture(t))
}
func TestIAM5_PolicyAttachmentEnforcement(t *testing.T) {
	phaseIAM5_PolicyAttachmentEnforcement(t, requireSingleNodeFixture(t))
}
func TestIAM6_PolicyLifecycle(t *testing.T) {
	phaseIAM6_PolicyLifecycle(t, requireSingleNodeFixture(t))
}
func TestIAM7_Cleanup(t *testing.T) {
	phaseIAM7_Cleanup(t, requireSingleNodeFixture(t))
}

// Account scoping.
func TestAccountScoping(t *testing.T) {
	phase8Acct_AccountScoping(t, requireSingleNodeFixture(t))
}

// VPC / NAT / datapath.
func TestVPCSubnetE2E(t *testing.T) {
	phase8b_VPCSubnetE2E(t, requireSingleNodeFixture(t))
}
func TestRouteTableValidation(t *testing.T) {
	phase8c_RouteTableValidation(t, requireSingleNodeFixture(t))
}
func TestNATGateway(t *testing.T)     { phase8d_NATGateway(t, requireSingleNodeFixture(t)) }
func TestSGToSGDatapath(t *testing.T) { phase8e_SGToSGDatapath(t, requireSingleNodeFixture(t)) }

// Final cluster sanity (replaces the umbrella's phase9b — phase9 + phase9a
// deleted; harness.Fixture.Close drains cleanups at process exit).
func TestFinalClusterStats(t *testing.T) {
	phase9b_FinalClusterStats(t, requireSingleNodeFixture(t))
}
