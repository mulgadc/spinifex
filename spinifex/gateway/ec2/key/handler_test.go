package gateway_ec2_key

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

// Handler tests — call handlers directly to cover validation + NATS error paths

func TestCreateKeyPair_ValidationErrors(t *testing.T) {
	_, err := CreateKeyPair(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestCreateKeyPair_NilNATS(t *testing.T) {
	_, err := CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("my-key"),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDeleteKeyPair_ValidationErrors(t *testing.T) {
	_, err := DeleteKeyPair(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteKeyPair_NilNATS(t *testing.T) {
	_, err := DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
		KeyName: aws.String("my-key"),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestImportKeyPair_ValidationErrors(t *testing.T) {
	_, err := ImportKeyPair(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)

	_, err = ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName: aws.String("my-key"),
	}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestImportKeyPair_NilNATS(t *testing.T) {
	_, err := ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("my-key"),
		PublicKeyMaterial: []byte("ssh-rsa AAAA..."),
	}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDescribeKeyPairs_NilNATS(t *testing.T) {
	_, err := DescribeKeyPairs(context.Background(), nil, nil, "acct-123")
	assert.Error(t, err)

	_, err = DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, nil, "acct-123")
	assert.Error(t, err)
}
