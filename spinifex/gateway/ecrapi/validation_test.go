package gateway_ecrapi_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsRepositoryNameMessage is AWS's text for a repositoryName that fails the
// name pattern, captured from CreateRepository with "Bad_Name!".
const awsRepositoryNameMessage = `Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must satisfy regular expression '[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*''`

func requireInvalidParameter(t *testing.T, err error, wantMessage string) {
	t.Helper()
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Equal(t, wantMessage, message)
}

func TestValidateRepositoryName(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"pattern", "Bad_Name!", awsRepositoryNameMessage},
		{"uppercase", "Team/App", awsRepositoryNameMessage},
		{"missing", "", "1 validation error detected: Value null at 'repositoryName' failed to satisfy constraint: Member must not be null"},
		{"too short", "a", "1 validation error detected: Value 'a' at 'repositoryName' failed to satisfy constraint: Member must have length greater than or equal to 2"},
		{"too long", strings.Repeat("a", 257), "1 validation error detected: Value '" + strings.Repeat("a", 257) + "' at 'repositoryName' failed to satisfy constraint: Member must have length less than or equal to 256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireInvalidParameter(t, gateway_ecrapi.ValidateRepositoryName(tc.input), tc.want)
		})
	}
	assert.NoError(t, gateway_ecrapi.ValidateRepositoryName("team/app"))
}

func TestValidateTags(t *testing.T) {
	requireInvalidParameter(t,
		gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("env")}, {Value: aws.String("prod")}}),
		"1 validation error detected: Value null at 'tags.2.member.key' failed to satisfy constraint: Member must not be null")
	requireInvalidParameter(t,
		gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("")}}),
		"1 validation error detected: Value '' at 'tags.1.member.key' failed to satisfy constraint: Member must have length greater than or equal to 1")
	assert.NoError(t, gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("env"), Value: aws.String("")}}))
}

func TestResourceARNs_AmbiguousBodyNamesTheFault(t *testing.T) {
	_, err := gateway_ecrapi.ResourceARNs("DeleteRepository", "ap-southeast-2", "123456789012",
		[]byte(`{"repositoryName":"dev","RepositoryName":"prod"}`))
	requireInvalidParameter(t, err, "The request body names the same field more than once in different letter case")
}
