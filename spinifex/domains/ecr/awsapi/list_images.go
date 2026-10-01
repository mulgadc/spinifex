package awsapi

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
)

type listImagesRequest struct {
	RepositoryName string           `json:"repositoryName"`
	RegistryID     string           `json:"registryId"`
	Filter         *tagStatusFilter `json:"filter"`
}

type tagStatusFilter struct {
	TagStatus string `json:"tagStatus"`
}

// ListImages returns every image identifier in the caller's repository,
// optionally filtering tagged or untagged manifests. Each tag is an AWS image
// identifier; an untagged manifest is represented by its digest only.
func ListImages(ctx context.Context, catalog ImageCatalog, accountID string, body []byte) (*ecr.ListImagesOutput, error) {
	var req listImagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := validateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}

	records, err := listImageRecords(ctx, catalog, accountID, req.RepositoryName)
	if err != nil {
		return nil, err
	}

	status := ""
	if req.Filter != nil {
		status = req.Filter.TagStatus
	}
	identifiers := make([]*ecr.ImageIdentifier, 0, len(records))
	for _, record := range records {
		if len(record.Tags) == 0 {
			if status == ecr.TagStatusTagged {
				continue
			}
			identifiers = append(identifiers, &ecr.ImageIdentifier{ImageDigest: aws.String(record.Digest)})
			continue
		}
		if status == ecr.TagStatusUntagged {
			continue
		}
		for _, tag := range record.Tags {
			identifiers = append(identifiers, &ecr.ImageIdentifier{
				ImageDigest: aws.String(record.Digest),
				ImageTag:    aws.String(tag),
			})
		}
	}

	return &ecr.ListImagesOutput{ImageIds: identifiers}, nil
}
