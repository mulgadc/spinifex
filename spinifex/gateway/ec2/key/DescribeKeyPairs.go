// Package gateway_ec2_key implements the EC2 key pair actions: it validates
// each request and forwards it to the key pair service over NATS.
package gateway_ec2_key

import (
	"context"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	"github.com/nats-io/nats.go"
)

// DescribeKeyPairs lists key pairs via NATS.
func DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, natsConn *nats.Conn, accountID string) (output ec2.DescribeKeyPairsOutput, err error) {
	keyService := handlers_ec2_key.NewNATSKeyService(natsConn)
	result, err := keyService.DescribeKeyPairs(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
