package handlers_elbv2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// nlbManagedSGName returns the deterministic name of the internal security
// group minted for an NLB. Stable so recovery paths can re-resolve it.
func nlbManagedSGName(lbID string) string {
	return "spinifex-nlb-" + lbID
}

// lbENIGroups returns the SGs to attach to an LB's data-plane ENIs: customer SGs
// when present (ALBs always, NLBs that were created with SGs), else the managed SG
// for an NLB created without SGs. Shared by the create-time ENI loop and SetSubnets
// relaunch so both paths attach the same SG.
func lbENIGroups(lb *LoadBalancerRecord) []string {
	if lb.Type == LoadBalancerTypeNetwork {
		if len(lb.SecurityGroups) > 0 {
			return lb.SecurityGroups
		}
		if lb.NLBManagedSGID != "" {
			return []string{lb.NLBManagedSGID}
		}
		return nil
	}
	return lb.SecurityGroups
}

// createNLBManagedSG mints the managed front-end SG for an NLB; the VPC is resolved
// from the first subnet. The SG is tagged like other managed resources so the
// ownership sweep can find it.
func (s *ELBv2ServiceImpl) createNLBManagedSG(ctx context.Context, lbID, lbArn, subnetID, accountID string) (string, error) {
	subnet, err := s.VPCService.GetSubnet(accountID, subnetID)
	if err != nil {
		return "", fmt.Errorf("resolve subnet %s vpc: %w", subnetID, err)
	}
	if subnet == nil || subnet.VpcId == "" {
		return "", fmt.Errorf("subnet %s has no vpc", subnetID)
	}

	out, err := s.VPCService.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(nlbManagedSGName(lbID)),
		Description: aws.String(fmt.Sprintf("Managed front-end SG for NLB %s", lbID)),
		VpcId:       aws.String(subnet.VpcId),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("security-group"),
			Tags: []*ec2.Tag{
				{Key: aws.String(elbv2ManagedByTag), Value: aws.String(elbv2ManagedByValue)},
				{Key: aws.String(elbv2LBTag), Value: aws.String(lbArn)},
			},
		}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("create NLB SG: %w", err)
	}
	if out == nil || out.GroupId == nil || *out.GroupId == "" {
		return "", errors.New("CreateSecurityGroup returned empty GroupId")
	}
	return *out.GroupId, nil
}

// deleteNLBManagedSG best-effort deletes an NLB's managed SG; nil VPC service or
// empty ID is a no-op. Failures are logged but not returned — the SG can only
// be deleted after its ENIs are gone, which the delete path ensures.
func (s *ELBv2ServiceImpl) deleteNLBManagedSG(ctx context.Context, sgID, accountID string) {
	if s.VPCService == nil || sgID == "" {
		return
	}
	if _, err := s.VPCService.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, accountID); err != nil && !awserrors.IsNotFound(err) {
		slog.WarnContext(ctx, "deleteNLBManagedSG: delete failed", "sg", sgID, "err", err)
	}
}

// resolveNLBIngressCIDRs returns the client CIDRs a listener port is opened to.
// An explicit override wins; otherwise the default is scheme-based:
// internet-facing → 0.0.0.0/0, internal → the LB's VPC CIDR.
func (s *ELBv2ServiceImpl) resolveNLBIngressCIDRs(ctx context.Context, lb *LoadBalancerRecord) ([]string, error) {
	if len(lb.NLBIngressCIDRs) > 0 {
		return lb.NLBIngressCIDRs, nil
	}
	if lb.Scheme == SchemeInternal {
		cidr, err := s.vpcCIDR(ctx, lb.VpcId, lb.AccountID)
		if err != nil {
			return nil, err
		}
		return []string{cidr}, nil
	}
	return []string{"0.0.0.0/0"}, nil
}

// vpcCIDR returns the primary CIDR block of a VPC.
func (s *ELBv2ServiceImpl) vpcCIDR(ctx context.Context, vpcID, accountID string) (string, error) {
	if s.VPCService == nil {
		return "", errors.New("vpc service unavailable")
	}
	if vpcID == "" {
		return "", errors.New("empty vpc id")
	}
	out, err := s.VPCService.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: aws.StringSlice([]string{vpcID}),
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("describe vpc %s: %w", vpcID, err)
	}
	for _, v := range out.Vpcs {
		if v != nil && aws.StringValue(v.CidrBlock) != "" {
			return *v.CidrBlock, nil
		}
	}
	return "", fmt.Errorf("vpc %s has no cidr", vpcID)
}

// listenerIPProtocols maps an ELBv2 listener protocol to the IP protocols its
// SG ingress rule(s) cover. TCP/TLS ride tcp; UDP rides udp; TCP_UDP opens both.
func listenerIPProtocols(protocol string) []string {
	switch protocol {
	case ProtocolUDP:
		return []string{"udp"}
	case ProtocolTCPUDP:
		return []string{"tcp", "udp"}
	default: // TCP, TLS
		return []string{"tcp"}
	}
}

// authorizeNLBListenerPort opens a listener port on the NLB's managed SG for each
// (protocol, CIDR) pair; each rule is authorized separately so a Duplicate error
// never blocks a sibling rule. No-op when there is no managed SG.
func (s *ELBv2ServiceImpl) authorizeNLBListenerPort(ctx context.Context, lb *LoadBalancerRecord, protocol string, port int64, cidrs []string, accountID string) error {
	if s.VPCService == nil || lb.NLBManagedSGID == "" {
		return nil
	}
	for _, proto := range listenerIPProtocols(protocol) {
		for _, cidr := range cidrs {
			_, err := s.VPCService.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
				GroupId: aws.String(lb.NLBManagedSGID),
				IpPermissions: []*ec2.IpPermission{{
					IpProtocol: aws.String(proto),
					FromPort:   aws.Int64(port),
					ToPort:     aws.Int64(port),
					IpRanges: []*ec2.IpRange{{
						CidrIp:      aws.String(cidr),
						Description: aws.String(fmt.Sprintf("NLB %s listener :%d", lb.LoadBalancerID, port)),
					}},
				}},
			}, accountID)
			if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
				return fmt.Errorf("authorize :%d/%s from %s: %w", port, proto, cidr, err)
			}
		}
	}
	return nil
}

// revokeNLBListenerPort removes the listener-port rules authorizeNLBListenerPort
// added. An absent rule (NotFound, swallowed) is fine so delete / re-CIDR paths
// stay idempotent. No-op when there is no managed SG.
func (s *ELBv2ServiceImpl) revokeNLBListenerPort(ctx context.Context, lb *LoadBalancerRecord, protocol string, port int64, cidrs []string, accountID string) error {
	if s.VPCService == nil || lb.NLBManagedSGID == "" {
		return nil
	}
	for _, proto := range listenerIPProtocols(protocol) {
		for _, cidr := range cidrs {
			_, err := s.VPCService.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(lb.NLBManagedSGID),
				IpPermissions: []*ec2.IpPermission{{
					IpProtocol: aws.String(proto),
					FromPort:   aws.Int64(port),
					ToPort:     aws.Int64(port),
					IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(cidr)}},
				}},
			}, accountID)
			if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionNotFound) {
				return fmt.Errorf("revoke :%d/%s from %s: %w", port, proto, cidr, err)
			}
		}
	}
	return nil
}

// SetLoadBalancerIngressCIDRs overrides the client CIDRs for an NLB's listener ports,
// revoking old rules and authorizing new ones. Passing nil/empty reverts to the
// scheme-based default (0.0.0.0/0 internet-facing, VPC CIDR internal).
func (s *ELBv2ServiceImpl) SetLoadBalancerIngressCIDRs(lbArn string, cidrs []string, accountID string) error {
	if lbArn == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	lb, err := s.store.GetLoadBalancerByArn(lbArn)
	if err != nil {
		slog.Error("SetLoadBalancerIngressCIDRs: failed to get LB", "arn", lbArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil {
		return errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}
	if lb.Type != LoadBalancerTypeNetwork {
		return errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
	}
	for _, c := range cidrs {
		if _, _, perr := net.ParseCIDR(c); perr != nil {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	oldCIDRs, err := s.resolveNLBIngressCIDRs(context.Background(), lb)
	if err != nil {
		slog.Error("SetLoadBalancerIngressCIDRs: resolve old CIDRs failed", "arn", lbArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	listeners, err := s.store.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		slog.Error("SetLoadBalancerIngressCIDRs: list listeners failed", "arn", lbArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	lb.NLBIngressCIDRs = cidrs
	newCIDRs, err := s.resolveNLBIngressCIDRs(context.Background(), lb)
	if err != nil {
		slog.Error("SetLoadBalancerIngressCIDRs: resolve new CIDRs failed", "arn", lbArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	for _, l := range listeners {
		if err := s.revokeNLBListenerPort(context.Background(), lb, l.Protocol, l.Port, oldCIDRs, accountID); err != nil {
			slog.Error("SetLoadBalancerIngressCIDRs: revoke failed", "arn", lbArn, "port", l.Port, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if err := s.authorizeNLBListenerPort(context.Background(), lb, l.Protocol, l.Port, newCIDRs, accountID); err != nil {
			slog.Error("SetLoadBalancerIngressCIDRs: authorize failed", "arn", lbArn, "port", l.Port, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	if err := s.store.PutLoadBalancer(lb); err != nil {
		slog.Error("SetLoadBalancerIngressCIDRs: persist failed", "arn", lbArn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	slog.Info("SetLoadBalancerIngressCIDRs completed", "arn", lbArn, "cidrs", cidrs, "listeners", len(listeners), "accountID", accountID)
	return nil
}
