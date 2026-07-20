//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// assumedRoleTrustPolicy lets any principal assume the role, so the test
	// isolates policy attachment as the sole variable under test.
	assumedRoleTrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`

	// A single-action grant, so the positive case proves a specific managed
	// policy was resolved and evaluated - not a blanket allow.
	assumedRoleDescribeRegionsPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:DescribeRegions","Resource":"*"}]}`
)

// requireAWSErrorCode fails t unless err is an awserr.Error carrying code.
// Shared by every allow/deny authz assertion in this tier.
func requireAWSErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var awsErr awserr.Error
	require.ErrorAs(t, err, &awsErr, "expected awserr.Error, got %T: %v", err, err)
	assert.Equal(t, code, awsErr.Code())
}

// TestAssumedRoleControlPlaneEnforcement proves the gateway evaluates
// assumed-role sessions against managed policies: a zero-policy role is
// denied a guarded action, and the same live session is allowed once a
// policy is attached to the role (policies resolved per request).
// ec2:DescribeRegions is dispatched entirely inside the gateway
// (gateway/ec2.go ec2LocalActions) so this exercises only gateway-side authz,
// with no NATS stub involved.
func TestAssumedRoleControlPlaneEnforcement(t *testing.T) {
	gw := StartGateway(t)

	roleOut, err := gw.IAMClient(t).CreateRole(&iam.CreateRoleInput{
		RoleName:                 aws.String("are-role"),
		AssumeRolePolicyDocument: aws.String(assumedRoleTrustPolicy),
	})
	require.NoError(t, err, "create-role")

	assumeOut, err := gw.STSClient(t).AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         roleOut.Role.Arn,
		RoleSessionName: aws.String("are-session"),
	})
	require.NoError(t, err, "assume-role")
	creds := assumeOut.Credentials
	require.NotNil(t, creds, "AssumeRole returned nil Credentials")

	sessionCli := gw.ClientsWithSessionCreds(t,
		aws.StringValue(creds.AccessKeyId),
		aws.StringValue(creds.SecretAccessKey),
		aws.StringValue(creds.SessionToken))

	// Zero-policy role: the session authenticates, then default-denies.
	_, err = sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	requireAWSErrorCode(t, err, "AccessDenied")

	policyOut, err := gw.IAMClient(t).CreatePolicy(&iam.CreatePolicyInput{
		PolicyName:     aws.String("are-ec2-describe-regions"),
		PolicyDocument: aws.String(assumedRoleDescribeRegionsPolicy),
	})
	require.NoError(t, err, "create-policy")

	_, err = gw.IAMClient(t).AttachRolePolicy(&iam.AttachRolePolicyInput{
		RoleName:  roleOut.Role.RoleName,
		PolicyArn: policyOut.Policy.Arn,
	})
	require.NoError(t, err, "attach-role-policy")

	// Same live session credentials are now allowed - the role grants it.
	_, err = sessionCli.EC2.DescribeRegions(&ec2.DescribeRegionsInput{})
	require.NoError(t, err, "describe-regions must be allowed once the role grants it")
}
