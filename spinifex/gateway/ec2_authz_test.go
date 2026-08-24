//test:in-package — drives EC2_Request through the gateway's unexported test
// helpers (setupEC2Request, policyMockIAMService) and auth context keys.

package gateway

import (
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	authzAccountID = "123456789012"
	authzRegion    = "ap-southeast-2"
)

// scopedPolicyGateway serves one policy document to every principal.
func scopedPolicyGateway(statements ...handlers_iam.Statement) *GatewayConfig {
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	return &GatewayConfig{
		DisableLogging: true,
		Region:         authzRegion,
		IAMService: &policyMockIAMService{
			getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
			getRolePoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		},
	}
}

func statement(effect, action, resource string) handlers_iam.Statement {
	return handlers_iam.Statement{
		Effect:   effect,
		Action:   handlers_iam.StringOrArr{action},
		Resource: handlers_iam.StringOrArr{resource},
	}
}

// dispatchEC2 drives the gateway with no NATS connection. A permitted request
// therefore reaches the handler and fails there, which is what proves the policy
// check ran ahead of the resource existing at all.
func dispatchEC2(t *testing.T, gw *GatewayConfig, body string) error {
	t.Helper()
	return gw.EC2_Request(httptest.NewRecorder(), setupEC2Request(body, authzAccountID))
}

func assertDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// assertPermitted asserts the policy gate passed. The request then fails on the
// absent NATS connection rather than on authorization, so a denial cannot hide
// behind a not-found.
func assertPermitted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// TestEC2Request_ScopedDenyFires is the bypass this work closes. An operator
// fences a production instance; before the resolver the fence was inert and
// TerminateInstances against it was permitted with nothing logged.
func TestEC2Request_ScopedDenyFires(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:TerminateInstances", "arn:aws:ec2:*:*:instance/i-prod"),
	)

	assertDenied(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-prod"))
	assertPermitted(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-dev"))
}

// TestEC2Request_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestEC2Request_ScopedAllowGrants(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:TerminateInstances", "arn:aws:ec2:*:*:instance/i-dev"),
	)

	assertPermitted(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-dev"))
	assertDenied(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-prod"))
}

// TestEC2Request_DenyOneMemberFailsTheBatch pins the AWS combining rule: a batch
// containing one fenced instance fails whole rather than terminating the rest.
func TestEC2Request_DenyOneMemberFailsTheBatch(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:TerminateInstances", "arn:aws:ec2:*:*:instance/i-prod"),
	)

	assertDenied(t, dispatchEC2(t, gw,
		"Action=TerminateInstances&InstanceId.1=i-dev&InstanceId.2=i-prod&InstanceId.3=i-other"))
	assertPermitted(t, dispatchEC2(t, gw,
		"Action=TerminateInstances&InstanceId.1=i-dev&InstanceId.2=i-other"))
}

// TestEC2Request_MultiResourceParity covers a resource beyond the primary one.
// A tenant constraining launches by instance would otherwise have the AMI and
// subnet unchecked, which is a smaller instance of the same bug.
func TestEC2Request_MultiResourceParity(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:RunInstances", "arn:aws:ec2:*:*:image/ami-restricted"),
	)

	assertDenied(t, dispatchEC2(t, gw, "Action=RunInstances&ImageId=ami-restricted&MinCount=1&MaxCount=1"))
	assertPermitted(t, dispatchEC2(t, gw, "Action=RunInstances&ImageId=ami-allowed&MinCount=1&MaxCount=1"))
}

// TestEC2Request_AccountWideAllowIsUnaffected is the property the remaining
// per-service beads rest on: passing a real ARN where "*" was passed cannot
// withdraw access that works today.
func TestEC2Request_AccountWideAllowIsUnaffected(t *testing.T) {
	for _, policy := range []handlers_iam.Statement{
		statement("Allow", "ec2:*", "*"),
		statement("Allow", "*", "*"),
	} {
		gw := scopedPolicyGateway(policy)
		for _, body := range []string{
			"Action=TerminateInstances&InstanceId.1=i-abc",
			"Action=RunInstances&ImageId=ami-1&SubnetId=subnet-1&KeyName=k1",
			"Action=CreateTags&ResourceId.1=i-abc&Tag.1.Key=Name&Tag.1.Value=x",
			"Action=CreateTags&ResourceId.1=nope-abc&Tag.1.Key=Name&Tag.1.Value=x",
			"Action=DeleteVolume&VolumeId=vol-1",
			"Action=DescribeInstances",
		} {
			t.Run(body, func(t *testing.T) {
				assertPermitted(t, dispatchEC2(t, gw, body))
			})
		}
	}
}

// TestEC2Request_MissingIdentifierStaysAValidationFault: the gate must not turn
// a malformed request into a denial. With an account-wide grant the absent
// identifier resolves to "*" and the handler reports its own fault.
func TestEC2Request_MissingIdentifierStaysAValidationFault(t *testing.T) {
	gw := scopedPolicyGateway(statement("Allow", "ec2:*", "*"))

	assertPermitted(t, dispatchEC2(t, gw, "Action=TerminateInstances"))
	assertPermitted(t, dispatchEC2(t, gw, "Action=DeleteVolume"))
}

// TestEC2Request_IdentifierIsAValueNotAPattern: an id of literally * builds an
// ARN ending /*, which neither matches a scoped Deny on a real instance nor
// widens a grant to one.
func TestEC2Request_IdentifierIsAValueNotAPattern(t *testing.T) {
	fenced := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:TerminateInstances", "arn:aws:ec2:*:*:instance/i-prod"),
	)
	assertPermitted(t, dispatchEC2(t, fenced, "Action=TerminateInstances&InstanceId.1=*"))

	// An id containing / is not truncated on the way to the evaluator. The
	// reverted STS work built an ARN from the segment after the final /, so a
	// Deny on i-prod fenced i-prod/admin and a grant fenced the wrong object.
	assertPermitted(t, dispatchEC2(t, fenced, "Action=TerminateInstances&InstanceId.1=i-prod/admin"))
	assertDenied(t, dispatchEC2(t, fenced, "Action=TerminateInstances&InstanceId.1=i-prod"))
}

// TestEC2Request_ARNIsBuiltFromTheNodeRegion: the credential-scope region is
// caller-supplied and never validated, so an ARN built from it would let a
// caller sign for another region and slide out from under a Deny scoped to the
// real one.
func TestEC2Request_ARNIsBuiltFromTheNodeRegion(t *testing.T) {
	gw := scopedPolicyGateway(
		statement("Allow", "ec2:*", "*"),
		statement("Deny", "ec2:TerminateInstances", "arn:aws:ec2:"+authzRegion+":*:instance/i-prod"),
	)

	assertDenied(t, dispatchEC2(t, gw, "Action=TerminateInstances&InstanceId.1=i-prod"))
}
