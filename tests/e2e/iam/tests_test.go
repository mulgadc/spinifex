//go:build e2e

package iam

import "testing"

// Top-level Test* entry points for the IAM/STS + instance-identity suite. Each
// delegates to a runX function in the matching <name>_test.go file. Kept as
// separate top-level Test* (not one inline Test) so `go test -run TestX` and the
// suite selector can address each individually.
//
// Execution is sequential in source order: these tests share the package-scoped
// IAM state (users/policies/roles are account-flat), and TestIAMCleanup tears
// down the user/policy graph the earlier phases build. The parallelism win of
// the split is the suite-as-matrix-leg, not intra-suite concurrency — sub-tests
// inside each runIAM* still fan out with t.Parallel where it is safe.
//
// The final two entries (TestIAMInstanceProfileAssociation, TestIMDS) boot guest
// VMs. They self-bootstrap their own roles/profiles/VPCs under collision-safe
// namespaces and run last, after the control-plane phases.

func TestIAMUserCRUD(t *testing.T) {
	runIAMUserCRUD(t, requireIAMFixture(t))
}

func TestIAMAccessKeyLifecycle(t *testing.T) {
	runIAMAccessKeyLifecycle(t, requireIAMFixture(t))
}

func TestIAMUserAuthentication(t *testing.T) {
	runIAMUserAuthentication(t, requireIAMFixture(t))
}

func TestIAMPolicyCRUD(t *testing.T) {
	runIAMPolicyCRUD(t, requireIAMFixture(t))
}

func TestIAMPolicyAttachmentEnforcement(t *testing.T) {
	runIAMPolicyAttachmentEnforcement(t, requireIAMFixture(t))
}

func TestIAMPolicyLifecycle(t *testing.T) {
	runIAMPolicyLifecycle(t, requireIAMFixture(t))
}

func TestIAMCleanup(t *testing.T) {
	runIAMCleanup(t, requireIAMFixture(t))
}

func TestIAMRolesAndProfiles(t *testing.T) {
	runIAMRolesAndProfiles(t, requireIAMFixture(t))
}

// TestSTSAssumeRoleAndGetCallerIdentity is sequential to avoid racing trust-policy
// mutations against a parallel AssumeRole.
func TestSTSAssumeRoleAndGetCallerIdentity(t *testing.T) {
	runSTS(t, requireIAMFixture(t))
}

// TestAssumedRoleControlPlaneEnforcement verifies a zero-policy assumed-role is
// denied and permitted once a policy is attached. Sequential to avoid racing
// the mid-test grant.
func TestAssumedRoleControlPlaneEnforcement(t *testing.T) {
	runAssumedRoleControlPlaneEnforcement(t, requireIAMFixture(t))
}

// TestIAMGroupsLifecycle exercises group CRUD, membership, group-policy
// attachment, the reverse-lookup listings, and every deletion guard. Sequential:
// it creates/destroys its own dedicated group/user/policy graph.
func TestIAMGroupsLifecycle(t *testing.T) {
	runIAMGroupsLifecycle(t, requireIAMFixture(t))
}

// TestIAMGroupEnforcement proves a group-attached policy grants its permission
// to members: denied with no grant, allowed once in the group, denied again
// after leaving. Sequential to avoid racing the mid-test grant.
func TestIAMGroupEnforcement(t *testing.T) {
	runIAMGroupEnforcement(t, requireIAMFixture(t))
}

// TestIAMInstanceProfileAssociation exercises the EC2 IAM instance-profile
// association lifecycle (associate/replace/disassociate, RunInstances
// --iam-instance-profile, auto-disassociate on terminate). It boots its own
// instance and self-bootstraps a role + two profiles under the
// single-iamprofile- namespace, distinct from this suite's app-role/app-profile.
// Sequential: it boots VMs and mutates instance state.
func TestIAMInstanceProfileAssociation(t *testing.T) {
	runIAMInstanceProfileAssociation(t, requireIAMFixture(t))
}

// TestIMDS exercises IMDSv2 end-to-end: token issuance, metadata surface,
// instance-role credentials, OVN datapath, and cross-VPC isolation. Sequential
// because it launches profile-bound VMs and creates fresh VPCs (imds-e2e-*).
func TestIMDS(t *testing.T) {
	runIMDS(t, requireIAMFixture(t))
}
