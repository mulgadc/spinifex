package handlers_ec2_eip

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisabledEIPService_DescribeReturnsEmpty(t *testing.T) {
	s := NewDisabledEIPService()

	out, err := s.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{}, "123456789012")
	require.NoError(t, err)
	require.NotNil(t, out.Addresses)
	assert.Empty(t, out.Addresses)

	attrOut, err := s.DescribeAddressesAttribute(context.Background(), &ec2.DescribeAddressesAttributeInput{}, "123456789012")
	require.NoError(t, err)
	require.NotNil(t, attrOut.Addresses)
	assert.Empty(t, attrOut.Addresses)
}

func TestDisabledEIPService_MutationsUnsupported(t *testing.T) {
	s := NewDisabledEIPService()

	_, err := s.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorUnsupportedOperation)
	_, err = s.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorUnsupportedOperation)
	_, err = s.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorUnsupportedOperation)
	_, err = s.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{}, "123456789012")
	assert.EqualError(t, err, awserrors.ErrorUnsupportedOperation)
}
