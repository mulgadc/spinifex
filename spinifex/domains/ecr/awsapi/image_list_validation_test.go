package awsapi

import (
	"context"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The messages below are AWS's, captured from real ECR refusals.
const (
	awsImageIDsNull     = "Invalid parameter at 'imageIds' failed to satisfy constraint: 'Member must not be null'"
	awsImageIDsEmpty    = "Invalid parameter at 'imageIds' failed to satisfy constraint: 'Member must have length greater than or equal to 1'"
	awsImageIDsTooMany  = "Invalid parameter at 'imageIds' failed to satisfy constraint: 'Member must have length less than or equal to 100'"
	awsImageTagEmpty    = "Invalid parameter at 'imageIds.2.member.imageTag' failed to satisfy constraint: 'Member must have length greater than or equal to 1'"
	awsImageTagTooLong  = "Invalid parameter at 'imageIds.1.member.imageTag' failed to satisfy constraint: 'Member must have length less than or equal to 300'"
	awsMediaTypesEmpty  = "Invalid parameter at 'acceptedMediaTypes' failed to satisfy constraint: 'Member must have length greater than or equal to 1'"
	awsMediaTypesTooMny = "Invalid parameter at 'acceptedMediaTypes' failed to satisfy constraint: 'Member must have length less than or equal to 100'"
)

func requireECRInvalidParameter(t *testing.T, err error, want string) {
	t.Helper()
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECRInvalidParameter, code)
	assert.Equal(t, want, message)
}

func imageIDCases() map[string]struct{ ids, want string } {
	long := strings.Repeat("a", 301)
	return map[string]struct{ ids, want string }{
		"omitted":      {"", awsImageIDsNull},
		"empty":        {`,"imageIds":[]`, awsImageIDsEmpty},
		"too many":     {`,"imageIds":[` + repeatImageIdentifier(101) + `]`, awsImageIDsTooMany},
		"empty tag":    {`,"imageIds":[{"imageTag":"v1"},{"imageDigest":"sha256:x","imageTag":""}]`, awsImageTagEmpty},
		"tag too long": {`,"imageIds":[{"imageTag":"` + long + `"}]`, awsImageTagTooLong},
	}
}

func TestBatchImageActions_RefuseImageIDsOutsideModel(t *testing.T) {
	for name, tc := range imageIDCases() {
		body := []byte(`{"repositoryName":"team/app"` + tc.ids + `}`)
		t.Run("BatchGetImage "+name, func(t *testing.T) {
			_, err := BatchGetImage(context.Background(), fakeManifestReader{}, "123456789012", body)
			requireECRInvalidParameter(t, err, tc.want)
		})
		t.Run("BatchDeleteImage "+name, func(t *testing.T) {
			_, err := BatchDeleteImage(context.Background(), fakeImageDeleter{}, "123456789012", body)
			requireECRInvalidParameter(t, err, tc.want)
		})
	}
}

func TestBatchGetImage_RefusesAcceptedMediaTypesOutsideModel(t *testing.T) {
	many := strings.TrimSuffix(strings.Repeat(`"m",`, 101), ",")
	for want, types := range map[string]string{awsMediaTypesEmpty: `[]`, awsMediaTypesTooMny: `[` + many + `]`} {
		body := []byte(`{"repositoryName":"team/app","imageIds":[{"imageTag":"v1"}],"acceptedMediaTypes":` + types + `}`)
		_, err := BatchGetImage(context.Background(), fakeManifestReader{}, "123456789012", body)
		requireECRInvalidParameter(t, err, want)
	}
}

// DescribeImages makes imageIds optional, so only an omitted list lists all.
func TestDescribeImages_RefusesImageIDsOutsideModel(t *testing.T) {
	for name, tc := range imageIDCases() {
		if tc.want == awsImageIDsNull {
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, err := DescribeImages(context.Background(), nil, "123456789012", []byte(`{"repositoryName":"team/app"`+tc.ids+`}`))
			requireECRInvalidParameter(t, err, tc.want)
		})
	}
}

func TestDescribeRepositories_RefusesRepositoryNamesOutsideModel(t *testing.T) {
	store := ecr.NewNATSMetaStore(newRepositoryActionTestConn(t))
	many := strings.TrimSuffix(strings.Repeat(`"team/app",`, 101), ",")
	cases := map[string]string{
		`[]`:             "Invalid parameter at 'RepositoryNameList' failed to satisfy constraint: 'Must not be empty'",
		`[` + many + `]`: "Invalid parameter at 'RepositoryNameList' failed to satisfy constraint: 'Must have length less than or equal to 100'",
	}
	for names, want := range cases {
		_, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"repositoryNames":`+names+`}`))
		requireECRInvalidParameter(t, err, want)
	}
}
