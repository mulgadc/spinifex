package awsapi_test

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertDryRun(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorDryRunOperation, err.Error())
}

// A caller the policy refuses is denied rather than told the request would
// have succeeded.
func TestEC2Request_DryRunDeniedCaller(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ec2:*", "*"),
		gwtest.Statement("Deny", "ec2:CreateVpc", "*"),
	)
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=CreateSecurityGroup&GroupName=g&GroupDescription=d&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=ResetNetworkInterfaceAttribute&NetworkInterfaceId=eni-0f3a9c2b7d1e4a5b6&SourceDestCheck=&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=GetDefaultCreditSpecification&InstanceFamily=t3&DryRun=true"))
}

func TestEC2Request_DryRunFalseDispatches(t *testing.T) {
	h := gwtest.AllowAll()
	gwtest.AssertPermitted(t, dispatchEC2(t, h, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=false"))
}

// The ID-format calls answer without NATS, and the two whose input models
// DryRun answer DryRunOperation.
func TestEC2Request_IdFormatCalls(t *testing.T) {
	h := gwtest.AllowAll()
	require.NoError(t, dispatchEC2(t, h, "Action=DescribeIdFormat"))
	require.NoError(t, dispatchEC2(t, h, "Action=DescribeIdentityIdFormat&PrincipalArn=arn:aws:iam::"+gwtest.AccountID+":root"))
	require.NoError(t, dispatchEC2(t, h, "Action=DescribeAggregateIdFormat"))
	require.NoError(t, dispatchEC2(t, h, "Action=DescribePrincipalIdFormat"))
	assertDryRun(t, dispatchEC2(t, h, "Action=DescribeAggregateIdFormat&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=DescribePrincipalIdFormat&DryRun=true"))
}

// ResetInstanceAttribute is authorised on the instance ARN, and a permitted
// DryRun answers DryRunOperation even for an instance that does not exist.
func TestEC2Request_ResetInstanceAttributeAuthzAndDryRun(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ec2:*", "*"),
		gwtest.Statement("Deny", "ec2:ResetInstanceAttribute", "arn:aws:ec2:*:*:instance/i-0f3a9c2b"),
	)
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=ResetInstanceAttribute&InstanceId=i-0f3a9c2b&Attribute=kernel&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=ResetInstanceAttribute&InstanceId=i-0f3a9c2b7d1e4a5b6&Attribute=kernel&DryRun=true"))
}

// Enable/DisableImageDeprecation are authorised on the image ARN, and a
// permitted DryRun answers DryRunOperation even for an image that does not exist.
func TestEC2Request_ImageDeprecationAuthzAndDryRun(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ec2:*", "*"),
		gwtest.Statement("Deny", "ec2:EnableImageDeprecation", "arn:aws:ec2:*:*:image/ami-0f3a9c2b"),
		gwtest.Statement("Deny", "ec2:DisableImageDeprecation", "arn:aws:ec2:*:*:image/ami-0f3a9c2b"),
	)
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=EnableImageDeprecation&ImageId=ami-0f3a9c2b&DeprecateAt=2030-01-01T00:00:00Z&DryRun=true"))
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=DisableImageDeprecation&ImageId=ami-0f3a9c2b&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=EnableImageDeprecation&ImageId=ami-0f3a9c2b7d1e4a5b6&DeprecateAt=2030-01-01T00:00:00Z&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=DisableImageDeprecation&ImageId=ami-0f3a9c2b7d1e4a5b6&DryRun=true"))
}
