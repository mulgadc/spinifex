package gateway_ec2_eip

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
)

const testAccountID = "123456789012"

// AllocateAddress tests

func TestAllocateAddress_NilInput(t *testing.T) {
	_, err := AllocateAddress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAllocateAddress_NilNATS(t *testing.T) {
	_, err := AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, nil, testAccountID)
	assert.Error(t, err)
}

// AssociateAddress tests

func TestAssociateAddress_NilInput(t *testing.T) {
	_, err := AssociateAddress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAssociateAddress_NilAllocationId(t *testing.T) {
	_, err := AssociateAddress(context.Background(), &ec2.AssociateAddressInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAssociateAddress_EmptyAllocationId(t *testing.T) {
	_, err := AssociateAddress(context.Background(), &ec2.AssociateAddressInput{AllocationId: aws.String("")}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestAssociateAddress_NilNATS(t *testing.T) {
	_, err := AssociateAddress(context.Background(), &ec2.AssociateAddressInput{AllocationId: aws.String("eipalloc-123")}, nil, testAccountID)
	assert.Error(t, err)
}

// DescribeAddresses tests

func TestDescribeAddresses_NilInput(t *testing.T) {
	_, err := DescribeAddresses(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeAddresses_NilNATS(t *testing.T) {
	_, err := DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, nil, testAccountID)
	assert.Error(t, err)
}

// DescribeAddressesAttribute tests

func TestDescribeAddressesAttribute_NilInput(t *testing.T) {
	_, err := DescribeAddressesAttribute(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDescribeAddressesAttribute_NilNATS(t *testing.T) {
	_, err := DescribeAddressesAttribute(context.Background(), &ec2.DescribeAddressesAttributeInput{}, nil, testAccountID)
	assert.Error(t, err)
}

// DisassociateAddress tests

func TestDisassociateAddress_NilInput(t *testing.T) {
	_, err := DisassociateAddress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestDisassociateAddress_NilAssociationId(t *testing.T) {
	_, err := DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDisassociateAddress_EmptyAssociationId(t *testing.T) {
	_, err := DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{AssociationId: aws.String("")}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestDisassociateAddress_NilNATS(t *testing.T) {
	_, err := DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{AssociationId: aws.String("eipassoc-123")}, nil, testAccountID)
	assert.Error(t, err)
}

// ReleaseAddress tests

func TestReleaseAddress_NilInput(t *testing.T) {
	_, err := ReleaseAddress(context.Background(), nil, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestReleaseAddress_NilAllocationId(t *testing.T) {
	_, err := ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestReleaseAddress_EmptyAllocationId(t *testing.T) {
	_, err := ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: aws.String("")}, nil, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorMissingParameter)
}

func TestReleaseAddress_NilNATS(t *testing.T) {
	_, err := ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{AllocationId: aws.String("eipalloc-123")}, nil, testAccountID)
	assert.Error(t, err)
}
