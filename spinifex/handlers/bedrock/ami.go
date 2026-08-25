package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// servingAMIRoleTag identifies the single vllm-serving runtime AMI, the same
// way engineTagKey/engineVersionTagKey identify an RDS engine build. There is
// only one serving runtime today (vLLM), so no version filter is needed —
// every self-host catalog entry boots the same guest OS/runtime and differs
// only by the weights volume it is handed and its VLLMArgs.
const servingAMIRoleTag = "spinifex:bedrock-role"

// ErrServingAMINotFound is returned when no vllm-serving AMI is registered.
var ErrServingAMINotFound = errors.New("bedrock: no vllm-serving AMI found")

type amiResolver interface {
	DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

// resolveServingAMI finds the vllm-serving AMI, taking the most recently
// imported one if more than one is registered (a rebuild leaves the old image
// in place until an operator prunes it).
func resolveServingAMI(ctx context.Context, imgSvc amiResolver) (string, error) {
	out, err := imgSvc.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByBedrock})},
			{Name: aws.String("tag:" + servingAMIRoleTag), Values: aws.StringSlice([]string{"vllm-serving"})},
		},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", fmt.Errorf("bedrock: describe vllm-serving AMI: %w", err)
	}
	if out == nil {
		return "", ErrServingAMINotFound
	}

	// Serving images are GPU-tagged by design, so nothing is excluded here.
	newestID, _, matches := utils.SelectNewestImage(out.Images, "")
	if newestID == "" {
		return "", ErrServingAMINotFound
	}
	if matches > 1 {
		slog.WarnContext(ctx, "bedrock: multiple vllm-serving AMIs registered; using newest",
			"imageId", newestID, "matches", matches)
	}
	return newestID, nil
}
