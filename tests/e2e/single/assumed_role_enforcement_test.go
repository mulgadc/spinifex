//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Identifiers for the assumed-role control-plane enforcement suite. The are-
// prefix keeps them clear of the STS suite (sts-e2e-) and the IAM-roles suite
// (app-role / other-role).
const (
	areRoleName    = "are-e2e-role"
	arePolicyName  = "are-e2e-ec2-describe-regions"
	areSessionName = "are-e2e-session"

	// A single-action grant, so the positive case proves a specific managed
	// policy was resolved and evaluated — not a blanket allow.
	arePolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`
)

// runAssumedRoleControlPlaneEnforcement proves the gateway evaluates an
// assumed-role session's managed policies against the control plane — the
// symmetric twin of predastore's assumed-role S3 enforcement. It exercises the
// full SigV4-session → resolve-role-policies → evaluate path through the real
// gateway:
//
//   - a zero-policy role is denied (assumability does not imply permissions);
//   - after attaching a policy granting ec2:DescribeRegions, the SAME live
//     session is permitted — the gateway resolves the role's current managed
//     policies at request time, so already-minted credentials pick up the grant.
//
// Owns its own role + policy, so it is safe alongside the other IAM/STS suites,
// but registered sequential so its grant doesn't race a parallel evaluation.
func runAssumedRoleControlPlaneEnforcement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Assumed-role control-plane enforcement")
	adminAccount := iamEnsureAdminAccountID(t, fix)
	roleARN := iamRoleARN(adminAccount, areRoleName)
	policyARN := iamPolicyARN(adminAccount, arePolicyName)

	// Defensive pre-sweep + teardown — a previous failed run may have left
	// fragments. iamDeleteRoleAndProfilesBestEffort detaches the policy before
	// deleting the role; the explicit DeletePolicy then removes the managed
	// policy itself.
	sweep := func() {
		iamDeleteRoleAndProfilesBestEffort(fix, areRoleName, nil, policyARN)
		_, _ = fix.AWS.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyARN)})
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

	// Role assumable by anyone in-account (wildcard trust), with NO permission
	// policy yet.
	harness.Step(t, "create-role %q (no permission policy)", areRoleName)
	_, err := fix.AWS.IAM.CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String(areRoleName),
		AssumeRolePolicyDocument: aws.String(stsTrustPolicyAllowAny),
		Description:              aws.String("E2E assumed-role control-plane enforcement"),
	})
	require.NoError(t, err, "create-role")

	// Mint assumed-role (ASIA) credentials.
	harness.Step(t, "assume-role %q session=%q", roleARN, areSessionName)
	aOut, err := fix.AWS.STS.AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(areSessionName),
	})
	require.NoError(t, err, "assume-role")
	creds := aOut.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")
	sessionCli := harness.NewAWSClientWithSessionCreds(t, fix.Env,
		aws.StringValue(creds.AccessKeyId),
		aws.StringValue(creds.SecretAccessKey),
		aws.StringValue(creds.SessionToken))

	// Zero-policy role → implicit deny. The session authenticates (SigV4 ASIA
	// path) but the resolved role grants nothing.
	harness.Step(t, "describe-regions with zero-policy role (expect AccessDenied)")
	harness.ExpectError(t, "AccessDenied", func() error {
		_, e := sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
		return e
	})

	// Attach a managed policy granting ec2:DescribeRegions to the role.
	harness.Step(t, "create+attach policy %q (grants ec2:DescribeRegions)", arePolicyName)
	_, err = fix.AWS.IAM.CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String(arePolicyName),
		PolicyDocument: aws.String(arePolicyDoc),
	})
	require.NoError(t, err, "create-policy")
	_, err = fix.AWS.IAM.AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  aws.String(areRoleName),
		PolicyArn: aws.String(policyARN),
	})
	require.NoError(t, err, "attach-role-policy")

	// The SAME live session is now permitted: the gateway resolves the role's
	// current managed policies per request, so an already-minted session picks
	// up the new grant.
	harness.Step(t, "describe-regions after grant (expect success)")
	_, err = sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the role grants it")
}
