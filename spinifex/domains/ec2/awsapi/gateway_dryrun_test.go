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
}

func TestEC2Request_DryRunFalseDispatches(t *testing.T) {
	h := gwtest.AllowAll()
	gwtest.AssertPermitted(t, dispatchEC2(t, h, "Action=CreateVpc&CidrBlock=10.0.0.0/16&DryRun=false"))
}
