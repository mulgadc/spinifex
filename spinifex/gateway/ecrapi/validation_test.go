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
	assert.Equal(t, awserrors.ErrorECRInvalidParameter, code)
	assert.Equal(t, wantMessage, message)
}

func TestValidateRepositoryName(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"pattern", "Bad_Name!", awsRepositoryNameMessage},
		{"uppercase", "Team/App", awsRepositoryNameMessage},
		{"missing", "", "Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must not be null'"},
		{"too short", "a", "Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must have length greater than or equal to 2'"},
		{"too long", strings.Repeat("a", 257), "Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must have length less than or equal to 256'"},
		{"triple underscore", "a___b", awsRepositoryNameMessage},
		{"underscore then hyphen", "a_-b", awsRepositoryNameMessage},
		{"leading separator", "_a", awsRepositoryNameMessage},
		{"trailing separator", "a_", awsRepositoryNameMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireInvalidParameter(t, gateway_ecrapi.ValidateRepositoryName(tc.input), tc.want)
		})
	}
	for _, name := range []string{"team/app", "a__b", "a--b", "a---b", "a/b__c", strings.Repeat("a", 256)} {
		assert.NoError(t, gateway_ecrapi.ValidateRepositoryName(name), "expected %q valid", name)
	}
}

func TestValidateTags(t *testing.T) {
	requireInvalidParameter(t,
		gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("env")}, {Value: aws.String("prod")}}),
		"Invalid parameter at 'tags.2.member.key' failed to satisfy constraint: 'Member must not be null'")
	err := gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("")}})
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, "InvalidTagParameterException", code)
	assert.Equal(t, "Tag parameters are invalid", message)
	assert.NoError(t, gateway_ecrapi.ValidateTags([]*ecr.Tag{{Key: aws.String("env"), Value: aws.String("")}}))
}

// The messages below are AWS's, captured from real ECR refusals.
func TestConstraintHelpers_AWSMessages(t *testing.T) {
	requireInvalidParameter(t, gateway_ecrapi.RequiredParameterError("imageTagMutability"),
		"Invalid parameter at 'imageTagMutability' failed to satisfy constraint: 'Member must not be null'")
	requireInvalidParameter(t, gateway_ecrapi.EnumValueError("imageTagMutability", gateway_ecrapi.ImageTagMutabilityValues...),
		"Invalid parameter at 'imageTagMutability' failed to satisfy constraint: 'Member must satisfy enum value set: [IMMUTABLE, MUTABLE, MUTABLE_WITH_EXCLUSION, IMMUTABLE_WITH_EXCLUSION]'")
	requireInvalidParameter(t, gateway_ecrapi.MaxItemsError("imageIds", 100),
		"Invalid parameter at 'imageIds' failed to satisfy constraint: 'Member must have length less than or equal to 100'")
}

func TestResourceARNs_AmbiguousBodyNamesTheFault(t *testing.T) {
	_, err := gateway_ecrapi.ResourceARNs("DeleteRepository", "ap-southeast-2", "123456789012",
		[]byte(`{"repositoryName":"dev","RepositoryName":"prod"}`))
	requireInvalidParameter(t, err, "The request body names the same field more than once in different letter case")
}
