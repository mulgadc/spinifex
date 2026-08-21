package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// The appliance's reachable address is the customer-ENI IP RDS places in the
// system account's default VPC, an OVN logical switch on br-int. The daemon
// host has no route to it, so a healthy appliance is otherwise never
// reachable -- the same host-to-guest gap the Bedrock fix closed for
// InvokeModel.
const (
	ochreDaemonPortRoleTagKey = "spinifex:ochrevector-role"
	ochreDaemonPortRole       = "daemon-port"
	ochreDaemonNodeTagKey     = "spinifex:ochrevector-node"
)

// hostPortPlumber installs and removes the host-side port backing the
// daemon's own ENI. Narrowed to what this package uses so it does not import
// the vm or network/host packages.
type hostPortPlumber interface {
	EnsureVPCHostPort(eniID, mac, addr string) error
	RemoveVPCHostPort(eniID string) error
}

// vpcProvisioner is the narrow EC2 surface ensureDaemonENI and subnet
// resolution need: mint-or-describe the daemon's own ENI, and list the
// subnets in the appliance's VPC.
type vpcProvisioner interface {
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
}

// HostPortDeps bundles the collaborators the daemon's appliance-VPC host
// port needs. A zero HostPortDeps (HostPort nil) is a valid "do no
// host-port work" configuration, so Appliance instances that never opt in
// (every unit test, or a build with the fix not wired up) are unaffected.
type HostPortDeps struct {
	VPC      vpcProvisioner
	HostPort hostPortPlumber
	// NodeID names the daemon replica, so each node's appliance-VPC port is
	// a distinct ENI rather than one they contend for.
	NodeID string
}

// daemonENI is the daemon's own network interface in the appliance's VPC,
// unattached to any VM: it exists so ovn-controller has a logical switch
// port to bind the host's internal port to.
type daemonENI struct {
	id  string
	ip  string
	mac string
}

// daemonPortDescription is what idempotence keys off. ENIs carry no tag
// filter in DescribeNetworkInterfaces, but description is filterable, so the
// node's identity goes there and the tags are for operator visibility only.
func daemonPortDescription(nodeID string) string {
	return "Ochre vector daemon port for node " + nodeID
}

// ensureDaemonENI describe-or-creates this node's ENI in subnetID. Mirrors
// handlers/bedrock's helper of the same shape: description-filtered
// describe first, create only on a miss, so a restart reuses the same
// address rather than leaking one per launch.
func ensureDaemonENI(ctx context.Context, vpcSvc vpcProvisioner, subnetID, nodeID string) (*daemonENI, error) {
	desc := daemonPortDescription(nodeID)
	out, err := vpcSvc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("subnet-id"), Values: aws.StringSlice([]string{subnetID})},
			{Name: aws.String("description"), Values: aws.StringSlice([]string{desc})},
		},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: describe daemon ENI in subnet %s: %w", subnetID, err)
	}
	if out != nil {
		for _, ni := range out.NetworkInterfaces {
			eni := &daemonENI{
				id:  aws.StringValue(ni.NetworkInterfaceId),
				ip:  aws.StringValue(ni.PrivateIpAddress),
				mac: aws.StringValue(ni.MacAddress),
			}
			// A record missing either field cannot back a host port, so fall
			// through and mint a usable one rather than fail the connect.
			if eni.id != "" && eni.ip != "" && eni.mac != "" {
				return eni, nil
			}
			slog.WarnContext(ctx, "ochrevector: ignoring incomplete daemon ENI record",
				"node", nodeID, "eni", eni.id)
		}
	}

	created, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(desc),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(ochreDaemonPortRoleTagKey), Value: aws.String(ochreDaemonPortRole)},
				{Key: aws.String(ochreDaemonNodeTagKey), Value: aws.String(nodeID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("ochrevector: create daemon ENI in subnet %s: %w", subnetID, err)
	}
	if created == nil || created.NetworkInterface == nil {
		return nil, fmt.Errorf("ochrevector: create daemon ENI in subnet %s: no interface returned", subnetID)
	}
	ni := created.NetworkInterface
	eni := &daemonENI{
		id:  aws.StringValue(ni.NetworkInterfaceId),
		ip:  aws.StringValue(ni.PrivateIpAddress),
		mac: aws.StringValue(ni.MacAddress),
	}
	if eni.id == "" || eni.ip == "" || eni.mac == "" {
		return nil, fmt.Errorf("ochrevector: create daemon ENI in subnet %s: incomplete interface returned", subnetID)
	}
	return eni, nil
}

// applianceInstanceTagKey mirrors the unexported rdsInstanceTagKey in
// handlers/rds/launch.go, which stamps it on the appliance's customer ENI at
// launch. Keep this literal in sync with that const; the e2e harness
// (tests/e2e/harness/rds.go) duplicates it the same way.
const applianceInstanceTagKey = "spinifex:rds-db-instance"

// endpointENIDescriptionPrefix mirrors the description handlers/rds gives the
// customer (endpoint) ENI in resolveCustomerENI. The management NIC shares the
// rds-db-instance tag, so description is what tells the reachable ENI apart.
const endpointENIDescriptionPrefix = "RDS endpoint ENI for "

// resolveApplianceTarget finds the appliance's own customer ENI by the tag
// handlers/rds stamps on it at launch, returning the real private IP to dial
// and the subnet it lives in -- never the vanity endpoint hostname, which is
// unresolvable and unroutable from the daemon host.
func resolveApplianceTarget(ctx context.Context, deps HostPortDeps, identifier string) (dialIP, subnetID, subnetCIDR string, err error) {
	if deps.VPC == nil {
		return "", "", "", errors.New("ochrevector: no VPC provider configured; cannot resolve the appliance's ENI")
	}

	out, err := deps.VPC.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + applianceInstanceTagKey), Values: aws.StringSlice([]string{identifier})},
		},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", "", "", fmt.Errorf("ochrevector: describe appliance ENI for %s: %w", identifier, err)
	}
	// Both the endpoint ENI and the management NIC carry the rds-db-instance
	// tag, but only the endpoint ENI (in the customer VPC) is reachable from the
	// daemon. Prefer it by description; fall back to any usable tagged NIC.
	var ni *ec2.NetworkInterface
	if out != nil {
		for _, cand := range out.NetworkInterfaces {
			if aws.StringValue(cand.PrivateIpAddress) == "" || aws.StringValue(cand.SubnetId) == "" {
				continue
			}
			if strings.HasPrefix(aws.StringValue(cand.Description), endpointENIDescriptionPrefix) {
				ni = cand
				break
			}
			if ni == nil {
				ni = cand
			}
		}
	}
	if ni == nil {
		return "", "", "", fmt.Errorf("ochrevector: no customer ENI found for appliance %s", identifier)
	}
	dialIP = aws.StringValue(ni.PrivateIpAddress)
	subnetID = aws.StringValue(ni.SubnetId)

	subOut, err := deps.VPC.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("subnet-id"), Values: aws.StringSlice([]string{subnetID})}},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", "", "", fmt.Errorf("ochrevector: describe subnet %s: %w", subnetID, err)
	}
	if subOut == nil || len(subOut.Subnets) == 0 || aws.StringValue(subOut.Subnets[0].CidrBlock) == "" {
		return "", "", "", fmt.Errorf("ochrevector: subnet %s has no cidr block", subnetID)
	}
	subnetCIDR = aws.StringValue(subOut.Subnets[0].CidrBlock)
	return dialIP, subnetID, subnetCIDR, nil
}

// ensureApplianceHostPort gives this daemon a routed presence in the subnet
// the platform appliance's real ENI lives in, so that address (not the
// unroutable vanity hostname) becomes reachable. One ENI per node, not per
// connect: describe-or-create means a restart reuses the same address
// rather than leaking one per launch. Returns the appliance's dial IP and
// the daemon's own ENI id, so the caller can build the DSN and later remove
// the port.
func ensureApplianceHostPort(ctx context.Context, deps HostPortDeps, identifier string) (dialIP, eniID string, err error) {
	if deps.HostPort == nil {
		return "", "", errors.New("ochrevector: no host-port plumber configured; the daemon cannot reach the platform appliance")
	}
	if deps.NodeID == "" {
		return "", "", errors.New("ochrevector: no node id; cannot identify this daemon's appliance-VPC port")
	}
	if deps.VPC == nil {
		return "", "", errors.New("ochrevector: no VPC provider configured; cannot mint the daemon's appliance-VPC port")
	}

	dialIP, subnetID, subnetCIDR, err := resolveApplianceTarget(ctx, deps, identifier)
	if err != nil {
		return "", "", err
	}
	subnet, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return "", "", fmt.Errorf("ochrevector: parse appliance subnet CIDR %q: %w", subnetCIDR, err)
	}

	eni, err := ensureDaemonENI(ctx, deps.VPC, subnetID, deps.NodeID)
	if err != nil {
		return "", "", err
	}
	ip, err := netip.ParseAddr(eni.ip)
	if err != nil {
		return "", "", fmt.Errorf("ochrevector: daemon ENI %s has unparseable address %q: %w", eni.id, eni.ip, err)
	}

	// The ENI's own address at the SUBNET's prefix length. A /32 would
	// address the port and still leave the appliance unreachable; this
	// prefix is what installs the connected route to it.
	addr := netip.PrefixFrom(ip, subnet.Bits())
	if err := deps.HostPort.EnsureVPCHostPort(eni.id, eni.mac, addr.String()); err != nil {
		return "", "", fmt.Errorf("ochrevector: install daemon host port for ENI %s: %w", eni.id, err)
	}
	slog.InfoContext(ctx, "ochrevector: daemon appliance-VPC port ready",
		"node", deps.NodeID, "eni", eni.id, "addr", addr)
	return dialIP, eni.id, nil
}

// removeApplianceHostPort tears down the port ensureApplianceHostPort
// installed. A no-op when there is nothing to remove, so it is safe to call
// on a daemon that never reached a successful ensure.
func removeApplianceHostPort(deps HostPortDeps, eniID string) error {
	if deps.HostPort == nil || eniID == "" {
		return nil
	}
	if err := deps.HostPort.RemoveVPCHostPort(eniID); err != nil {
		return fmt.Errorf("ochrevector: remove daemon host port for ENI %s: %w", eniID, err)
	}
	return nil
}
