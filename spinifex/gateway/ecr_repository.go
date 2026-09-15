package gateway

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// buildRepository projects a RepoMeta record into the AWS *ecr.Repository
// shape shared by CreateRepository, DescribeRepositories, and
// DeleteRepository, so a new field is added here once rather than thrice.
func (gw *GatewayConfig) buildRepository(accountID, name string, meta handlers_ecr.RepoMeta) *ecr.Repository {
	return &ecr.Repository{
		RegistryId:         aws.String(accountID),
		RepositoryName:     aws.String(name),
		RepositoryArn:      aws.String(gw.ecrRepositoryArn(accountID, name)),
		RepositoryUri:      aws.String(gw.ecrRepositoryUri(accountID, name)),
		CreatedAt:          aws.Time(meta.CreatedAt),
		ImageTagMutability: aws.String(meta.TagMutability()),
		EncryptionConfiguration: &ecr.EncryptionConfiguration{
			EncryptionType: aws.String(meta.EncryptionTypeOrDefault()),
		},
		ImageScanningConfiguration: &ecr.ImageScanningConfiguration{
			ScanOnPush: aws.Bool(meta.ScanOnPush),
		},
	}
}
