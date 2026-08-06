package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// The system-VPC subnet lives inside OVN on br-int, so the daemon host has no
// route to it and cannot dial a serving VM at all. This is not only a readiness
// concern: InvokeModel is proxied from inside the daemon too, so the same hop
// carries real inference traffic.
const (
	bedrockDaemonPortRole   = "daemon-port"
	bedrockDaemonNodeTagKey = "spinifex:bedrock-node"
)

// hostPortPlumber installs the host-side port backing the daemon's own ENI.
// Narrowed to what this package uses so it does not import the vm package.
type hostPortPlumber interface {
	EnsureVPCHostPort(eniID, mac, addr string) error
}

// daemonPortDescription is what idempotence keys off. ENIs carry no tag filter
// in DescribeNetworkInterfaces, but description is filterable, so the node's
// identity goes there and the tags are for operator visibility only.
func daemonPortDescription(nodeID string) string {
	return "Bedrock daemon port for node " + nodeID
}

// EnsureDaemonPort gives this daemon a routed presence in the Bedrock system
// VPC, so BaseURL's private address becomes reachable. One ENI per node, not
// per endpoint: describe-or-create means a restart or a second launch reuses
// the same address rather than leaking one per launch.
//
// Called after the system VPC is ensured and before the serving VM launches.
// Both halves are idempotent, so a host reboot is re-driven by the next launch
// without any separate recovery path.
func EnsureDaemonPort(ctx context.Context, deps LaunchDeps, subnetID, subnetCIDR string) error {
	if deps.HostPort == nil {
		return errors.New("bedrock: no host-port plumber configured; the daemon cannot reach a serving VM")
	}
	if deps.NodeID == "" {
		return errors.New("bedrock: no node id; cannot identify this daemon's system-VPC port")
	}
	subnet, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return fmt.Errorf("bedrock: parse system subnet CIDR %q: %w", subnetCIDR, err)
	}

	eni, err := ensureDaemonENI(ctx, deps.VPC, subnetID, deps.NodeID)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(eni.ip)
	if err != nil {
		return fmt.Errorf("bedrock: daemon ENI %s has unparseable address %q: %w", eni.id, eni.ip, err)
	}

	// The ENI's own address at the SUBNET's prefix length. A /32 would address
	// the port and still leave every serving VM unreachable; this prefix is what
	// installs the connected route that makes them reachable.
	addr := netip.PrefixFrom(ip, subnet.Bits())
	if err := deps.HostPort.EnsureVPCHostPort(eni.id, eni.mac, addr.String()); err != nil {
		return fmt.Errorf("bedrock: install daemon host port for ENI %s: %w", eni.id, err)
	}
	slog.InfoContext(ctx, "bedrock: daemon system-VPC port ready",
		"node", deps.NodeID, "eni", eni.id, "addr", addr)
	return nil
}

// ensureDaemonENI describe-or-creates this node's ENI in the system subnet.
// Unattached to any VM: it exists so ovn-controller has a logical switch port
// to bind the host's internal port to.
func ensureDaemonENI(ctx context.Context, vpcSvc launchVPCProvisioner, subnetID, nodeID string) (*launchENI, error) {
	desc := daemonPortDescription(nodeID)
	out, err := vpcSvc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("subnet-id"), Values: aws.StringSlice([]string{subnetID})},
			{Name: aws.String("description"), Values: aws.StringSlice([]string{desc})},
		},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("bedrock: describe daemon ENI in subnet %s: %w", subnetID, err)
	}
	if out != nil {
		for _, ni := range out.NetworkInterfaces {
			eni := &launchENI{
				id:  aws.StringValue(ni.NetworkInterfaceId),
				ip:  aws.StringValue(ni.PrivateIpAddress),
				mac: aws.StringValue(ni.MacAddress),
			}
			// A record missing either field cannot back a host port, so fall
			// through and mint a usable one rather than fail the launch.
			if eni.id != "" && eni.ip != "" && eni.mac != "" {
				return eni, nil
			}
			slog.WarnContext(ctx, "bedrock: ignoring incomplete daemon ENI record",
				"node", nodeID, "eni", eni.id)
		}
	}

	created, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(desc),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByBedrock)},
				{Key: aws.String(bedrockSystemVPCRoleTagKey), Value: aws.String(bedrockDaemonPortRole)},
				{Key: aws.String(bedrockDaemonNodeTagKey), Value: aws.String(nodeID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("bedrock: create daemon ENI in subnet %s: %w", subnetID, err)
	}
	if created == nil || created.NetworkInterface == nil {
		return nil, fmt.Errorf("bedrock: create daemon ENI in subnet %s: no interface returned", subnetID)
	}
	ni := created.NetworkInterface
	eni := &launchENI{
		id:  aws.StringValue(ni.NetworkInterfaceId),
		ip:  aws.StringValue(ni.PrivateIpAddress),
		mac: aws.StringValue(ni.MacAddress),
	}
	if eni.id == "" || eni.ip == "" || eni.mac == "" {
		return nil, fmt.Errorf("bedrock: create daemon ENI in subnet %s: incomplete interface returned", subnetID)
	}
	return eni, nil
}
