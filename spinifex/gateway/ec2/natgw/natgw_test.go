package gateway_ec2_natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

// CreateNatGateway tests

func TestValidateCreateNatGatewayInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.CreateNatGatewayInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing SubnetId", &ec2.CreateNatGatewayInput{AllocationId: aws.String("eipalloc-1")}, awserrors.ErrorMissingParameter},
		{"empty SubnetId", &ec2.CreateNatGatewayInput{SubnetId: aws.String(""), AllocationId: aws.String("eipalloc-1")}, awserrors.ErrorMissingParameter},
		{"missing AllocationId", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1")}, awserrors.ErrorMissingParameter},
		{"empty AllocationId", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1"), AllocationId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"private connectivity unsupported", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1"), ConnectivityType: aws.String("private")}, awserrors.ErrorUnsupported},
		{"private connectivity unsupported with AllocationId", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1"), AllocationId: aws.String("eipalloc-1"), ConnectivityType: aws.String("private")}, awserrors.ErrorUnsupported},
		{"valid input", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1"), AllocationId: aws.String("eipalloc-1")}, ""},
		{"valid public connectivity", &ec2.CreateNatGatewayInput{SubnetId: aws.String("subnet-1"), AllocationId: aws.String("eipalloc-1"), ConnectivityType: aws.String("public")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCreateNatGatewayInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestCreateNatGateway_NilInput(t *testing.T) {
	_, err := CreateNatGateway(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateNatGateway_NilNATS(t *testing.T) {
	_, err := CreateNatGateway(&ec2.CreateNatGatewayInput{
		SubnetId:     aws.String("subnet-1"),
		AllocationId: aws.String("eipalloc-1"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DeleteNatGateway tests

func TestValidateDeleteNatGatewayInput(t *testing.T) {
	tests := []struct {
		name    string
		input   *ec2.DeleteNatGatewayInput
		wantErr string
	}{
		{"nil input", nil, awserrors.ErrorInvalidParameterValue},
		{"missing NatGatewayId", &ec2.DeleteNatGatewayInput{}, awserrors.ErrorMissingParameter},
		{"empty NatGatewayId", &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String("")}, awserrors.ErrorMissingParameter},
		{"valid input", &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String("nat-1")}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDeleteNatGatewayInput(tt.input)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tt.wantErr)
			}
		})
	}
}

func TestDeleteNatGateway_NilInput(t *testing.T) {
	_, err := DeleteNatGateway(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDeleteNatGateway_NilNATS(t *testing.T) {
	_, err := DeleteNatGateway(&ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String("nat-1"),
	}, nil, testAccountID)
	assert.Error(t, err)
}

// DescribeNatGateways tests

func TestDescribeNatGateways_NilInput(t *testing.T) {
	_, err := DescribeNatGateways(nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeNatGateways_NilNATS(t *testing.T) {
	_, err := DescribeNatGateways(&ec2.DescribeNatGatewaysInput{}, nil, testAccountID)
	assert.Error(t, err)
}
