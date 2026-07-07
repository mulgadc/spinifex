package gateway_ec2_igw

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

func TestCreateInternetGateway_NilInput(t *testing.T) {
	_, err := CreateInternetGateway(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteInternetGateway_NilInput(t *testing.T) {
	_, err := DeleteInternetGateway(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteInternetGateway_NilIGWId(t *testing.T) {
	_, err := DeleteInternetGateway(context.Background(), &ec2.DeleteInternetGatewayInput{}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDeleteInternetGateway_EmptyIGWId(t *testing.T) {
	_, err := DeleteInternetGateway(context.Background(), &ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String("")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachInternetGateway_NilInput(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAttachInternetGateway_NilIGWId(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{VpcId: aws.String("vpc-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachInternetGateway_EmptyIGWId(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String(""), VpcId: aws.String("vpc-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachInternetGateway_NilVpcId(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String("igw-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAttachInternetGateway_EmptyVpcId(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String("igw-123"), VpcId: aws.String("")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachInternetGateway_NilInput(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDetachInternetGateway_NilIGWId(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{VpcId: aws.String("vpc-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachInternetGateway_EmptyIGWId(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String(""), VpcId: aws.String("vpc-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachInternetGateway_NilVpcId(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String("igw-123")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDetachInternetGateway_EmptyVpcId(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String("igw-123"), VpcId: aws.String("")}, nil, "")
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDescribeInternetGateways_NilInput(t *testing.T) {
	_, err := DescribeInternetGateways(context.Background(), nil, nil, "")
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

// Handler tests with valid input + nil NATS — covers NATS error paths

func TestCreateInternetGateway_NilNATS(t *testing.T) {
	_, err := CreateInternetGateway(context.Background(), &ec2.CreateInternetGatewayInput{}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDeleteInternetGateway_NilNATS(t *testing.T) {
	_, err := DeleteInternetGateway(context.Background(), &ec2.DeleteInternetGatewayInput{InternetGatewayId: aws.String("igw-123")}, nil, "acct-123")
	assert.Error(t, err)
}

func TestAttachInternetGateway_NilNATS(t *testing.T) {
	_, err := AttachInternetGateway(context.Background(), &ec2.AttachInternetGatewayInput{InternetGatewayId: aws.String("igw-123"), VpcId: aws.String("vpc-123")}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDetachInternetGateway_NilNATS(t *testing.T) {
	_, err := DetachInternetGateway(context.Background(), &ec2.DetachInternetGatewayInput{InternetGatewayId: aws.String("igw-123"), VpcId: aws.String("vpc-123")}, nil, "acct-123")
	assert.Error(t, err)
}

func TestDescribeInternetGateways_NilNATS(t *testing.T) {
	_, err := DescribeInternetGateways(context.Background(), &ec2.DescribeInternetGatewaysInput{}, nil, "acct-123")
	assert.Error(t, err)
}
