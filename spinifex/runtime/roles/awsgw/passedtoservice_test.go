package awsgw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ptsECSTargetARN = "arn:aws:iam::123456789012:role/ecs-task-role"

// ptsProfileIAM resolves every instance profile to one holding role app.
type ptsProfileIAM struct{}

func (ptsProfileIAM) CanonicalResourceARN(accountID string, _ arn.IAMResourceType, name string) (string, error) {
	return "arn:aws:iam::" + accountID + ":role/" + name, nil
}

func (ptsProfileIAM) ResolveInstanceProfile(accountID, _ string) (*iamdomain.InstanceProfile, error) {
	return &iamdomain.InstanceProfile{AccountID: accountID, RoleName: "app"}, nil
}

type ptsGateway struct {
	h   *gwtest.Harness
	nc  *nats.Conn
	eks eksawsapi.Deps
}

func passRoleToGateway(t *testing.T, statements ...iamdomain.Statement) *ptsGateway {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	return &ptsGateway{h: gwtest.Scoped(statements...), nc: nc}
}

func eksPassRoleGateway(t *testing.T, statements ...iamdomain.Statement) *ptsGateway {
	t.Helper()
	svc := &roleStoreIAMService{
		PolicyIAM: gwfix.PolicyIAM{},
		roles: map[string]map[string]string{
			gwtest.AccountID: {"node-role": eksNodeRoleARN},
			foreignAccountID: {"node-role": "arn:aws:iam::" + foreignAccountID + ":role/eks/node-role"},
		},
	}
	return &ptsGateway{
		h:   gwtest.Scoped(statements...),
		eks: eksawsapi.Deps{Roles: mockRoleResolver{iam: svc}},
	}
}

func (g *ptsGateway) dispatchEC2(t *testing.T, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return g.h.Dispatch(ec2awsapi.NewRegistration(ec2awsapi.Deps{NATS: g.nc, IAM: ptsProfileIAM{}, Quota: ec2quota.Adapter{},
		ActiveNodes: func(context.Context) int { return 0 }}),
		ec2awsapi.ServiceName, httptest.NewRecorder(), req)
}

func (g *ptsGateway) registerTaskDefinition(t *testing.T) error {
	t.Helper()
	body, err := json.Marshal(&ecs.RegisterTaskDefinitionInput{
		Family: aws.String("web"), TaskRoleArn: aws.String(ptsECSTargetARN),
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	req.Header.Set("X-Amz-Target", ecsawsapi.TargetPrefix+".RegisterTaskDefinition")
	return g.h.Dispatch(ecsawsapi.NewRegistration(ecsawsapi.Deps{NATS: g.nc}),
		ecsawsapi.ServiceName, httptest.NewRecorder(), req)
}

func (g *ptsGateway) dispatchEKS(t *testing.T, method, path, body string) error {
	t.Helper()
	d := g.eks
	d.NATS = g.nc
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return g.h.Dispatch(eksawsapi.NewRegistration(d), eksawsapi.ServiceName, httptest.NewRecorder(), req)
}

func (g *ptsGateway) associateProfile(t *testing.T) error {
	t.Helper()
	return g.dispatchEC2(t, "Action=AssociateIamInstanceProfile&InstanceId=i-dev&IamInstanceProfile.Name=web")
}

func (g *ptsGateway) createNodegroup(t *testing.T) error {
	t.Helper()
	return g.dispatchEKS(t, http.MethodPost, "/clusters/prod/node-groups",
		`{"nodegroupName":"workers","nodeRole":"`+eksNodeRoleARN+`"}`)
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
			assertNotUnauthorized(t, passRoleToGateway(t,
				gwtest.Statement("Allow", "ec2:*", "*"),
				gwtest.PassRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
			).dispatchEC2(t, tt.body))
			gwtest.AssertUnauthorized(t, passRoleToGateway(t,
				gwtest.Statement("Allow", "ec2:*", "*"),
				gwtest.PassRoleTo("Allow", iampolicy.OpStringEquals, "ecs-tasks.amazonaws.com"),
			).dispatchEC2(t, tt.body))
		})
	}
}

func TestPassedToService_ECS(t *testing.T) {
	gwtest.AssertNotDenied(t, passRoleToGateway(t,
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.PassRoleTo("Allow", iampolicy.OpStringLike, "ecs-tasks.*"),
	).registerTaskDefinition(t))
	gwtest.AssertDenied(t, passRoleToGateway(t,
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.PassRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
	).registerTaskDefinition(t))
}

// AWS reports a nodegroup's node role as passed to EKS, not to the EC2
// instances that end up running under it.
func TestPassedToService_EKS(t *testing.T) {
	allowed := eksPassRoleGateway(t,
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.PassRoleTo("Allow", iampolicy.OpStringEquals, "eks.amazonaws.com"),
	)
	gwtest.AssertPermitted(t, allowed.createNodegroup(t))
	gwtest.AssertPermitted(t, allowed.dispatchEKS(t, http.MethodPost, "/clusters",
		`{"name":"prod","roleArn":"`+eksNodeRoleARN+`"}`))

	denied := eksPassRoleGateway(t,
		gwtest.Statement("Allow", "eks:*", "*"),
		gwtest.PassRoleTo("Allow", iampolicy.OpStringEquals, "ec2.amazonaws.com"),
	)
	gwtest.AssertDenied(t, denied.createNodegroup(t))
	gwtest.AssertDenied(t, denied.dispatchEKS(t, http.MethodPost, "/clusters",
		`{"name":"prod","roleArn":"`+eksNodeRoleARN+`"}`))
}

// A Deny scoped to one service fences that service and leaves the others to
// the unconditional grant.
func TestPassedToService_DenyFencesOneService(t *testing.T) {
	statements := []iamdomain.Statement{
		gwtest.Statement("Allow", "ec2:*", "*"),
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Allow", "iam:PassRole", "*"),
		gwtest.PassRoleTo("Deny", iampolicy.OpStringEquals, "ecs-tasks.amazonaws.com"),
	}
	gwtest.AssertDenied(t, passRoleToGateway(t, statements...).registerTaskDefinition(t))
	assertNotUnauthorized(t, passRoleToGateway(t, statements...).associateProfile(t))
}

// The key exists only on a PassRole check, so a condition on it attached to
// any other action does not hold.
func TestPassedToService_AbsentOutsidePassRole(t *testing.T) {
	grant := gwtest.Statement("Allow", "ec2:*", "*")
	grant.Condition = map[string]map[string]iamdomain.ConditionValue{
		iampolicy.OpStringEquals: {iampolicy.KeyPassedToService: {"ec2.amazonaws.com"}},
	}
	h := gwtest.Scoped(grant)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=DeleteVolume"))
	gwtest.AssertUnauthorized(t, h.Dispatch(ec2awsapi.NewRegistration(ec2awsapi.Deps{}),
		ec2awsapi.ServiceName, httptest.NewRecorder(), req))
}
