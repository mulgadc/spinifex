package awsapi

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
)

type listImagesRequest struct {
	RepositoryName string           `json:"repositoryName"`
	RegistryID     string           `json:"registryId"`
	MaxResults     *int64           `json:"maxResults"`
	NextToken      *string          `json:"nextToken"`
	Filter         *tagStatusFilter `json:"filter"`
}

type tagStatusFilter struct {
	TagStatus   string  `json:"tagStatus"`
	ImageStatus *string `json:"imageStatus"`
}

// validate refuses a tagStatus or imageStatus outside its enum; an omitted
// filter or member is valid. imageStatus is checked but not applied.
func (f *tagStatusFilter) validate() error {
	switch {
	case f == nil:
		return nil
	case f.TagStatus != "" && !slices.Contains(TagStatusValues, f.TagStatus):
		return EnumValueError("filter.tagStatus", TagStatusValues...)
	case f.ImageStatus != nil && !slices.Contains(ImageStatusValues, *f.ImageStatus):
		return EnumValueError("filter.imageStatus", ImageStatusValues...)
	}
	return nil
}

// ListImages returns a page of the image identifiers in the caller's repository,
// optionally filtering tagged or untagged manifests. Each tag is an AWS image
// identifier; an untagged manifest is represented by its digest only.
func ListImages(ctx context.Context, catalog ImageCatalog, accountID string, body []byte) (*ecr.ListImagesOutput, error) {
	var req listImagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}
	page, err := parsePage(req.MaxResults, req.NextToken, "", 0)
	if err != nil {
		return nil, err
	}
	if err := req.Filter.validate(); err != nil {
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

	// A digest holds no "/", so digest/tag names one identifier uniquely.
	key := func(id *ecr.ImageIdentifier) string {
		return aws.StringValue(id.ImageDigest) + "/" + aws.StringValue(id.ImageTag)
	}
	identifiers, next := paging.EC2Page(identifiers, key, page)
	return &ecr.ListImagesOutput{ImageIds: identifiers, NextToken: next}, nil
}
