package awsapi

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/stretchr/testify/assert"
)

func TestRepositoryEndpointRegistryURIHost(t *testing.T) {
	accountID := "123456789012"
	cases := []struct {
		name         string
		registryHost string
		port         string
		want         string
	}{
		{"parity, port-less", "", "", accountID + ".dkr.ecr.ap-southeast-2.services.internal"},
		{"parity, 443 port-less", "", "443", accountID + ".dkr.ecr.ap-southeast-2.services.internal"},
		{"parity, non-standard port", "", "9999", accountID + ".dkr.ecr.ap-southeast-2.services.internal:9999"},
		{"advertised host", "registry.example.com", "9999", "registry.example.com:9999"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := RepositoryEndpoint{
				Region:         "ap-southeast-2",
				ServicesDomain: "services.internal",
				RegistryHost:   tc.registryHost,
				RegistryPort:   tc.port,
			}
			assert.Equal(t, tc.want, endpoint.RegistryURIHost(accountID))
		})
	}
}

func TestRepositoryEndpointRepositoryFromMeta(t *testing.T) {
	createdAt := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	endpoint := RepositoryEndpoint{Region: "us-east-1", ServicesDomain: "services.internal"}

	got := endpoint.RepositoryFromMeta("123456789012", "team/app", ecr.RepoMeta{
		CreatedAt:          createdAt,
		ImageTagMutability: ecr.TagMutabilityImmutable,
		EncryptionType:     ecr.EncryptionTypeKMS,
		ScanOnPush:         true,
	})

	assert.Equal(t, "123456789012", aws.StringValue(got.RegistryId))
	assert.Equal(t, "team/app", aws.StringValue(got.RepositoryName))
	assert.Equal(t, "arn:aws:ecr:us-east-1:123456789012:repository/team/app", aws.StringValue(got.RepositoryArn))
	assert.Equal(t, "123456789012.dkr.ecr.us-east-1.services.internal/team/app", aws.StringValue(got.RepositoryUri))
	assert.Equal(t, createdAt, aws.TimeValue(got.CreatedAt))
	assert.Equal(t, ecr.TagMutabilityImmutable, aws.StringValue(got.ImageTagMutability))
	assert.Equal(t, ecr.EncryptionTypeKMS, aws.StringValue(got.EncryptionConfiguration.EncryptionType))
	assert.True(t, aws.BoolValue(got.ImageScanningConfiguration.ScanOnPush))
}
