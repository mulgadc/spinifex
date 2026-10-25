//test:in-package — drives EC2_Request through the gateway's unexported test
// helpers (scopedPolicyGateway, dispatchEC2).

package gateway

import (
	"testing"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// The PowerUserAccess shape: an Allow on everything except the listed actions.
func TestEC2Request_AllowNotActionGrantsTheComplement(t *testing.T) {
	gw := scopedPolicyGateway(iamdomain.Statement{
		Effect:    "Allow",
		NotAction: iamdomain.StringOrArr{"iam:*", "ec2:TerminateInstances"},
		Resource:  iamdomain.StringOrArr{"*"},
	})

	assertPermitted(t, dispatchEC2(t, gw, "Action=StopInstances&InstanceId.1=i-dev"))
	assertUnauthorized(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-dev"))
}

// A Deny on NotResource fences every instance but the named one.
func TestEC2Request_DenyNotResourceFencesTheComplement(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		iamdomain.Statement{
			Effect:      "Deny",
			Action:      iamdomain.StringOrArr{"ec2:TerminateInstances"},
			NotResource: iamdomain.StringOrArr{"arn:aws:ec2:*:*:instance/i-dev"},
		},
	)

	assertPermitted(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-dev"))
	assertUnauthorized(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-prod"))
}
