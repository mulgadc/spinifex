package handlers_quota

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_eip "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eip"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// exceeds rejects with ResourceLimitExceeded when an account already holding
// count of a live-counted dimension would pass its cap by adding want more. It is
// the shared comparison for every live dimension; the per-dimension methods
// supply count from the relevant Describe* call.
func exceeds(count, want, limit int) error {
	if limit == Unlimited {
		return nil
	}
	if count+want > limit {
		return errors.New(awserrors.ErrorResourceLimitExceeded)
	}
	return nil
}

// EnforceVPCs gates CreateVpc on the account's live DescribeVpcs count.
func (s *Service) EnforceVPCs(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_vpc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Vpcs), want, s.limitsFor(ctx, accountID).VPCs)
}

// EnforceSubnets gates CreateSubnet on the account's live DescribeSubnets count.
// Subnets are capped per-account in aggregate, not per-VPC.
func (s *Service) EnforceSubnets(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_vpc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Subnets), want, s.limitsFor(ctx, accountID).Subnets)
}

// EnforceEIPs gates AllocateAddress on the account's live DescribeAddresses count.
func (s *Service) EnforceEIPs(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_ec2_eip.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Addresses), want, s.limitsFor(ctx, accountID).EIPs)
}

// EnforceLoadBalancers gates CreateLoadBalancer on the account's live count.
// ALBs and NLBs share one cap, as both consume a load-balancer appliance VM.
func (s *Service) EnforceLoadBalancers(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := gateway_elbv2.DescribeLoadBalancers(ctx, &elbv2.DescribeLoadBalancersInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.LoadBalancers), want, s.limitsFor(ctx, accountID).LoadBalancers)
}

// EnforceRDSInstances gates CreateDBInstance on the account's live count. A DB
// instance is capped by count rather than by vCPU because its VM runs in the
// system account and never reaches the tenant's vCPU counter.
func (s *Service) EnforceRDSInstances(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := handlers_rds.NewNATSService(natsConn).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{}, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.DBInstances), want, s.limitsFor(ctx, accountID).RDSInstances)
}
