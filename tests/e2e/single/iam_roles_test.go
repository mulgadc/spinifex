//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Cross-phase identifiers for IAM Role / InstanceProfile tests. The names
// mirror the bash driver (run-e2e.sh IAM Phase 8 / 9) so a future
// side-by-side diff stays greppable.
const (
	iamRoleAppName            = "app-role"
	iamProfileAppName         = "app-profile"
	iamProfileOtherName       = "other-profile"
	iamPolicyAdministrator    = "AdministratorAccess"
	iamTrustPolicyEC2Standard = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	iamTrustPolicyEC2V2       = `{"Version":"2012-10-17","Statement":[{"Sid":"v2","Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
)

// runIAMRolesAndProfiles ports run-e2e.sh IAM Phase 8: Role + InstanceProfile
// CRUD, attach-role-policy, add-role-to-instance-profile, the one-role and
// delete-while-referenced guards, and full teardown. Runs sequentially under
// TestIAMRolesAndProfiles so cleanup never collides with TestIAMInstanceProfileAssociation
// which recreates the same names.
func runIAMRolesAndProfiles(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — IAM Roles & Instance Profiles")
	adminAccount := iamEnsureAdminAccountID(t, fix)
	roleARN := iamRoleARN(adminAccount, iamRoleAppName)
	adminPolicyARN := iamPolicyARN(adminAccount, iamPolicyAdministrator)

	// Defensive sweep — previous failed run may have left fragments. Order
	// matters: detach role from profile before delete-instance-profile,
	// detach policy from role before delete-role.
	iamDeleteRoleAndProfilesBestEffort(fix, iamRoleAppName,
		[]string{iamProfileAppName, iamProfileOtherName}, adminPolicyARN)
	fix.Harness.RegisterCleanup(func() {
		iamDeleteRoleAndProfilesBestEffort(fix, iamRoleAppName,
			[]string{iamProfileAppName, iamProfileOtherName}, adminPolicyARN)
	})

	// CreateRole — happy path with non-default description.
	harness.Step(t, "create-role %q", iamRoleAppName)
	createOut, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(iamRoleAppName),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
		Description:              aws.String("E2E test role"),
	})
	require.NoError(t, err, "create-role")
	require.Equal(t, iamRoleAppName, aws.StringValue(createOut.Role.RoleName))
	require.Equal(t, roleARN, aws.StringValue(createOut.Role.Arn),
		"role ARN must follow arn:aws:iam::<acct>:role/<name>")
	harness.Detail(t, "role", iamRoleAppName, "arn", roleARN)

	// Duplicate.
	harness.Step(t, "create-role duplicate (expect EntityAlreadyExists)")
	harness.ExpectError(t, "EntityAlreadyExists", func() error {
		_, e := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
			RoleName:                 aws.String(iamRoleAppName),
			AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
		})
		return e
	})

	// Malformed trust policy → MalformedPolicyDocument.
	harness.Step(t, "create-role malformed trust policy (expect MalformedPolicyDocument)")
	harness.ExpectError(t, "MalformedPolicyDocument", func() error {
		_, e := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
			RoleName:                 aws.String("bad-role"),
			AssumeRolePolicyDocument: aws.String(`{not valid json`),
		})
		return e
	})

	// GetRole + NoSuchEntity probe.
	harness.Step(t, "get-role %q", iamRoleAppName)
	got, err := fix.AWS.IAM.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "get-role")
	require.Equal(t, iamRoleAppName, aws.StringValue(got.Role.RoleName))

	harness.Step(t, "get-role nonexistent (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetRole(&iam.GetRoleInput{RoleName: aws.String("ghost-role")})
		return e
	})

	// ListRoles + PathPrefix scan.
	harness.Step(t, "list-roles (>= 1)")
	listed, err := fix.AWS.IAM.ListRoles(&iam.ListRolesInput{})
	require.NoError(t, err, "list-roles")
	require.GreaterOrEqual(t, len(listed.Roles), 1,
		"expected >= 1 role, got %d", len(listed.Roles))

	harness.Step(t, "list-roles --path-prefix /")
	pp, err := fix.AWS.IAM.ListRoles(&iam.ListRolesInput{PathPrefix: aws.String("/")})
	require.NoError(t, err, "list-roles --path-prefix /")
	require.GreaterOrEqual(t, len(pp.Roles), 1, "path-prefix / must surface roles at /")

	// UpdateRole — description + MaxSessionDuration round-trip + range guard.
	harness.Step(t, "update-role description + max-session-duration=7200")
	_, err = fix.AWS.IAM.UpdateRole(&iam.UpdateRoleInput{
		RoleName:           aws.String(iamRoleAppName),
		Description:        aws.String("updated"),
		MaxSessionDuration: aws.Int64(7200),
	})
	require.NoError(t, err, "update-role")
	got, err = fix.AWS.IAM.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "get-role after update")
	require.Equal(t, "updated", aws.StringValue(got.Role.Description))
	require.Equal(t, int64(7200), aws.Int64Value(got.Role.MaxSessionDuration))

	harness.Step(t, "update-role max-session-duration=60 (out of range, expect ValidationError)")
	harness.ExpectError(t, "ValidationError", func() error {
		_, e := fix.AWS.IAM.UpdateRole(&iam.UpdateRoleInput{
			RoleName:           aws.String(iamRoleAppName),
			MaxSessionDuration: aws.Int64(60),
		})
		return e
	})

	// UpdateAssumeRolePolicy — swap document, no enforcement yet.
	harness.Step(t, "update-assume-role-policy")
	_, err = fix.AWS.IAM.UpdateAssumeRolePolicy(&iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(iamRoleAppName),
		PolicyDocument: aws.String(iamTrustPolicyEC2V2),
	})
	require.NoError(t, err, "update-assume-role-policy")

	// AttachRolePolicy — idempotent re-attach must not grow the count.
	harness.Step(t, "attach-role-policy %s <- %s", iamRoleAppName, iamPolicyAdministrator)
	_, err = fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(iamRoleAppName),
		PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "attach-role-policy")

	attached, err := fix.AWS.IAM.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "list-attached-role-policies")
	require.Len(t, attached.AttachedPolicies, 1)

	_, err = fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(iamRoleAppName),
		PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "idempotent re-attach")
	reAttached, err := fix.AWS.IAM.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(iamRoleAppName),
	})
	require.NoError(t, err)
	require.Len(t, reAttached.AttachedPolicies, 1, "re-attach must be idempotent")

	// Attach unknown policy → NoSuchEntity.
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
			RoleName:  aws.String(iamRoleAppName),
			PolicyArn: aws.String(iamPolicyARN(adminAccount, "Ghost")),
		})
		return e
	})

	// CreateInstanceProfile — primary + replace-target.
	harness.Step(t, "create-instance-profile %q", iamProfileAppName)
	prim, err := fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName),
	})
	require.NoError(t, err, "create-instance-profile primary")
	require.Equal(t, iamProfileAppName, aws.StringValue(prim.InstanceProfile.InstanceProfileName))

	harness.Step(t, "create-instance-profile %q", iamProfileOtherName)
	_, err = fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileOtherName),
	})
	require.NoError(t, err, "create-instance-profile other")

	harness.Step(t, "create-instance-profile duplicate (expect EntityAlreadyExists)")
	harness.ExpectError(t, "EntityAlreadyExists", func() error {
		_, e := fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(iamProfileAppName),
		})
		return e
	})

	// GetInstanceProfile / ListInstanceProfiles.
	gotProf, err := fix.AWS.IAM.GetInstanceProfile(&iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName),
	})
	require.NoError(t, err, "get-instance-profile")
	require.Equal(t, iamProfileAppName, aws.StringValue(gotProf.InstanceProfile.InstanceProfileName))

	listedProf, err := fix.AWS.IAM.ListInstanceProfiles(&iam.ListInstanceProfilesInput{})
	require.NoError(t, err, "list-instance-profiles")
	require.GreaterOrEqual(t, len(listedProf.InstanceProfiles), 2)

	// AddRoleToInstanceProfile — primary; second add rejected by one-role limit.
	harness.Step(t, "add-role-to-instance-profile %s <- %s", iamProfileAppName, iamRoleAppName)
	_, err = fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName),
		RoleName:            aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "add-role-to-instance-profile primary")

	harness.Step(t, "add-role-to-instance-profile second role (expect LimitExceeded)")
	harness.ExpectError(t, "LimitExceeded", func() error {
		_, e := fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(iamProfileAppName),
			RoleName:            aws.String(iamRoleAppName),
		})
		return e
	})

	// ListInstanceProfilesForRole — reverse lookup.
	rev, err := fix.AWS.IAM.ListInstanceProfilesForRole(&iam.ListInstanceProfilesForRoleInput{
		RoleName: aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "list-instance-profiles-for-role")
	require.Len(t, rev.InstanceProfiles, 1,
		"reverse lookup should see only the profile we added")

	// DeleteRole guards: attached policy + still referenced by a profile.
	harness.Step(t, "delete-role while attached (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(iamRoleAppName)})
		return e
	})

	// DeleteInstanceProfile guard: role still attached.
	harness.Step(t, "delete-instance-profile while role attached (expect DeleteConflict)")
	harness.ExpectError(t, "DeleteConflict", func() error {
		_, e := fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(iamProfileAppName),
		})
		return e
	})

	// Full teardown so TestIAMInstanceProfileAssociation gets a clean slate.
	// remove-role then delete-profile, detach-policy then delete-role, in
	// that order — same as iamDeleteRoleAndProfilesBestEffort but asserted.
	harness.Step(t, "remove-role-from-instance-profile %s", iamProfileAppName)
	_, err = fix.AWS.IAM.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName),
		RoleName:            aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "remove-role-from-instance-profile")

	_, err = fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName),
	})
	require.NoError(t, err, "delete-instance-profile primary")

	_, err = fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileOtherName),
	})
	require.NoError(t, err, "delete-instance-profile other")

	_, err = fix.AWS.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
		RoleName:  aws.String(iamRoleAppName),
		PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "detach-role-policy")

	_, err = fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "delete-role")

	harness.Step(t, "get-role after delete (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
		return e
	})
}

// iamRoleARN constructs arn:aws:iam::<acct>:role/<name>.
func iamRoleARN(account, name string) string {
	return "arn:aws:iam::" + account + ":role/" + name
}

// iamDeleteRoleAndProfilesBestEffort tears down every fragment of a role +
// profile graph the suite might have left behind. Each step swallows errors
// so a missing fragment doesn't cascade. Used both as a pre-test sweep and
// as a fixture-teardown cleanup.
func iamDeleteRoleAndProfilesBestEffort(fix *Fixture, roleName string, profileNames []string, policyARNs ...string) {
	for _, p := range profileNames {
		// Drop role-from-profile binding (idempotent).
		_, _ = fix.AWS.IAM.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(p),
			RoleName:            aws.String(roleName),
		})
		_, _ = fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(p),
		})
	}
	for _, arn := range policyARNs {
		_, _ = fix.AWS.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
			RoleName:  aws.String(roleName),
			PolicyArn: aws.String(arn),
		})
	}
	// Defensive: pull any other attached policies we don't know about so the
	// final DeleteRole isn't blocked by a stray attach from a partial run.
	if attached, err := fix.AWS.IAM.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
		RoleName: aws.String(roleName),
	}); err == nil {
		for _, p := range attached.AttachedPolicies {
			_, _ = fix.AWS.IAM.DetachRolePolicy(&iam.DetachRolePolicyInput{
				RoleName:  aws.String(roleName),
				PolicyArn: p.PolicyArn,
			})
		}
	}
	_, _ = fix.AWS.IAM.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(roleName)})
}
