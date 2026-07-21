//go:build e2e

package iam

import "testing"

// Top-level Test* entry points for the instance-identity suite. Each delegates
// to a runX function in the matching <name>_test.go file. Kept as separate
// top-level Test* (not one inline Test) so `go test -run TestX` and the suite
// selector can address each individually.
//
// The IAM/STS control-plane authz tests that used to live here have been
// ported to the in-process integration tier (tests/integration/) and removed
// — the gateway dispatches every IAM/STS action directly with no daemon hop,
// so the integration tier is a faithful, much cheaper substitute. What
// remains boots guest VMs and self-bootstraps its own roles/profiles/VPCs
// under collision-safe namespaces.

// TestIAMInstanceProfileAssociation exercises the EC2 IAM instance-profile
// association lifecycle (associate/replace/disassociate, RunInstances
// --iam-instance-profile, auto-disassociate on terminate). It boots its own
// instance and self-bootstraps a role + two profiles under the
// single-iamprofile- namespace.
func TestIAMInstanceProfileAssociation(t *testing.T) {
	runIAMInstanceProfileAssociation(t, requireIAMFixture(t))
}

// TestIMDS exercises IMDSv2 end-to-end: token issuance, metadata surface,
// instance-role credentials, OVN datapath, and cross-VPC isolation. Sequential
// because it launches profile-bound VMs and creates fresh VPCs (imds-e2e-*).
func TestIMDS(t *testing.T) {
	runIMDS(t, requireIAMFixture(t))
}
