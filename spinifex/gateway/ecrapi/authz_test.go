package gateway_ecrapi_test

import (
	"testing"

	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func ecrARN(resource string) string {
	return "arn:aws:ecr:" + testRegion + ":" + testAccountID + ":" + resource
}

func resolve(t *testing.T, action, body string) []string {
	t.Helper()
	resources, err := gateway_ecrapi.ResourceARNs(action, testRegion, testAccountID, []byte(body))
	require.NoError(t, err)
	return resources
}

// TestScopeTableIsExhaustive is what stops the next ECR action being added with
// a silent account-wide grant. Both directions, so a scope left behind by a
// deleted or renamed action fails too.
func TestScopeTableIsExhaustive(t *testing.T) {
	for action := range gateway_ecrapi.Actions {
		assert.True(t, gateway_ecrapi.HasScope(action),
			"ecr action %q has no resource scope entry: add one to ecrScopes in gateway/ecrapi/authz.go", action)
	}
	for _, action := range gateway_ecrapi.ScopedActions() {
		_, served := gateway_ecrapi.Actions[action]
		assert.True(t, served,
			"ecrScopes has an entry for %q, which the dispatch table does not serve: remove it from gateway/ecrapi/authz.go", action)
	}
}

func TestResourceARNs_RepositoryIsResolvedFromTheBody(t *testing.T) {
	assert.Equal(t, []string{ecrARN("repository/app")},
		resolve(t, "DeleteRepository", `{"repositoryName":"app"}`))
	assert.Equal(t, []string{ecrARN("repository/app"), ecrARN("repository/api")},
		resolve(t, "DescribeRepositories", `{"repositoryNames":["app","api"]}`))
}

// A caller-supplied account would let a request slide out from under a Deny
// scoped to the real one, so registryId is ignored.
func TestResourceARNs_RegistryIDInTheBodyIsIgnored(t *testing.T) {
	assert.Equal(t, []string{ecrARN("repository/app")},
		resolve(t, "PutImage", `{"registryId":"999999999999","repositoryName":"app"}`))
}

// The registry-level surface is what AWS evaluates against "*".
func TestResourceARNs_RegistryLevelActionsStayAccountWide(t *testing.T) {
	for _, action := range []string{
		"GetAuthorizationToken", "GetRegistryPolicy", "PutRegistryPolicy", "DescribeRegistry",
		"GetRegistryScanningConfiguration", "PutRegistryScanningConfiguration",
		"BatchGetRepositoryScanningConfiguration", "PutReplicationConfiguration", "ListRepositories",
	} {
		assert.Equal(t, []string{"*"}, resolve(t, action, `{"repositoryName":"app"}`), "action %q", action)
	}
}

// A tag request names its resource by ARN; the tag handler keys off the
// repository name alone, so the gate re-anchors on the caller's account.
func TestResourceARNs_TagARNIsReanchored(t *testing.T) {
	foreign := `{"resourceArn":"arn:aws:ecr:us-east-1:999999999999:repository/app"}`
	assert.Equal(t, []string{ecrARN("repository/app")}, resolve(t, "ListTagsForResource", foreign))
	assert.Equal(t, []string{"*"},
		resolve(t, "TagResource", `{"resourceArn":"arn:aws:ecr:::registry/app"}`))
}

// A body the gate cannot parse authorizes account-wide: it is the handler that
// rejects a malformed request.
func TestResourceARNs_UnparseableOrAbsentIdentifierAuthorizesAccountWide(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteRepository", "{not json"))
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteRepository", `{}`))
}

// An action that is served but has no entry fails closed rather than defaulting
// to an account-wide grant.
func TestResourceARNs_UnknownActionIsRejected(t *testing.T) {
	_, err := gateway_ecrapi.ResourceARNs("MadeUpAction", testRegion, testAccountID, nil)
	require.Error(t, err)
}
