package gateway_ecrapi

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/nats-io/nats.go"
)

// ListTagsForResource returns the resource tags for an ECR repository. Resource
// tagging is a deferred feature (TagResource/UntagResource are not implemented),
// so this returns an empty tag set. AWS clients such as the Terraform provider
// call ListTagsForResource on every repository read, so a 200 with no tags keeps
// those reads working without claiming tag support.
func ListTagsForResource(_ context.Context, _ *nats.Conn, _ string, _ []byte) (any, error) {
	return &ecr.ListTagsForResourceOutput{Tags: []*ecr.Tag{}}, nil
}
