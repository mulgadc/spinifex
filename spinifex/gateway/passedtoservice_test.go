package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passRoleTo is a PassRole statement on every role, conditioned on the
// consuming service the way AWS's documented examples scope it.
func passRoleTo(effect, operator, service string) handlers_iam.Statement {
	s := statement(effect, "iam:PassRole", "*")
	s.Condition = map[string]map[string]handlers_iam.ConditionValue{
		operator: {iampolicy.KeyPassedToService: {service}},
	}
	return s
}

func passRoleToGateway(t *testing.T, statements ...handlers_iam.Statement) *GatewayConfig {
	t.Helper()
	docs := []handlers_iam.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	return &GatewayConfig{
		DisableLogging: true,
		Region:         authzRegion,
		NATSConn:       startTestNATS(t),
		IAMService: &profileIAMService{
			policyMockIAMService: policyMockIAMService{
				getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
			},
		},
	}
}

func associateProfile(t *testing.T, gw *GatewayConfig) error {
	t.Helper()
	return dispatchEC2(t, gw, "Action=AssociateIamInstanceProfile&InstanceId=i-dev&IamInstanceProfile.Name=web")
}

func registerTaskDefinition(t *testing.T, gw *GatewayConfig) error {
	t.Helper()
	req := setupECSUserRequest("AmazonEC2ContainerServiceV20141113.RegisterTaskDefinition",
		registerTaskDefinitionBody(t, testECSPassRoleTargetARN))
	return gw.ECS_Request(httptest.NewRecorder(), req)
}

func createNodegroup(t *testing.T, gw *GatewayConfig) error {
	t.Helper()
	return dispatchEKS(t, gw, http.MethodPost, "/clusters/prod/node-groups", createNodegroupBody(eksNodeRoleARN))
}

func assertNotUnauthorized(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorUnauthorizedOperation, err.Error())
}

// Each EC2 action that hands over an instance profile builds its own PassRole
// check, so every one is pinned to ec2.amazonaws.com.
func TestPassedToService_EC2(t *testing.T) {
	tests := []struct {
		action string
		body   string
	}{
		{"RunInstances", "Action=RunInstances&ImageId=ami-1&InstanceType=t3.micro&MinCount=1&MaxCount=1&IamInstanceProfile.Name=web"},
		{"AssociateIamInstanceProfile", "Action=AssociateIamInstanceProfile&InstanceId=i-dev&IamInstanceProfile.Name=web"},
		{"ReplaceIamInstanceProfileAssociation", "Action=ReplaceIamInstanceProfileAssociation" +
			"&AssociationId=iip-assoc-dev&IamInstanceProfile.Name=web"},
		{"RequestSpotInstances", "Action=RequestSpotInstances&LaunchSpecification.ImageId=ami-1" +
			"&LaunchSpecification.InstanceType=t3.micro&LaunchSpecification.IamInstanceProfile.Name=web"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			assertNotUnauthorized(t, dispatchEC2(t, passRoleToGateway(t,
				statement("Allow", "ec2:*", "*"),
				passRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
			), tt.body))
			assertUnauthorized(t, dispatchEC2(t, passRoleToGateway(t,
				statement("Allow", "ec2:*", "*"),
				passRoleTo("Allow", iampolicy.OpStringEquals, "ecs-tasks.amazonaws.com"),
			), tt.body))
		})
	}
}

func TestPassedToService_ECS(t *testing.T) {
	assertNotDenied(t, registerTaskDefinition(t, passRoleToGateway(t,
		statement("Allow", "ecs:*", "*"),
		passRoleTo("Allow", iampolicy.OpStringLike, "ecs-tasks.*"),
	)))
	assertDenied(t, registerTaskDefinition(t, passRoleToGateway(t,
		statement("Allow", "ecs:*", "*"),
		passRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
	)))
}

// AWS reports a nodegroup's node role as passed to EKS, not to the EC2
// instances that end up running under it.
func TestPassedToService_EKS(t *testing.T) {
	allowed := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		passRoleTo("Allow", iampolicy.OpStringEquals, "eks.amazonaws.com"),
	)
	assertPermitted(t, createNodegroup(t, allowed))
	assertPermitted(t, dispatchEKS(t, allowed, http.MethodPost, "/clusters",
		`{"name":"prod","roleArn":"`+eksNodeRoleARN+`"}`))

	denied := eksPassRoleGateway(
		statement("Allow", "eks:*", "*"),
		passRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
	)
	assertDenied(t, createNodegroup(t, denied))
	assertDenied(t, dispatchEKS(t, denied, http.MethodPost, "/clusters",
		`{"name":"prod","roleArn":"`+eksNodeRoleARN+`"}`))
}

// A Deny scoped to one service fences that service and leaves the others to
// the unconditional grant.
func TestPassedToService_DenyFencesOneService(t *testing.T) {
	statements := []handlers_iam.Statement{
		statement("Allow", "ec2:*", "*"),
		statement("Allow", "ecs:*", "*"),
		statement("Allow", "iam:PassRole", "*"),
		passRoleTo("Deny", iampolicy.OpStringEquals, "ecs-tasks.amazonaws.com"),
	}
	assertDenied(t, registerTaskDefinition(t, passRoleToGateway(t, statements...)))
	assertNotUnauthorized(t, associateProfile(t, passRoleToGateway(t, statements...)))
}

// The key exists only on a PassRole check, so a condition on it attached to
// any other action does not hold.
func TestPassedToService_AbsentOutsidePassRole(t *testing.T) {
	grant := statement("Allow", "ec2:*", "*")
	grant.Condition = map[string]map[string]handlers_iam.ConditionValue{
		iampolicy.OpStringEquals: {iampolicy.KeyPassedToService: {"ec2.amazonaws.com"}},
	}
	assertUnauthorized(t, dispatchEC2(t, scopedPolicyGateway(grant), "Action=DeleteVolume"))
}
