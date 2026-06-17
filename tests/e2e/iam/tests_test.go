//go:build e2e

package iam

import "testing"

// Top-level Test* entry points for the IAM/STS suite. Each delegates to a runX
// function in the matching <name>_test.go file. Kept as separate top-level
// Test* (not one inline Test) so `go test -run TestX` and the suite selector
// can address each individually.
//
// Execution is sequential in source order: these tests share the package-scoped
// IAM state (users/policies/roles are account-flat), and TestIAMCleanup tears
// down the user/policy graph the earlier phases build. The parallelism win of
// the split is the suite-as-matrix-leg, not intra-suite concurrency — sub-tests
// inside each runIAM* still fan out with t.Parallel where it is safe.

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
