package elbv2

import (
	"context"
	"errors"
	"fmt"
)

// errOVNVIPRefused means the owner or parent evidence does not admit a
// private address reservation for the load balancer, so none was attempted.
var errOVNVIPRefused = errors.New("elbv2: load balancer address reservation refused")

// reserveOVNFixtureVIP reserves, or recovers, the one private address of the
// sealed network load balancer arn in the caller's region and account. It reads
// fresh owner, slot and parent evidence first and writes only the IPAM entry.
func (s *ELBv2ServiceImpl) reserveOVNFixtureVIP(ctx context.Context, arn, accountID string) (string, error) {
	if s.ovn == nil || s.ovn.vips == nil {
		return "", fmt.Errorf("%w: no address allocator", errOVNVIPRefused)
	}
	root, err := s.ovn.owners.GetSealedByArn(ctx, arn, s.region, accountID)
	if err != nil {
		return "", fmt.Errorf("read owner of %s: %w", arn, err)
	}
	if root == nil {
		return "", fmt.Errorf("%w: no sealed load balancer %s", errOVNVIPRefused, arn)
	}
	if root.VpcId == "" || len(root.Subnets) != 1 || root.Subnets[0] == "" {
		return "", fmt.Errorf("%w: load balancer %s needs one VPC and one subnet", errOVNVIPRefused, arn)
	}
	subnetID := root.Subnets[0]
	if err := s.ovn.owners.requireOwnerSlots(ctx, root); err != nil {
		return "", err
	}
	cidr, err := s.ovn.parentCIDR(ctx, accountID, root.VpcId, subnetID)
	if err != nil {
		return "", err
	}
	return s.ovn.vips.ReserveVIP(ctx, subnetID, cidr, root.LoadBalancerID)
}

// requireOwnerSlots confirms by leader reads that both of the account's VPC and
// subnet slots name exactly this sealed owner and its parent pair.
func (f *fixtureOwnerStore) requireOwnerSlots(ctx context.Context, root *LoadBalancerRecord) error {
	want := fixtureSlot{ClaimKey: LBNameKey(root.Name, root.AccountID), OwnerID: root.LoadBalancerID, VpcID: root.VpcId, SubnetID: root.Subnets[0]}
	for _, key := range want.keys(root.AccountID) {
		_, slot, err := f.readSlot(ctx, root.AccountID, key)
		if err != nil {
			return fmt.Errorf("read slot %s: %w", key, err)
		}
		if slot == nil || *slot != want {
			return fmt.Errorf("%w: slot %s does not hold load balancer %s", errOVNVIPRefused, key, root.LoadBalancerArn)
		}
	}
	return nil
}

// parentCIDR returns the subnet's stored CIDR once the VPC and the subnet both
// exist, are not being deleted and are the pair the owner names.
func (o *ovnFixture) parentCIDR(ctx context.Context, accountID, vpcID, subnetID string) (string, error) {
	vpc, _, err := o.parents.VPC(ctx, accountID, vpcID)
	if err != nil {
		return "", fmt.Errorf("read VPC %s: %w", vpcID, err)
	}
	if vpc == nil || vpc.Deleting || vpc.VpcId != vpcID {
		return "", fmt.Errorf("%w: VPC %s is absent or being deleted", errOVNVIPRefused, vpcID)
	}
	subnet, _, err := o.parents.Subnet(ctx, accountID, subnetID)
	if err != nil {
		return "", fmt.Errorf("read subnet %s: %w", subnetID, err)
	}
	if subnet == nil || subnet.Deleting || subnet.SubnetId != subnetID || subnet.VpcId != vpcID {
		return "", fmt.Errorf("%w: subnet %s is absent, being deleted or outside VPC %s", errOVNVIPRefused, subnetID, vpcID)
	}
	return subnet.CidrBlock, nil
}
