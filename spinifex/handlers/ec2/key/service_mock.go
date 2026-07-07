package handlers_ec2_key

import (
	"context"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

var _ KeyService = (*MockKeyService)(nil)

// MockKeyService provides mock responses for testing
type MockKeyService struct{}

// NewMockKeyService creates a new mock key service
func NewMockKeyService() KeyService {
	return &MockKeyService{}
}

func (s *MockKeyService) CreateKeyPair(ctx context.Context, input *ec2.CreateKeyPairInput, accountID string) (*ec2.CreateKeyPairOutput, error) {
	return &ec2.CreateKeyPairOutput{
		KeyFingerprint: aws.String("1f:51:ae:28:bf:89:e9:d8:1f:25:5d:37:2d:7d:b8:ca:9f:f5:f1:6f"),
		KeyMaterial:    aws.String("-----BEGIN RSA PRIVATE KEY-----\nMIIEpQIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----"),
		KeyName:        input.KeyName,
		KeyPairId:      aws.String("key-0123456789abcdef0"),
	}, nil
}

func (s *MockKeyService) DeleteKeyPair(ctx context.Context, input *ec2.DeleteKeyPairInput, accountID string) (*ec2.DeleteKeyPairOutput, error) {
	return &ec2.DeleteKeyPairOutput{
		Return:    aws.Bool(true),
		KeyPairId: input.KeyPairId,
	}, nil
}

func (s *MockKeyService) DescribeKeyPairs(ctx context.Context, input *ec2.DescribeKeyPairsInput, accountID string) (*ec2.DescribeKeyPairsOutput, error) {
	return &ec2.DescribeKeyPairsOutput{
		KeyPairs: []*ec2.KeyPairInfo{
			{
				KeyPairId:      aws.String("key-0123456789abcdef0"),
				KeyFingerprint: aws.String("1f:51:ae:28:bf:89:e9:d8:1f:25:5d:37:2d:7d:b8:ca:9f:f5:f1:6f"),
				KeyName:        aws.String("test-key"),
				KeyType:        aws.String("rsa"),
			},
		},
	}, nil
}

func (s *MockKeyService) ImportKeyPair(ctx context.Context, input *ec2.ImportKeyPairInput, accountID string) (*ec2.ImportKeyPairOutput, error) {
	return &ec2.ImportKeyPairOutput{
		KeyFingerprint: aws.String("1f:51:ae:28:bf:89:e9:d8:1f:25:5d:37:2d:7d:b8:ca:9f:f5:f1:6f"),
		KeyName:        input.KeyName,
		KeyPairId:      aws.String("key-0987654321fedcba0"),
	}, nil
}
