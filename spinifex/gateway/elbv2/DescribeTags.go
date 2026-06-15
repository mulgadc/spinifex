package gateway_elbv2

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// DescribeTags returns tags for the ELBv2 resources named in ResourceArns.
// Untagged resources produce a TagDescription with an empty Tags slice, as
// the Terraform AWS provider expects during post-create refresh.
func DescribeTags(input *elbv2.DescribeTagsInput, natsConn *nats.Conn, accountID string) (elbv2.DescribeTagsOutput, error) {
	var output elbv2.DescribeTagsOutput

	if input == nil || len(input.ResourceArns) == 0 {
		return output, errors.New(awserrors.ErrorMissingParameter)
	}

	svc := handlers_elbv2.NewNATSELBv2Service(natsConn)
	result, err := svc.DescribeTags(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
