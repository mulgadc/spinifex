package gateway_ecs_test

import (
	"testing"

	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func ecsARN(resource string) string {
	return "arn:aws:ecs:" + testRegion + ":" + testAccountID + ":" + resource
}

func resolve(t *testing.T, action, body string) []string {
	t.Helper()
	resources, err := gateway_ecs.ResourceARNs(action, testRegion, testAccountID, []byte(body))
	require.NoError(t, err)
	return resources
}

// TestScopeTableIsExhaustive is what stops the next ECS action being added with
// a silent account-wide grant. Both directions, so a scope left behind by a
// deleted or renamed action fails too.
func TestScopeTableIsExhaustive(t *testing.T) {
	for action := range gateway_ecs.Actions {
		assert.True(t, gateway_ecs.HasScope(action),
			"ecs action %q has no resource scope entry: add one to ecsScopes in gateway/ecs/authz.go", action)
	}
	for _, action := range gateway_ecs.ScopedActions() {
		_, served := gateway_ecs.Actions[action]
		assert.True(t, served,
			"ecsScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/ecs/authz.go", action)
	}
}

// The detail that decides whether a fence on the default cluster fires at all:
// the handler resolves an omitted cluster to "default", so the gate must too.
func TestResourceARNs_OmittedClusterResolvesToDefault(t *testing.T) {
	assert.Equal(t, []string{ecsARN("cluster/default")}, resolve(t, "DeleteCluster", `{}`))
	assert.Equal(t, []string{ecsARN("cluster/default")}, resolve(t, "CreateCluster", `{}`))
	assert.Equal(t, []string{ecsARN("service/default/web")},
		resolve(t, "DeleteService", `{"service":"web"}`))
}

// A cluster ARN and a bare name name the same cluster, because the handler
// normalizes both the same way.
func TestResourceARNs_ClusterARNAndNameResolveIdentically(t *testing.T) {
	byName := resolve(t, "DeleteCluster", `{"cluster":"prod"}`)
	byARN := resolve(t, "DeleteCluster", `{"cluster":"`+ecsARN("cluster/prod")+`"}`)
	assert.Equal(t, byName, byARN)
	assert.Equal(t, []string{ecsARN("cluster/prod")}, byName)
}

func TestResourceARNs_ShortIDsAreExtractedFromARNs(t *testing.T) {
	assert.Equal(t, []string{ecsARN("task/prod/t-1")},
		resolve(t, "StopTask", `{"cluster":"prod","task":"`+ecsARN("task/prod/t-1")+`"}`))
	assert.Equal(t, []string{ecsARN("container-instance/prod/ci-1")},
		resolve(t, "DeregisterContainerInstance",
			`{"cluster":"prod","containerInstance":"`+ecsARN("container-instance/prod/ci-1")+`"}`))
	assert.Equal(t, []string{ecsARN("capacity-provider/cp")},
		resolve(t, "DeleteCapacityProvider", `{"capacityProvider":"`+ecsARN("capacity-provider/cp")+`"}`))
}

// A reference without a revision means latest, which the gate cannot resolve.
// The trailing "*" is the AWS-documented spelling, and is a value, not a widening.
func TestResourceARNs_TaskDefinitionWithoutRevisionWildcardsIt(t *testing.T) {
	assert.Equal(t, []string{ecsARN("task-definition/app:*")},
		resolve(t, "RunTask", `{"taskDefinition":"app"}`))
	assert.Equal(t, []string{ecsARN("task-definition/app:3")},
		resolve(t, "RunTask", `{"taskDefinition":"app:3"}`))
	assert.Equal(t, []string{ecsARN("task-definition/app:3")},
		resolve(t, "StartTask", `{"taskDefinition":"`+ecsARN("task-definition/app:3")+`"}`))
}

// A list action names every resource it asks about, and each is evaluated.
func TestResourceARNs_ListFieldsResolveEveryMember(t *testing.T) {
	assert.Equal(t, []string{ecsARN("cluster/prod"), ecsARN("cluster/dev")},
		resolve(t, "DescribeClusters", `{"clusters":["prod","dev"]}`))
	assert.Equal(t, []string{ecsARN("service/prod/web"), ecsARN("service/prod/api")},
		resolve(t, "DescribeServices", `{"cluster":"prod","services":["web","api"]}`))
}

// A tag request names its resource by ARN. The handler ignores that ARN's
// region and account and works in the caller's, so the gate must too.
func TestResourceARNs_TagARNIsReanchored(t *testing.T) {
	foreign := `{"resourceArn":"arn:aws:ecs:us-east-1:999999999999:cluster/prod"}`
	assert.Equal(t, []string{ecsARN("cluster/prod")}, resolve(t, "TagResource", foreign))
	assert.Equal(t, []string{ecsARN("service/prod/web")},
		resolve(t, "ListTagsForResource", `{"resourceArn":"`+ecsARN("service/prod/web")+`"}`))
	// An ARN the tag handler rejects stays its own validation fault.
	assert.Equal(t, []string{"*"}, resolve(t, "UntagResource", `{"resourceArn":"arn:aws:ecs:::not-a-resource"}`))
}

// A body the gate cannot parse authorizes account-wide: it is the handler that
// rejects a malformed request.
func TestResourceARNs_UnparseableBodyAuthorizesAccountWide(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolve(t, "DescribeServices", "{not json"))
	assert.Equal(t, []string{"*"}, resolve(t, "DescribeTasks", `{"cluster":"prod"}`))
}

func TestResourceARNs_AccountWideActionsStayAccountWide(t *testing.T) {
	for _, action := range []string{
		"ListClusters", "ListServices", "ListTasks", "ListTaskDefinitions",
		"ListTaskDefinitionFamilies", "ListContainerInstances", "ListServicesByNamespace",
		"RegisterTaskDefinition", "DescribeTaskDefinition", "RegisterContainerInstance",
		"SubmitTaskStateChange", "PutAccountSetting", "ListAccountSettings",
	} {
		assert.Equal(t, []string{"*"}, resolve(t, action, `{"cluster":"prod"}`), "action %q", action)
	}
}

// An action that is served but has no entry fails closed rather than defaulting
// to an account-wide grant.
func TestResourceARNs_UnknownActionIsRejected(t *testing.T) {
	_, err := gateway_ecs.ResourceARNs("MadeUpAction", testRegion, testAccountID, nil)
	require.Error(t, err)
}
