package handlers_quota

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_eip "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eip"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	"github.com/nats-io/nats.go"
)

// Resource type identifiers for the live-counted quota dimensions. These hold
// no stored state: usage is the current count from an account-filtered
// Describe* sweep, so deletion frees quota on the next call.
const (
	ResourceVPC    = "vpc"
	ResourceSubnet = "subnet"
	ResourceEIP    = "eip"
	// ResourceStorage is counted in GiB summed across an account's volumes
	// rather than as a resource count, but shares the same EnforceLive
	// comparison against its configured cap.
	ResourceStorage = "storage"
)

// EnforceLive rejects with ResourceLimitExceeded when an account already
// holding count of resourceType would exceed its configured limit by adding
// want more. It is the pure comparison shared by every live-counted dimension;
// the per-dimension methods supply count from the relevant Describe* call.
func (s *Service) EnforceLive(resourceType string, count, want int) error {
	limit, ok := s.liveLimit(resourceType)
	if !ok {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if count+want > limit {
		return errors.New(awserrors.ErrorResourceLimitExceeded)
	}
	return nil
}

// liveLimit maps a live-counted resource type to its configured cap.
func (s *Service) liveLimit(resourceType string) (int, bool) {
	switch resourceType {
	case ResourceVPC:
		return s.limits.VPCs, true
	case ResourceSubnet:
		return s.limits.Subnets, true
	case ResourceEIP:
		return s.limits.EIPs, true
	case ResourceStorage:
		return s.limits.VolumesGiB, true
	default:
		return 0, false
	}
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
	return s.EnforceLive(ResourceVPC, len(out.Vpcs), want)
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
	return s.EnforceLive(ResourceSubnet, len(out.Subnets), want)
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
	return s.EnforceLive(ResourceEIP, len(out.Addresses), want)
}
