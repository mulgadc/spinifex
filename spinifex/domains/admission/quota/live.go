package quota

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/rds"
	ec2eipapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/eip"
	ec2vpcapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/vpc"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// exceeds rejects with ResourceLimitExceeded when an account already holding
// count of a live-counted dimension would pass its cap by adding want more. It is
// the shared comparison for every live dimension; the per-dimension methods
// supply count from the relevant Describe* call. It compares against the
// remaining headroom so a huge want cannot overflow count+want past the cap.
func exceeds(count, want, limit int) error {
	if limit == Unlimited {
		return nil
	}
	if want > limit-count {
		return errors.New(awserrors.ErrorResourceLimitExceeded)
	}
	return nil
}

// EnforceVPCs gates CreateVpc on the account's live DescribeVpcs count.
func (s *Service) EnforceVPCs(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := ec2vpcapi.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	limits, err := s.limitsFor(ctx, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Vpcs), want, limits.VPCs)
}

// EnforceSubnets gates CreateSubnet on the account's live DescribeSubnets count.
// Subnets are capped per-account in aggregate, not per-VPC.
func (s *Service) EnforceSubnets(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := ec2vpcapi.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	limits, err := s.limitsFor(ctx, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Subnets), want, limits.Subnets)
}

// EnforceEIPs gates AllocateAddress on the account's live DescribeAddresses count.
func (s *Service) EnforceEIPs(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := ec2eipapi.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	limits, err := s.limitsFor(ctx, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.Addresses), want, limits.EIPs)
}

// EnforceLoadBalancers gates CreateLoadBalancer on the account's live count.
// ALBs and NLBs share one cap, as both consume a load-balancer appliance VM.
func (s *Service) EnforceLoadBalancers(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := elbv2awsapi.DescribeLoadBalancers(ctx, &elbv2.DescribeLoadBalancersInput{}, natsConn, accountID)
	if err != nil {
		return err
	}
	limits, err := s.limitsFor(ctx, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.LoadBalancers), want, limits.LoadBalancers)
}

// EnforceRDSInstances gates CreateDBInstance on the account's live count. A DB
// instance is capped by count rather than by vCPU because its VM runs in the
// system account and never reaches the tenant's vCPU counter.
func (s *Service) EnforceRDSInstances(ctx context.Context, natsConn *nats.Conn, accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	out, err := rdsinstance.NewNATSService(natsConn).DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{}, accountID)
	if err != nil {
		return err
	}
	limits, err := s.limitsFor(ctx, accountID)
	if err != nil {
		return err
	}
	return exceeds(len(out.DBInstances), want, limits.RDSInstances)
}

// LoadBalancerLimit is the load balancer cap DescribeAccountLimits reports.
func (s *Service) LoadBalancerLimit(ctx context.Context, accountID string) (int, error) {
	// An exempt account has no cap enforced against it, so the AWS default is a
	// truer answer than a configured limit nothing consults.
	if s.Exempt(accountID) {
		return elbv2awsapi.DefaultLoadBalancerLimit, nil
	}
	_, limits, err := s.GetAccountQuota(ctx, accountID)
	if err != nil {
		if awserrors.IsErrorCode(err, awserrors.ErrorBlocked) {
			return 0, err
		}
		slog.ErrorContext(ctx, "DescribeAccountLimits: failed to read account quota", "accountID", accountID, "err", err)
		return 0, errors.New(awserrors.ErrorInternalError)
	}
	return limits.LoadBalancers, nil
}

var _ elbv2awsapi.Quota = (*Service)(nil)
