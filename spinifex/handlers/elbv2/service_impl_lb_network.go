package handlers_elbv2

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// SetIpAddressType sets the load balancer's IP address type. Spinifex ALBs are
// IPv4-only, so the only accepted value is "ipv4"; dualstack variants are
// rejected with InvalidConfigurationRequest. The call is idempotent.
func (s *ELBv2ServiceImpl) SetIpAddressType(input *elbv2.SetIpAddressTypeInput, accountID string) (*elbv2.SetIpAddressTypeOutput, error) {
	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.IpAddressType == nil || *input.IpAddressType == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if *input.IpAddressType != IPAddressTypeIPv4 {
		return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("SetIpAddressType: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	if lb.IPAddressType != IPAddressTypeIPv4 {
		lb.IPAddressType = IPAddressTypeIPv4
		if err := s.store.PutLoadBalancer(lb); err != nil {
			slog.Error("SetIpAddressType: failed to persist LB", "arn", *input.LoadBalancerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.SetIpAddressTypeOutput{
		IpAddressType: aws.String(lb.IPAddressType),
	}, nil
}

// SetSecurityGroups replaces the security groups associated with an
// (application) load balancer. The new groups are re-attached to every ENI the
// ALB spans via ModifyNetworkInterfaceAttribute, which validates them against
// the ENI's VPC and pushes the change to the live data-plane port before the
// record is persisted.
func (s *ELBv2ServiceImpl) SetSecurityGroups(input *elbv2.SetSecurityGroupsInput, accountID string) (*elbv2.SetSecurityGroupsOutput, error) {
	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.SecurityGroups) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("SetSecurityGroups: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	// NLBs do not support security groups (mirrors CreateLoadBalancer).
	if lb.Type == LoadBalancerTypeNetwork {
		return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
	}

	sgs := make([]string, 0, len(input.SecurityGroups))
	for _, sg := range input.SecurityGroups {
		if sg == nil || *sg == "" {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		sgs = append(sgs, *sg)
	}

	// Re-attach the new groups to each ALB ENI. This validates the groups
	// against the ENI's VPC and fires the live port-SG update; a failure here
	// (e.g. unknown SG) aborts before the record is persisted. All ENIs share
	// the LB's VPC and groups, so a successful first apply implies the rest.
	if s.VPCService != nil {
		for _, eniID := range lb.ENIs {
			if _, err := s.VPCService.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
				NetworkInterfaceId: aws.String(eniID),
				Groups:             aws.StringSlice(sgs),
			}, accountID); err != nil {
				slog.Error("SetSecurityGroups: failed to update ENI groups", "arn", *input.LoadBalancerArn, "eni", eniID, "err", err)
				return nil, err
			}
		}
	}

	lb.SecurityGroups = sgs
	if err := s.store.PutLoadBalancer(lb); err != nil {
		slog.Error("SetSecurityGroups: failed to persist LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &elbv2.SetSecurityGroupsOutput{
		SecurityGroupIds: aws.StringSlice(sgs),
	}, nil
}
