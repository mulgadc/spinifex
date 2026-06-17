//go:build e2e

package iam

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
	iamProfileEngName         = "eng-profile"
	iamProfileEngPath         = "/eng/"
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
	adminAccount := harness.IAMAccountID(t, fix.AWS)
	roleARN := harness.IAMRoleARN(adminAccount, iamRoleAppName)
	adminPolicyARN := harness.IAMPolicyARN(adminAccount, iamPolicyAdministrator)

	// Defensive sweep — previous failed run may have left fragments. Order
	// matters: detach role from profile before delete-instance-profile,
	// detach policy from role before delete-role.
	harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, iamRoleAppName,
		[]string{iamProfileAppName, iamProfileOtherName, iamProfileEngName}, adminPolicyARN)
	fix.Harness.RegisterCleanup(func() {
		harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, iamRoleAppName,
			[]string{iamProfileAppName, iamProfileOtherName, iamProfileEngName}, adminPolicyARN)
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

	// Server-side MaxSessionDuration range guard (900-43200) isn't reachable
	// via the AWS SDK: UpdateRoleInput carries min:"3600" so SDK.Validate()
	// blocks values < 3600 before dispatch, and botocore enforces the upper
	// bound the same way. The gateway guard is covered by
	// handlers/iam/roles_test.go TestCreateRole_MaxSessionDuration_TooSmall.

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
			PolicyArn: aws.String(harness.IAMPolicyARN(adminAccount, "Ghost")),
		})
		return e
	})

	// List attached policies for a role that doesn't exist → NoSuchEntity
	// (must not be masked into an empty list).
	harness.Step(t, "list-attached-role-policies ghost role (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
			RoleName: aws.String("ghost-role"),
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

	harness.Step(t, "get-instance-profile ghost (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.GetInstanceProfile(&iam.GetInstanceProfileInput{
			InstanceProfileName: aws.String("ghost-profile"),
		})
		return e
	})

	listedProf, err := fix.AWS.IAM.ListInstanceProfiles(&iam.ListInstanceProfilesInput{})
	require.NoError(t, err, "list-instance-profiles")
	require.GreaterOrEqual(t, len(listedProf.InstanceProfiles), 2)

	// Explicit Path → the ARN carries the path between instance-profile and the
	// name (arn:...:instance-profile/eng/<name>). Created, asserted, and torn
	// down inline so it doesn't perturb the count assertions above.
	harness.Step(t, "create-instance-profile %q path=%s (ARN carries the path)", iamProfileEngName, iamProfileEngPath)
	engProf, err := fix.AWS.IAM.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileEngName),
		Path:                aws.String(iamProfileEngPath),
	})
	require.NoError(t, err, "create-instance-profile with explicit path")
	require.Equal(t,
		"arn:aws:iam::"+adminAccount+":instance-profile"+iamProfileEngPath+iamProfileEngName,
		aws.StringValue(engProf.InstanceProfile.Arn),
		"explicit-Path instance-profile ARN must embed the path")
	_, err = fix.AWS.IAM.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileEngName),
	})
	require.NoError(t, err, "delete-instance-profile (explicit path)")

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

	// Ghost-entity binds → NoSuchEntity. Role is validated before the profile, so
	// a real profile + ghost role and a real role + ghost profile both surface
	// NoSuchEntity rather than silently no-op'ing.
	harness.Step(t, "add-role-to-instance-profile ghost role (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(iamProfileOtherName),
			RoleName:            aws.String("ghost-role"),
		})
		return e
	})
	harness.Step(t, "add-role-to-instance-profile ghost profile (expect NoSuchEntity)")
	harness.ExpectError(t, "NoSuchEntity", func() error {
		_, e := fix.AWS.IAM.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String("ghost-profile"),
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
