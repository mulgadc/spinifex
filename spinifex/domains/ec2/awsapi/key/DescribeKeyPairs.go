package key

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	"github.com/nats-io/nats.go"
)

// DescribeKeyPairs lists key pairs via NATS.
func DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, natsConn *nats.Conn, accountID string) (output ec2.DescribeKeyPairsOutput, err error) {
	keyService := ec2key.NewNATSKeyService(natsConn)
	result, err := keyService.DescribeKeyPairs(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
