package handlers_elbv2

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// SetIpAddressType sets the LB IP address type; only "ipv4" is accepted.
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

// SetSecurityGroups replaces the security groups on an ALB, re-attaching them
// to every ENI via ModifyNetworkInterfaceAttribute before persisting.
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

	// Re-attach to each ENI; failure aborts before the record is persisted.
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

// SetSubnets does a full add+remove of the LB's subnets and their ENIs.
// Because ENI hotplug is not supported, the LB VM is relaunched with the new ENI set.
func (s *ELBv2ServiceImpl) SetSubnets(input *elbv2.SetSubnetsInput, accountID string) (*elbv2.SetSubnetsOutput, error) {
	if input == nil || input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	desired := desiredSubnetSet(input)
	if len(desired) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("SetSubnets: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	current := subnetENIMap(lb)
	desiredSet := make(map[string]bool, len(desired))
	for _, sn := range desired {
		desiredSet[sn] = true
	}

	var toAdd, toRemove []string
	for _, sn := range desired {
		if _, ok := current[sn]; !ok {
			toAdd = append(toAdd, sn)
		}
	}
	for sn := range current {
		if !desiredSet[sn] {
			toRemove = append(toRemove, sn)
		}
	}

	if len(toAdd) == 0 && len(toRemove) == 0 {
		return s.setSubnetsOutput(lb), nil // idempotent — no change
	}

	// No VPC service: just record the subnet set (launcher-less / test deployments).
	if s.VPCService == nil {
		lb.Subnets = desired
		lb.AvailZones = rebuildAvailZones(desired, lb.AvailZones, nil)
		if err := s.store.PutLoadBalancer(lb); err != nil {
			slog.Error("SetSubnets: failed to persist LB", "arn", *input.LoadBalancerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		return s.setSubnetsOutput(lb), nil
	}

	// Create ENIs for added subnets; roll back on failure to avoid leaks.
	newENIBySubnet := make(map[string]string, len(toAdd))
	newAZBySubnet := make(map[string]string, len(toAdd))
	rollbackNewENIs := func() {
		for _, created := range newENIBySubnet {
			if _, delErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(created),
			}, accountID); delErr != nil {
				slog.Error("SetSubnets: rollback failed to delete ENI", "eni", created, "err", delErr)
			}
		}
	}
	for _, subnetID := range toAdd {
		eniID, az, eniErr := s.createLBENI(subnetID, lb, accountID)
		if eniErr != nil {
			rollbackNewENIs()
			slog.Error("SetSubnets: failed to create ENI", "subnet", subnetID, "err", eniErr)
			return nil, errors.New(awserrors.ErrorELBv2SubnetNotFound)
		}
		newENIBySubnet[subnetID] = eniID
		newAZBySubnet[subnetID] = az
	}

	// Assemble the new ENI set in desired-subnet order (primary = first subnet).
	newENIs := make([]string, 0, len(desired))
	for _, sn := range desired {
		if eniID, ok := current[sn]; ok {
			newENIs = append(newENIs, eniID)
		} else {
			newENIs = append(newENIs, newENIBySubnet[sn])
		}
	}

	// Terminate the LB VM before reshaping its ENI set, then relaunch on the new set.
	if lb.InstanceID != "" && s.InstanceLauncher != nil {
		if err := s.InstanceLauncher.TerminateSystemInstance(lb.InstanceID); err != nil {
			rollbackNewENIs()
			slog.Error("SetSubnets: failed to terminate LB VM for relaunch", "arn", *input.LoadBalancerArn, "instanceId", lb.InstanceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Detach all ENIs explicitly: TerminateSystemInstance doesn't clear in-use status.
	for _, eniID := range current {
		if detachErr := s.VPCService.DetachENI(accountID, eniID); detachErr != nil {
			slog.Warn("SetSubnets: failed to detach ENI before relaunch", "eni", eniID, "err", detachErr)
		}
	}

	// Delete ENIs for removed subnets now that they are detached.
	for _, sn := range toRemove {
		eniID := current[sn]
		if _, delErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: aws.String(eniID),
		}, accountID); delErr != nil {
			slog.Error("SetSubnets: failed to delete removed ENI", "subnet", sn, "eni", eniID, "err", delErr)
		}
	}

	launch := s.launchLBVM(lb.LoadBalancerID, lb.Scheme, newENIs, desired, accountID)
	availZones := rebuildAvailZones(desired, lb.AvailZones, newAZBySubnet)
	if launch.publicIP != "" && len(availZones) > 0 {
		availZones[0].PublicIP = launch.publicIP
	}

	lb.Subnets = desired
	lb.ENIs = newENIs
	lb.AvailZones = availZones
	lb.InstanceID = launch.instanceID
	lb.VPCIP = launch.vpcIP
	lb.HostPorts = launch.hostPorts
	lb.State = s.lbStateAfterLaunch(launch, lb.Scheme)

	if err := s.store.PutLoadBalancer(lb); err != nil {
		slog.Error("SetSubnets: failed to persist LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("SetSubnets completed", "arn", *input.LoadBalancerArn, "subnets", len(desired), "added", len(toAdd), "removed", len(toRemove), "state", lb.State)
	return s.setSubnetsOutput(lb), nil
}

// desiredSubnetSet flattens Subnets and SubnetMappings into a deduplicated, ordered list.
func desiredSubnetSet(input *elbv2.SetSubnetsInput) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(sn string) {
		if sn != "" && !seen[sn] {
			seen[sn] = true
			out = append(out, sn)
		}
	}
	for _, sn := range input.Subnets {
		if sn != nil {
			add(*sn)
		}
	}
	for _, m := range input.SubnetMappings {
		if m != nil && m.SubnetId != nil {
			add(*m.SubnetId)
		}
	}
	return out
}

// subnetENIMap pairs each current subnet with its ENI using the parallel Subnets/ENIs arrays.
func subnetENIMap(lb *LoadBalancerRecord) map[string]string {
	m := make(map[string]string, len(lb.Subnets))
	for i, sn := range lb.Subnets {
		if i < len(lb.ENIs) {
			m[sn] = lb.ENIs[i]
		}
	}
	return m
}

// createLBENI creates a managed ENI in the given subnet, returning the ENI ID and AZ.
func (s *ELBv2ServiceImpl) createLBENI(subnetID string, lb *LoadBalancerRecord, accountID string) (eniID, az string, err error) {
	eniIn := &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(fmt.Sprintf("ELB %s/%s", lb.Name, lb.LoadBalancerID)),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("network-interface"),
				Tags: []*ec2.Tag{
					{Key: aws.String(elbv2ManagedByTag), Value: aws.String(elbv2ManagedByValue)},
					{Key: aws.String(elbv2LBTag), Value: aws.String(lb.LoadBalancerArn)},
				},
			},
		},
	}
	if groups := lbENIGroups(lb); len(groups) > 0 {
		eniIn.Groups = aws.StringSlice(groups)
	}
	out, err := s.VPCService.CreateNetworkInterface(eniIn, accountID)
	if err != nil {
		return "", "", err
	}
	eni := out.NetworkInterface
	return aws.StringValue(eni.NetworkInterfaceId), aws.StringValue(eni.AvailabilityZone), nil
}

// rebuildAvailZones builds the AZ list for the new subnet set, preserving existing
// zone names and using newAZBySubnet for additions. PublicIP is cleared.
func rebuildAvailZones(subnets []string, existing []AvailZoneInfo, newAZBySubnet map[string]string) []AvailZoneInfo {
	bySubnet := make(map[string]AvailZoneInfo, len(existing))
	for _, az := range existing {
		bySubnet[az.SubnetId] = az
	}
	out := make([]AvailZoneInfo, 0, len(subnets))
	for _, sn := range subnets {
		if az, ok := bySubnet[sn]; ok {
			out = append(out, AvailZoneInfo{ZoneName: az.ZoneName, SubnetId: sn})
			continue
		}
		out = append(out, AvailZoneInfo{ZoneName: newAZBySubnet[sn], SubnetId: sn})
	}
	return out
}

// lbStateAfterLaunch returns the post-launch state: provisioning if the VM came up,
// failed if the launch failed or if an internal LB has no mgmt return route.
func (s *ELBv2ServiceImpl) lbStateAfterLaunch(launch lbVMLaunch, scheme string) string {
	if launch.instanceID == "" {
		if launch.failed {
			return StateFailed
		}
		return StateActive
	}
	if scheme == SchemeInternal {
		if gw, tgt := s.resolveMgmtRoute(scheme); gw == "" || tgt == "" {
			slog.Error("SetSubnets: internal LB has no mgmt return route; marking failed (lb-agent cannot heartbeat AWSGW)",
				"mgmtBridgeIP", s.MgmtBridgeIP, "advertiseIP", s.AdvertiseIP)
			return StateFailed
		}
	}
	return StateProvisioning
}

// setSubnetsOutput builds the SetSubnets response from the persisted record.
func (s *ELBv2ServiceImpl) setSubnetsOutput(lb *LoadBalancerRecord) *elbv2.SetSubnetsOutput {
	sdk := s.lbRecordToSDK(lb)
	return &elbv2.SetSubnetsOutput{
		AvailabilityZones: sdk.AvailabilityZones,
		IpAddressType:     aws.String(lb.IPAddressType),
	}
}
