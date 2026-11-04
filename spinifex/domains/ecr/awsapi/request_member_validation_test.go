package awsapi

import (
	"context"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/stretchr/testify/require"
)

// The messages below are AWS's, captured from real ECR refusals.
const (
	awsMaxResultsLow   = "Invalid parameter at 'maxResults' failed to satisfy constraint: 'Member must have value greater than or equal to 1'"
	awsMaxResultsHigh  = "Invalid parameter at 'maxResults' failed to satisfy constraint: 'Member must have value less than or equal to 1000'"
	awsImageStatusEnum = "Invalid parameter at 'filter.imageStatus' failed to satisfy constraint: 'Member must satisfy enum value set: [ARCHIVED, ACTIVE, ANY, ACTIVATING]'"
	awsExclusionField  = "Invalid parameter at 'imageTagMutabilityExclusionFilters"
	awsExclusionRegex  = "Member must satisfy regular expression pattern: ^[0-9a-zA-Z._*-]{1,128}$"
)

func TestListActions_RefuseMaxResultsOutsideRange(t *testing.T) {
	store := ecr.NewNATSMetaStore(newRepositoryActionTestConn(t))
	for value, want := range map[string]string{"0": awsMaxResultsLow, "1001": awsMaxResultsHigh} {
		repoBody := []byte(`{"repositoryName":"team/app","maxResults":` + value + `}`)
		_, err := ListImages(context.Background(), nil, "123456789012", repoBody)
		requireECRInvalidParameter(t, err, want)
		_, err = DescribeImages(context.Background(), nil, "123456789012", repoBody)
		requireECRInvalidParameter(t, err, want)
		_, err = DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"maxResults":`+value+`}`))
		requireECRInvalidParameter(t, err, want)
	}
	for _, value := range []string{"1", "1000"} {
		_, err := DescribeRepositories(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount, []byte(`{"maxResults":`+value+`}`))
		require.NoError(t, err, value)
	}
}

func TestImageListActions_RefuseImageStatusOutsideEnum(t *testing.T) {
	body := []byte(`{"repositoryName":"team/app","filter":{"imageStatus":"BOGUS"}}`)
	_, err := ListImages(context.Background(), nil, "123456789012", body)
	requireECRInvalidParameter(t, err, awsImageStatusEnum)
	_, err = DescribeImages(context.Background(), nil, "123456789012", body)
	requireECRInvalidParameter(t, err, awsImageStatusEnum)
}

func TestTagMutabilityActions_RefuseExclusionFiltersOutsideModel(t *testing.T) {
	nc := newRepositoryActionTestConn(t)
	store := ecr.NewNATSMetaStore(nc)
	six := strings.TrimSuffix(strings.Repeat(`{"filterType":"WILDCARD","filter":"v*"},`, 6), ",")
	member := awsExclusionField + ".1.member."
	cases := map[string]string{
		`[]`:                                     awsExclusionField + "' failed to satisfy constraint: 'Member must have length greater than or equal to 1'",
		`[` + six + `]`:                          awsExclusionField + "' failed to satisfy constraint: 'Member must have length less than or equal to 5'",
		`[{"filterType":"REGEX","filter":"v*"}]`: member + "filterType' failed to satisfy constraint: 'Member must satisfy enum value set: [WILDCARD]'",
		`[{"filterType":"WILDCARD","filter":"v/1"}]`:                              member + "filter' failed to satisfy constraint: '" + awsExclusionRegex + "'",
		`[{"filterType":"WILDCARD","filter":""}]`:                                 member + "filter' failed to satisfy constraint: '" + awsExclusionRegex + "; Member must have length greater than or equal to 1'",
		`[{"filterType":"WILDCARD","filter":"` + strings.Repeat("a", 129) + `"}]`: member + "filter' failed to satisfy constraint: '" + awsExclusionRegex + "; Member must have length less than or equal to 128'",
	}
	for filters, want := range cases {
		_, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount,
			[]byte(`{"repositoryName":"team/app","imageTagMutabilityExclusionFilters":`+filters+`}`))
		requireECRInvalidParameter(t, err, want)
		_, err = PutImageTagMutability(context.Background(), nc, repositoryActionTestAccount,
			[]byte(`{"repositoryName":"team/app","imageTagMutability":"MUTABLE","imageTagMutabilityExclusionFilters":`+filters+`}`))
		requireECRInvalidParameter(t, err, want)
	}
	five := strings.Repeat(`{"filterType":"WILDCARD","filter":"`+strings.Repeat("v", 128)+`"},`, 4) + `{"filterType":"WILDCARD","filter":"v*"}`
	_, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount,
		[]byte(`{"repositoryName":"team/app","imageTagMutabilityExclusionFilters":[`+five+`]}`))
	require.NoError(t, err)
}

func TestCreateRepository_RefusesEveryKMSKey(t *testing.T) {
	store := ecr.NewNATSMetaStore(newRepositoryActionTestConn(t))
	cases := map[string]string{
		`{"encryptionType":"AES256","kmsKey":""}`:                                  "Invalid parameter at 'encryptionConfiguration.kmsKey' failed to satisfy constraint: 'Member must have length greater than or equal to 1'",
		`{"encryptionType":"AES256","kmsKey":"` + strings.Repeat("k", 2049) + `"}`: "Invalid parameter at 'encryptionConfiguration.kmsKey' failed to satisfy constraint: 'Member must have length less than or equal to 2048'",
		`{"encryptionType":"AES256","kmsKey":"alias/app"}`:                         "Invalid parameter at 'encryptionConfiguration' failed to satisfy constraint: 'Invalid encryption configuration: Encryption key should not be set for AES256 encryption.'",
	}
	for cfg, want := range cases {
		_, err := CreateRepository(context.Background(), store, repositoryActionEndpoint(), repositoryActionTestAccount,
			[]byte(`{"repositoryName":"team/app","encryptionConfiguration":`+cfg+`}`))
		requireECRInvalidParameter(t, err, want)
	}
}

func TestValidateAuthorizationTokenRequest_RefusesRegistryIDsOutsideModel(t *testing.T) {
	eleven := strings.TrimSuffix(strings.Repeat(`"123456789012",`, 11), ",")
	cases := map[string]string{
		`[]`:               "Invalid parameter at 'registryIds' failed to satisfy constraint: 'Member must have length greater than or equal to 1'",
		`[` + eleven + `]`: "Invalid parameter at 'registryIds' failed to satisfy constraint: 'Member must have length less than or equal to 10'",
		`["abc"]`:          "Invalid parameter at 'registryIds' failed to satisfy constraint: 'Member must satisfy constraint: [Member must satisfy regular expression pattern: [0-9]{12}]'",
	}
	for ids, want := range cases {
		requireECRInvalidParameter(t, ValidateAuthorizationTokenRequest([]byte(`{"registryIds":`+ids+`}`)), want)
	}
	for _, body := range []string{``, `{}`, `{"registryIds":["123456789012"]}`} {
		require.NoError(t, ValidateAuthorizationTokenRequest([]byte(body)), body)
	}
}
