package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// TagResource — POST /tags/{arn}
func TagResource(natsConn *nats.Conn, accountID, resourceARN string, body []byte) (*eks.TagResourceOutput, error) {
	input := new(eks.TagResourceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ResourceArn = aws.String(resourceARN)
	return handlers_eks.NewNATSEKSService(natsConn).TagResource(input, accountID)
}

// UntagResource — DELETE /tags/{arn}
func UntagResource(natsConn *nats.Conn, accountID, resourceARN string, body []byte) (*eks.UntagResourceOutput, error) {
	input := new(eks.UntagResourceInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ResourceArn = aws.String(resourceARN)
	return handlers_eks.NewNATSEKSService(natsConn).UntagResource(input, accountID)
}

// ListTagsForResource — GET /tags/{arn}
func ListTagsForResource(natsConn *nats.Conn, accountID, resourceARN string) (*eks.ListTagsForResourceOutput, error) {
	input := &eks.ListTagsForResourceInput{ResourceArn: aws.String(resourceARN)}
	return handlers_eks.NewNATSEKSService(natsConn).ListTagsForResource(input, accountID)
}
