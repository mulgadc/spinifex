package awsapi_test

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// The PowerUserAccess shape: an Allow on everything except the listed actions.
func TestEC2Request_AllowNotActionGrantsTheComplement(t *testing.T) {
	h := gwtest.Scoped(iamdomain.Statement{
		Effect:    "Allow",
		NotAction: iamdomain.StringOrArr{"iam:*", "ec2:TerminateInstances"},
		Resource:  iamdomain.StringOrArr{"*"},
	})

	gwtest.AssertPermitted(t, dispatchEC2(t, h, "Action=StopInstances&InstanceId.1=i-dev"))
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=TerminateInstances&InstanceId.1=i-dev"))
}

// A Deny on NotResource fences every instance but the named one.
func TestEC2Request_DenyNotResourceFencesTheComplement(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ec2:*", "*"),
		iamdomain.Statement{
			Effect:      "Deny",
			Action:      iamdomain.StringOrArr{"ec2:TerminateInstances"},
			NotResource: iamdomain.StringOrArr{"arn:aws:ec2:*:*:instance/i-dev"},
		},
	)

	gwtest.AssertPermitted(t, dispatchEC2(t, h, "Action=TerminateInstances&InstanceId.1=i-dev"))
	gwtest.AssertUnauthorized(t, dispatchEC2(t, h, "Action=TerminateInstances&InstanceId.1=i-prod"))
}
