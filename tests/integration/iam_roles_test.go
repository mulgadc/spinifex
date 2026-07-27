//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/stretchr/testify/require"
)

// Identifiers for the Roles / InstanceProfiles suite, mirroring the E2E
// source's names so a future side-by-side diff stays greppable.
const (
	iamRoleAppName            = "app-role"
	iamProfileAppName         = "app-profile"
	iamProfileOtherName       = "other-profile"
	iamProfileEngName         = "eng-profile"
	iamProfileEngPath         = "/eng/"
	iamPolicyAdministrator    = "AdministratorAccess"
	iamTrustPolicyEC2Standard = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	iamTrustPolicyEC2V2       = `{"Version":"2012-10-17","Statement":[{"Sid":"v2","Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

	// iamDocAdministratorAccess stands in for the AWS-managed
	// AdministratorAccess policy a live account pre-seeds; a fresh harness
	// gateway doesn't seed it, so the test creates its own attachable
	// full-access policy under the same name.
	iamDocAdministratorAccess = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
)

// TestIAMRolesAndProfiles exercises Role + InstanceProfile CRUD,
// attach-role-policy, add-role-to-instance-profile, the one-role and
// delete-while-referenced guards, and full teardown. DeleteInstanceProfile
// always counts live associations over NATS (gateway/iam.go's countLive
// closure), even when the guard that actually fires is the IAM-internal
// role-still-attached check, so the ec2.IamProfileAssociation.describe
// subject must be stubbed for every DeleteInstanceProfile call in this test.
// Node discovery is already stubbed by StartGateway.
func TestIAMRolesAndProfiles(t *testing.T) {
	gw := StartGateway(t)
	iamCli := gw.IAMClient(t)
	adminAccount := gw.AccountID
	roleARN := iamRoleARN(adminAccount, iamRoleAppName)

	gw.StubSubject(t, "ec2.IamProfileAssociation.describe",
		mustMarshal(t, &ec2.DescribeIamInstanceProfileAssociationsOutput{}))

	adminPolicy, err := iamCli.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(iamPolicyAdministrator),
		PolicyDocument: aws.String(iamDocAdministratorAccess),
	})
	require.NoError(t, err, "create-policy AdministratorAccess")
	adminPolicyARN := aws.StringValue(adminPolicy.Policy.Arn)

	// CreateRole — happy path with non-default description.
	createOut, err := iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(iamRoleAppName),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
		Description:              aws.String("integration test role"),
	})
	require.NoError(t, err, "create-role")
	require.Equal(t, iamRoleAppName, aws.StringValue(createOut.Role.RoleName))
	require.Equal(t, roleARN, aws.StringValue(createOut.Role.Arn),
		"role ARN must follow arn:aws:iam::<acct>:role/<name>")

	// Duplicate.
	_, err = iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(iamRoleAppName),
		AssumeRolePolicyDocument: aws.String(iamTrustPolicyEC2Standard),
	})
	requireAWSErrorCode(t, err, "EntityAlreadyExists")

	// Malformed trust policy → MalformedPolicyDocument.
	_, err = iamCli.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String("bad-role"),
		AssumeRolePolicyDocument: aws.String(`{not valid json`),
	})
	requireAWSErrorCode(t, err, "MalformedPolicyDocument")

	// GetRole + NoSuchEntity probe.
	got, err := iamCli.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "get-role")
	require.Equal(t, iamRoleAppName, aws.StringValue(got.Role.RoleName))

	_, err = iamCli.GetRole(&iam.GetRoleInput{RoleName: aws.String("ghost-role")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// ListRoles + PathPrefix scan.
	listed, err := iamCli.ListRoles(&iam.ListRolesInput{})
	require.NoError(t, err, "list-roles")
	require.GreaterOrEqual(t, len(listed.Roles), 1, "expected >= 1 role, got %d", len(listed.Roles))

	pp, err := iamCli.ListRoles(&iam.ListRolesInput{PathPrefix: aws.String("/")})
	require.NoError(t, err, "list-roles --path-prefix /")
	require.GreaterOrEqual(t, len(pp.Roles), 1, "path-prefix / must surface roles at /")

	// UpdateRole — description + MaxSessionDuration round-trip.
	_, err = iamCli.UpdateRole(&iam.UpdateRoleInput{
		RoleName:           aws.String(iamRoleAppName),
		Description:        aws.String("updated"),
		MaxSessionDuration: aws.Int64(7200),
	})
	require.NoError(t, err, "update-role")
	got, err = iamCli.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "get-role after update")
	require.Equal(t, "updated", aws.StringValue(got.Role.Description))
	require.Equal(t, int64(7200), aws.Int64Value(got.Role.MaxSessionDuration))

	// Server-side MaxSessionDuration range guard (900-43200) isn't reachable
	// via the AWS SDK: UpdateRoleInput carries min:"3600" so SDK.Validate()
	// blocks values < 3600 before dispatch. The gateway guard is covered by
	// handlers/iam/roles_test.go TestCreateRole_MaxSessionDuration_TooSmall.

	// UpdateAssumeRolePolicy — swap document, no enforcement yet.
	_, err = iamCli.UpdateAssumeRolePolicy(&iam.UpdateAssumeRolePolicyInput{
		RoleName:       aws.String(iamRoleAppName),
		PolicyDocument: aws.String(iamTrustPolicyEC2V2),
	})
	require.NoError(t, err, "update-assume-role-policy")

	// AttachRolePolicy — idempotent re-attach must not grow the count.
	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(iamRoleAppName), PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "attach-role-policy")

	attached, err := iamCli.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "list-attached-role-policies")
	require.Len(t, attached.AttachedPolicies, 1)

	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(iamRoleAppName), PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "idempotent re-attach")
	reAttached, err := iamCli.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err)
	require.Len(t, reAttached.AttachedPolicies, 1, "re-attach must be idempotent")

	// Attach unknown policy → NoSuchEntity.
	_, err = iamCli.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName: aws.String(iamRoleAppName), PolicyArn: aws.String(iamPolicyARN(adminAccount, "Ghost")),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// List attached policies for a role that doesn't exist → NoSuchEntity
	// (must not be masked into an empty list).
	_, err = iamCli.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{RoleName: aws.String("ghost-role")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// CreateInstanceProfile — primary + replace-target.
	prim, err := iamCli.CreateInstanceProfile(&iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(iamProfileAppName)})
	require.NoError(t, err, "create-instance-profile primary")
	require.Equal(t, iamProfileAppName, aws.StringValue(prim.InstanceProfile.InstanceProfileName))

	_, err = iamCli.CreateInstanceProfile(&iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(iamProfileOtherName)})
	require.NoError(t, err, "create-instance-profile other")

	_, err = iamCli.CreateInstanceProfile(&iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(iamProfileAppName)})
	requireAWSErrorCode(t, err, "EntityAlreadyExists")

	// GetInstanceProfile / ListInstanceProfiles.
	gotProf, err := iamCli.GetInstanceProfile(&iam.GetInstanceProfileInput{InstanceProfileName: aws.String(iamProfileAppName)})
	require.NoError(t, err, "get-instance-profile")
	require.Equal(t, iamProfileAppName, aws.StringValue(gotProf.InstanceProfile.InstanceProfileName))

	_, err = iamCli.GetInstanceProfile(&iam.GetInstanceProfileInput{InstanceProfileName: aws.String("ghost-profile")})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	listedProf, err := iamCli.ListInstanceProfiles(&iam.ListInstanceProfilesInput{})
	require.NoError(t, err, "list-instance-profiles")
	require.GreaterOrEqual(t, len(listedProf.InstanceProfiles), 2)

	// Explicit Path → the ARN carries the path between instance-profile and
	// the name. Created, asserted, and torn down inline so it doesn't perturb
	// the count assertions above.
	engProf, err := iamCli.CreateInstanceProfile(&iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileEngName),
		Path:                aws.String(iamProfileEngPath),
	})
	require.NoError(t, err, "create-instance-profile with explicit path")
	require.Equal(t,
		"arn:aws:iam::"+adminAccount+":instance-profile"+iamProfileEngPath+iamProfileEngName,
		aws.StringValue(engProf.InstanceProfile.Arn),
		"explicit-Path instance-profile ARN must embed the path")
	_, err = iamCli.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(iamProfileEngName)})
	require.NoError(t, err, "delete-instance-profile (explicit path)")

	// AddRoleToInstanceProfile — primary; second add rejected by one-role limit.
	_, err = iamCli.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName), RoleName: aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "add-role-to-instance-profile primary")

	_, err = iamCli.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName), RoleName: aws.String(iamRoleAppName),
	})
	requireAWSErrorCode(t, err, "LimitExceeded")

	// Ghost-entity binds → NoSuchEntity. Role is validated before the profile,
	// so a real profile + ghost role and a real role + ghost profile both
	// surface NoSuchEntity rather than silently no-op'ing.
	_, err = iamCli.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileOtherName), RoleName: aws.String("ghost-role"),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")
	_, err = iamCli.AddRoleToInstanceProfile(&iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("ghost-profile"), RoleName: aws.String(iamRoleAppName),
	})
	requireAWSErrorCode(t, err, "NoSuchEntity")

	// ListInstanceProfilesForRole — reverse lookup.
	rev, err := iamCli.ListInstanceProfilesForRole(&iam.ListInstanceProfilesForRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "list-instance-profiles-for-role")
	require.Len(t, rev.InstanceProfiles, 1, "reverse lookup should see only the profile we added")

	// DeleteRole guards: attached policy + still referenced by a profile.
	_, err = iamCli.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(iamRoleAppName)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	// DeleteInstanceProfile guard: role still attached.
	_, err = iamCli.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(iamProfileAppName)})
	requireAWSErrorCode(t, err, "DeleteConflict")

	// Full teardown: remove-role then delete-profile, detach-policy then
	// delete-role, in that order.
	_, err = iamCli.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String(iamProfileAppName), RoleName: aws.String(iamRoleAppName),
	})
	require.NoError(t, err, "remove-role-from-instance-profile")

	_, err = iamCli.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(iamProfileAppName)})
	require.NoError(t, err, "delete-instance-profile primary")

	_, err = iamCli.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String(iamProfileOtherName)})
	require.NoError(t, err, "delete-instance-profile other")

	_, err = iamCli.DetachRolePolicy(&iam.DetachRolePolicyInput{
		RoleName: aws.String(iamRoleAppName), PolicyArn: aws.String(adminPolicyARN),
	})
	require.NoError(t, err, "detach-role-policy")

	_, err = iamCli.DeleteRole(&iam.DeleteRoleInput{RoleName: aws.String(iamRoleAppName)})
	require.NoError(t, err, "delete-role")

	_, err = iamCli.GetRole(&iam.GetRoleInput{RoleName: aws.String(iamRoleAppName)})
	requireAWSErrorCode(t, err, "NoSuchEntity")
}
