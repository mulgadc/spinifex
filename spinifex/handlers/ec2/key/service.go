package handlers_ec2_key

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// KeyService defines the interface for EC2 key pair operations business logic
type KeyService interface {
	CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, accountID string) (*ec2.CreateKeyPairOutput, error)
	DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, accountID string) (*ec2.DeleteKeyPairOutput, error)
	DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, accountID string) (*ec2.DescribeKeyPairsOutput, error)
	ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, accountID string) (*ec2.ImportKeyPairOutput, error)
}
