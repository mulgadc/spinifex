package gateway_ec2_tags

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	"github.com/nats-io/nats.go"
)

// DescribeTags handles the EC2 DescribeTags API call
func DescribeTags(input *ec2.DescribeTagsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeTagsOutput, error) {
	var output ec2.DescribeTagsOutput

	svc := handlers_ec2_tags.NewNATSTagsService(natsConn)
	result, err := svc.DescribeTags(input, accountID)
	if err != nil {
		return output, err
	}

	return *result, nil
}
