//go:build e2e

package iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	areRoleName    = "are-e2e-role"
	arePolicyName  = "are-e2e-ec2-describe-regions"
	areSessionName = "are-e2e-session"

	// A single-action grant, so the positive case proves a specific managed
	// policy was resolved and evaluated — not a blanket allow.
	arePolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`
)

// runAssumedRoleControlPlaneEnforcement proves the gateway evaluates assumed-role
// sessions against managed policies: a zero-policy role is denied, and the same
// live session is allowed once a policy is attached (policies resolved per request).
func runAssumedRoleControlPlaneEnforcement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Assumed-role control-plane enforcement")
	adminAccount := harness.IAMAccountID(t, fix.AWS)
	roleARN := harness.IAMRoleARN(adminAccount, areRoleName)
	policyARN := harness.IAMPolicyARN(adminAccount, arePolicyName)

	// Pre-sweep in case a previous run left fragments.
	sweep := func() {
		harness.IAMDeleteRoleAndProfilesBestEffort(fix.AWS, areRoleName, nil, policyARN)
		_, _ = fix.AWS.IAM.DeletePolicy(&iam.DeletePolicyInput{PolicyArn: aws.String(policyARN)})
	}
	sweep()
	fix.Harness.RegisterCleanup(sweep)

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

	harness.Step(t, "describe-regions after grant (expect success)")
	_, err = sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the role grants it")
}
