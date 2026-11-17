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
	assertDryRun(t, dispatchEC2(t, h, "Action=EnableImageDeregistrationProtection&ImageId=ami-0123456789abcdef0&DryRun=true"))
	assertDryRun(t, dispatchEC2(t, h, "Action=DisableImageDeregistrationProtection&ImageId=ami-0123456789abcdef0&DryRun=true"))
}

func TestEC2Request_DryRunFalseDispatches(t *testing.T) {
	h := gwtest.AllowAll()
	gwtest.AssertPermitted(t, dispatchEC2(t, h, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=false"))
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
