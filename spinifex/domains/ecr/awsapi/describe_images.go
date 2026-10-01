package awsapi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// imageIdentifier is the AWS JSON 1.1 {imageDigest, imageTag} pair.
type imageIdentifier struct {
	ImageDigest string `json:"imageDigest"`
	ImageTag    string `json:"imageTag"`
}

type describeImagesRequest struct {
	RepositoryName string            `json:"repositoryName"`
	RegistryID     string            `json:"registryId"`
	ImageIDs       []imageIdentifier `json:"imageIds"`
	// Filter is accepted but not yet implemented. Preserve its current wire
	// behavior during the structural extraction; its semantics are a separate
	// ECR compatibility decision.
	Filter *tagStatusFilter `json:"filter"`
}

// DescribeImages returns detailed metadata for repository images, optionally
// narrowed to requested digests or tags. The current implementation preserves
// the existing behavior of accepting, but not applying, the filter field.
func DescribeImages(ctx context.Context, catalog ImageCatalog, accountID string, body []byte) (*ecr.DescribeImagesOutput, error) {
	var req describeImagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, MalformedBodyError()
	}
	if err := ValidateRepositoryScope(req.RepositoryName, req.RegistryID, accountID); err != nil {
		return nil, err
	}

	records, err := listImageRecords(ctx, catalog, accountID, req.RepositoryName)
	if err != nil {
		return nil, err
	}

	wanted := func(record ecrregistry.ImageRecord) bool {
		if len(req.ImageIDs) == 0 {
			return true
		}
		for _, image := range req.ImageIDs {
			if image.ImageDigest != "" && image.ImageDigest == record.Digest {
				return true
			}
			if image.ImageTag != "" && slices.Contains(record.Tags, image.ImageTag) {
				return true
			}
		}
		return false
	}

	details := make([]*ecr.ImageDetail, 0, len(records))
	for _, record := range records {
		if !wanted(record) {
			continue
		}
		detail := &ecr.ImageDetail{
			RegistryId:             aws.String(accountID),
			RepositoryName:         aws.String(req.RepositoryName),
			ImageDigest:            aws.String(record.Digest),
			ImageSizeInBytes:       aws.Int64(record.Size),
			ImageManifestMediaType: aws.String(record.MediaType),
		}
		if !record.PushedAt.IsZero() {
			detail.ImagePushedAt = aws.Time(record.PushedAt)
		}
		for _, tag := range record.Tags {
			detail.ImageTags = append(detail.ImageTags, aws.String(tag))
		}
		details = append(details, detail)
	}
	if len(req.ImageIDs) > 0 && len(details) == 0 {
		return nil, errors.New(awserrors.ErrorImageNotFound)
	}

	return &ecr.DescribeImagesOutput{ImageDetails: details}, nil
}
