package handlers_quota

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_eip "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eip"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// exceeds rejects with ResourceLimitExceeded when an account already holding
// count of a live-counted dimension would pass its cap by adding want more. It is
// the shared comparison for every live dimension; the per-dimension methods
// supply count from the relevant Describe* call.
func exceeds(count, want, limit int) error {
	if count+want > limit {
		return errors.New(awserrors.ErrorResourceLimitExceeded)
	}
	return nil
}

// EnforceVPCs gates CreateVpc on the account's live DescribeVpcs count.
func (s *Service) EnforceVPCs(natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_vpc.DescribeVpcs(&ec2.DescribeVpcsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Vpcs), want, s.limits.VPCs)
}

// EnforceSubnets gates CreateSubnet on the account's live DescribeSubnets count.
// Subnets are capped per-account in aggregate, not per-VPC.
func (s *Service) EnforceSubnets(natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_vpc.DescribeSubnets(&ec2.DescribeSubnetsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Subnets), want, s.limits.Subnets)
}

// EnforceEIPs gates AllocateAddress on the account's live DescribeAddresses count.
func (s *Service) EnforceEIPs(natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_eip.DescribeAddresses(&ec2.DescribeAddressesInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Addresses), want, s.limits.EIPs)
}
