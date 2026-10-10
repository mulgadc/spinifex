package awsapi

import (
	"github.com/aws/aws-sdk-go/aws"
	awsecr "github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
)

// RepositoryEndpoint is the ECR endpoint profile the composition root supplies
// to AWS action adapters. It projects a configured registry listener into the
// AWS-visible repository ARN and URI forms without depending on gateway state.
type RepositoryEndpoint struct {
	Region         string
	ServicesDomain string
	RegistryHost   string
	RegistryPort   string
}

// RepositoryARN returns the ECR ARN for an account-scoped repository.
func (e RepositoryEndpoint) RepositoryARN(accountID, name string) string {
	return "arn:aws:ecr:" + e.Region + ":" + accountID + ":repository/" + name
}

// RegistryURIHost returns the configured registry listener for an account. An
// explicit advertised host takes precedence; otherwise the AWS-parity ECR DNS
// name is used. Standard HTTPS omits port 443 from the URI.
func (e RepositoryEndpoint) RegistryURIHost(accountID string) string {
	host := e.RegistryHost
	if host == "" {
		host = accountID + ".dkr.ecr." + e.Region + "." + e.ServicesDomain
	}
	if e.RegistryPort != "" && e.RegistryPort != "443" {
		return host + ":" + e.RegistryPort
	}
	return host
}

// RepositoryURI returns the pull/push URI for an account-scoped repository.
func (e RepositoryEndpoint) RepositoryURI(accountID, name string) string {
	return e.RegistryURIHost(accountID) + "/" + name
}

// RepositoryFromMeta projects domain repository metadata into the AWS ECR
// response shape shared by create, describe and delete actions.
func (e RepositoryEndpoint) RepositoryFromMeta(accountID, name string, meta ecr.RepoMeta) *awsecr.Repository {
	return &awsecr.Repository{
		RegistryId:         aws.String(accountID),
		RepositoryName:     aws.String(name),
		RepositoryArn:      aws.String(e.RepositoryARN(accountID, name)),
		RepositoryUri:      aws.String(e.RepositoryURI(accountID, name)),
		CreatedAt:          aws.Time(meta.CreatedAt),
		ImageTagMutability: aws.String(meta.TagMutability()),
		EncryptionConfiguration: &awsecr.EncryptionConfiguration{
			EncryptionType: aws.String(meta.EncryptionTypeOrDefault()),
		},
		ImageScanningConfiguration: &awsecr.ImageScanningConfiguration{
			ScanOnPush: aws.Bool(meta.ScanOnPush),
		},
	}
}
