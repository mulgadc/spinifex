package handlers_ec2_eip

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// DisabledEIPService serves EIP requests when the cluster has no external
// IPAM (external_mode "nat" or disabled): reads return AWS-faithful empty
// lists, mutations return UnsupportedOperation.
type DisabledEIPService struct{}

var _ EIPService = (*DisabledEIPService)(nil)

func NewDisabledEIPService() *DisabledEIPService { return &DisabledEIPService{} }

func (s *DisabledEIPService) AllocateAddress(_ *ec2.AllocateAddressInput, _ string) (*ec2.AllocateAddressOutput, error) {
	return nil, errors.New(awserrors.ErrorUnsupportedOperation)
}

func (s *DisabledEIPService) ReleaseAddress(_ *ec2.ReleaseAddressInput, _ string) (*ec2.ReleaseAddressOutput, error) {
	return nil, errors.New(awserrors.ErrorUnsupportedOperation)
}

func (s *DisabledEIPService) AssociateAddress(_ *ec2.AssociateAddressInput, _ string) (*ec2.AssociateAddressOutput, error) {
	return nil, errors.New(awserrors.ErrorUnsupportedOperation)
}

func (s *DisabledEIPService) DisassociateAddress(_ *ec2.DisassociateAddressInput, _ string) (*ec2.DisassociateAddressOutput, error) {
	return nil, errors.New(awserrors.ErrorUnsupportedOperation)
}

func (s *DisabledEIPService) DescribeAddresses(_ *ec2.DescribeAddressesInput, _ string) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{Addresses: []*ec2.Address{}}, nil
}

func (s *DisabledEIPService) DescribeAddressesAttribute(_ *ec2.DescribeAddressesAttributeInput, _ string) (*ec2.DescribeAddressesAttributeOutput, error) {
	return &ec2.DescribeAddressesAttributeOutput{Addresses: []*ec2.AddressAttribute{}}, nil
}
