package awsapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ecsPassRoleTargetARN = "arn:aws:iam::123456789012:role/ecs-task-role"

// dispatchECS drives the registration with no NATS connection. A permitted request
// therefore reaches the handler and fails there, which is what proves the policy
// check ran ahead of the resource existing at all.
func dispatchECS(t *testing.T, h *gwtest.Harness, action, body string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Amz-Target", ecsawsapi.TargetPrefix+"."+action)
	return h.Dispatch(ecsawsapi.NewRegistration(ecsawsapi.Deps{}), ecsawsapi.ServiceName, httptest.NewRecorder(), req)
}

func ecsResourceARN(resource string) string {
	return "arn:aws:ecs:" + gwtest.Region + ":" + gwtest.AccountID + ":" + resource
}

// TestECSRequest_ScopedDenyFires is the bypass this work closes. An operator
// fences a production service; before the resolver the fence was inert and
// DeleteService against it was permitted with nothing logged.
func TestECSRequest_ScopedDenyFires(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Deny", "ecs:DeleteService", "arn:aws:ecs:*:*:service/prod/web"),
	)

	gwtest.AssertDenied(t, dispatchECS(t, h, "DeleteService", `{"cluster":"prod","service":"web"}`))
	require.Error(t, dispatchECS(t, h, "DeleteService", `{"cluster":"prod","service":"api"}`))
	h.AssertGatePassed(t)
	require.Error(t, dispatchECS(t, h, "DeleteService", `{"cluster":"dev","service":"web"}`))
	h.AssertGatePassed(t)
}

// TestECSRequest_ScopedAllowGrants is the other half: a least-privilege policy
// used to deny everything, so the only working policy shape was Resource "*".
func TestECSRequest_ScopedAllowGrants(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:UpdateCluster", "arn:aws:ecs:*:*:cluster/dev"),
	)

	err := dispatchECS(t, h, "UpdateCluster", `{"cluster":"dev"}`)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
	gwtest.AssertDenied(t, dispatchECS(t, h, "UpdateCluster", `{"cluster":"prod"}`))
}

// An omitted cluster is the default cluster, both at the gate and in the
// handler, so a fence on it fires.
func TestECSRequest_OmittedClusterIsFencedAsDefault(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Deny", "ecs:UpdateCluster", ecsResourceARN("cluster/default")),
	)

	gwtest.AssertDenied(t, dispatchECS(t, h, "UpdateCluster", `{}`))
	gwtest.AssertDenied(t, dispatchECS(t, h, "UpdateCluster", `{"cluster":"default"}`))
	require.Error(t, dispatchECS(t, h, "UpdateCluster", `{"cluster":"prod"}`))
	h.AssertGatePassed(t)
}

// A body the gate cannot parse authorizes account-wide, and the handler still
// reports its own validation fault.
func TestECSRequest_UnparseableBodyStaysTheHandlersFault(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Deny", "ecs:UpdateCluster", ecsResourceARN("cluster/prod")),
	)

	err := dispatchECS(t, h, "UpdateCluster", "{not json")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// A body spelling one field two ways is the bypass a case-folded lookup opens:
// the handler reads the last spelling in document order, the gate's map cannot,
// so the request is rejected rather than authorized against the wrong cluster.
func TestECSRequest_FieldSpelledTwoWaysIsRejected(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Deny", "ecs:DeleteCluster", ecsResourceARN("cluster/prod")),
	)

	// Repeated: the fold's disagreement is random, the rejection must not be.
	for range 50 {
		err := dispatchECS(t, h, "DeleteCluster", `{"cluster":"dev","Cluster":"prod"}`)
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	}
}

// A describe naming no cluster describes the default one in the handler, so a
// fence on the default cluster fires against an empty list too.
func TestECSRequest_OmittedClusterListIsFencedAsDefault(t *testing.T) {
	h := gwtest.Scoped(
		gwtest.Statement("Allow", "ecs:*", "*"),
		gwtest.Statement("Deny", "ecs:DescribeClusters", ecsResourceARN("cluster/default")),
	)

	gwtest.AssertDenied(t, dispatchECS(t, h, "DescribeClusters", `{}`))
	gwtest.AssertDenied(t, dispatchECS(t, h, "DescribeClusters", `{"clusters":[]}`))
	require.Error(t, dispatchECS(t, h, "DescribeClusters", `{"clusters":["prod"]}`))
	h.AssertGatePassed(t)
}

// A body past the signed path's cap cannot be used to bypass the gate or to
// exhaust memory: it is rejected before either the gate or the handler.
func TestECSRequest_OversizedBodyIsRejected(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "ecs:*", "*"))
	body := `{"cluster":"` + strings.Repeat("a", sigv4.MaxPayloadLen) + `"}`

	err := dispatchECS(t, h, "UpdateCluster", body)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorRequestEntityTooLarge, err.Error())
}

// The property the per-service rollout rests on: passing a real ARN where "*"
// was passed before cannot withdraw access a working policy already grants.
func TestECSRequest_AccountWideGrantStillPermitsEveryAction(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "ecs:*", "*"))
	body := `{"cluster":"prod","service":"web","serviceName":"web","task":"t-1",` +
		`"containerInstance":"ci-1","taskDefinition":"app:1","name":"cp","capacityProvider":"cp",` +
		`"clusterName":"prod","resourceArn":"` + ecsResourceARN("cluster/prod") + `"}`

	for _, action := range ecsawsapi.ScopedActions() {
		t.Run(action, func(t *testing.T) {
			require.Error(t, dispatchECS(t, h, action, body))
			h.AssertGatePassed(t)
		})
	}
}

// The account-ID read moved above the gate, which used to reject a missing
// account itself. It is a server fault, so ECS answers ServerException.
func TestECSRequest_MissingAccountIDReturnsServerException(t *testing.T) {
	h := gwtest.Scoped(gwtest.Statement("Allow", "ecs:*", "*"))
	h.AccountID = ""

	err := dispatchECS(t, h, "ListClusters", "{}")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorECSServerException, err.Error())
}

func registerTaskDefinitionBody(t *testing.T, taskRoleARN string) string {
	t.Helper()
	input := &ecs.RegisterTaskDefinitionInput{Family: aws.String("web")}
	if taskRoleARN != "" {
		input.TaskRoleArn = aws.String(taskRoleARN)
	}
	body, err := json.Marshal(input)
	require.NoError(t, err)
	return string(body)
}

// allowRegisterTaskDefAndPassRole grants ecs:RegisterTaskDefinition
// unconditionally (the action-level check always runs first) plus iam:PassRole
// scoped to passRoleResource, so tests can isolate the resource-level PassRole
// decision from the action-level one.
func allowRegisterTaskDefAndPassRole(passRoleResource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"ecs:RegisterTaskDefinition"}, Resource: iamdomain.StringOrArr{"*"}},
			{Effect: "Allow", Action: iamdomain.StringOrArr{"iam:PassRole"}, Resource: iamdomain.StringOrArr{passRoleResource}},
		},
	}}
}

func TestECSRegisterTaskDefinition_DeniedWithoutPassRole(t *testing.T) {
	// ecs:RegisterTaskDefinition is granted; iam:PassRole is scoped to a
	// different role, so the target ARN is specifically denied.
	h := gwtest.FromDocs(allowRegisterTaskDefAndPassRole("arn:aws:iam::123456789012:role/some-other-role"))

	err := dispatchECS(t, h, "RegisterTaskDefinition", registerTaskDefinitionBody(t, ecsPassRoleTargetARN))
	require.Error(t, err)
	gwtest.AssertDenied(t, err)
}

func TestECSRegisterTaskDefinition_AllowedWithPassRole(t *testing.T) {
	h := gwtest.FromDocs(allowRegisterTaskDefAndPassRole(ecsPassRoleTargetARN))

	err := dispatchECS(t, h, "RegisterTaskDefinition", registerTaskDefinitionBody(t, ecsPassRoleTargetARN))
	// PassRole is granted, so the request proceeds past the check to the (nil,
	// disconnected) NATS dispatch; whatever fails downstream, it must not be
	// the PassRole denial.
	require.Error(t, err)
	h.AssertGatePassed(t)
}

func TestECSRegisterTaskDefinition_NoRoleSkipsPassRole(t *testing.T) {
	// No iam:PassRole grant at all — if the gateway still consulted PassRole
	// for a roleless task definition this would deny it.
	h := gwtest.Scoped(gwtest.Statement("Allow", "ecs:RegisterTaskDefinition", "*"))

	err := dispatchECS(t, h, "RegisterTaskDefinition", registerTaskDefinitionBody(t, ""))
	require.Error(t, err) // still fails downstream (nil NATS conn), just not on PassRole
	h.AssertGatePassed(t)
}

// Current behaviour, not a statement of what AWS does: ECS resolves nothing, so
// a role ARN that is malformed, in another account or absent from the store
// reaches PassRole as written and is accepted when the grant covers it.
func TestECSRegisterTaskDefinition_UnresolvedRoleARNsAreNotRejected(t *testing.T) {
	for name, roleARN := range map[string]string{
		"malformed":   "not-an-arn",
		"nonexistent": "arn:aws:iam::123456789012:role/no-such-role",
	} {
		t.Run(name, func(t *testing.T) {
			h := gwtest.FromDocs(allowRegisterTaskDefAndPassRole("*"))

			err := dispatchECS(t, h, "RegisterTaskDefinition", registerTaskDefinitionBody(t, roleARN))
			require.Error(t, err) // fails downstream on the nil NATS conn
			h.AssertGatePassed(t)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.NotEqual(t, awserrors.ErrorInvalidParameterValue, code)
		})
	}
}
