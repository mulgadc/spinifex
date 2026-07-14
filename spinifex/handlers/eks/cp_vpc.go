package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// The managed control-plane VPC ("Set B") is the spinifex analogue of AWS EKS's
// hidden managed-account VPC: CreateCluster builds it automatically, it is owned
// by the system account (admin.SystemAccountID), and the customer never
// provisions or sees it. The internet-facing NLB lives in its public subnet and
// the k3s control-plane VM(s) in its private subnet(s); a NAT gateway gives the
// private CP egress for image pulls. It is composed from the real EC2
// VPC-family APIs (not direct OVN mutation), so per-subnet egress gating
// (public 1000 reroute / NAT-GW reroute / private 1100 drop) is wired by the
// existing topology subscribers off the route-table events those APIs publish.
const (
	clusterEKSRoleCPVPC         = "cp-vpc"
	clusterEKSRolePublicSubnet  = "cp-public"
	clusterEKSRolePrivateSubnet = "cp-private"
	clusterEKSRolePublicRT      = "cp-public-rt"
	clusterEKSRolePrivateRT     = "cp-private-rt"
	clusterEKSRoleCPNatGW       = "cp-natgw"
)

// cpVPCSupernetSecondOctet anchors the managed CP VPC address space at
// 10.252.0.0/14 (10.252–10.255). Each cluster gets a deterministic /22 carved by
// cpVPCCIDRs: a public /24 + up to three private /24s (HA 3-node etcd spread).
// VPCs are L3-isolated (separate OVN logical routers) and egress via distinct
// per-VPC external IPs, so a hash collision across clusters is non-fatal; a
// proper managed-IPAM allocator is a follow-up.
const cpVPCSupernetSecondOctet = 252

const maxCPPrivateSubnets = 3

// cpVPCPrivateSubnetCount is how many private CP subnets the managed VPC carves.
// One today: the placement rule (CP ⇒ private subnet) is identical for single-
// and multi-node clusters, so all control-plane VMs share the private subnet;
// per-AZ private-subnet spread is a follow-up sizing concern, not a placement
// fork.
const cpVPCPrivateSubnetCount = 1

// managedCPVPCFromRefs projects the resolved refs onto the persisted ClusterMeta
// shape used for teardown.
func managedCPVPCFromRefs(r *ManagedCPVPCRefs) *ManagedCPVPC {
	if r == nil {
		return nil
	}
	return &ManagedCPVPC{
		VpcId:               r.VpcID,
		IGWId:               r.IGWID,
		PublicSubnetId:      r.PublicSubnetID,
		PublicRouteTableId:  r.PublicRouteTableID,
		PrivateSubnetIds:    r.PrivateSubnetIDs,
		PrivateRouteTableId: r.PrivateRouteTableID,
		NatGatewayId:        r.NatGatewayID,
		NatEIPAllocationID:  r.NatEIPAllocationID,
		NatEIPPublicIP:      r.NatEIPPublicIP,
	}
}

// vpcProvisioner is the narrow VPC + subnet surface the managed CP VPC needs.
// The daemon adapts the concrete VPC service onto this.
type vpcProvisioner interface {
	CreateVpc(ctx context.Context, input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error)
	DeleteVpc(ctx context.Context, input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error)
	DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error)
	CreateSubnet(ctx context.Context, input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error)
	DeleteSubnet(ctx context.Context, input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
}

// routeTableProvisioner is the narrow route-table surface the managed CP VPC
// needs to express public (0/0→IGW) and private (0/0→NAT GW) egress.
type routeTableProvisioner interface {
	CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error)
	DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, accountID string) (*ec2.DeleteRouteTableOutput, error)
	DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, accountID string) (*ec2.DescribeRouteTablesOutput, error)
	CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error)
	AssociateRouteTable(ctx context.Context, input *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error)
	DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, accountID string) (*ec2.DisassociateRouteTableOutput, error)
}

// natGatewayProvisioner is the narrow NAT-gateway surface the managed CP VPC
// needs to give the private control-plane subnet egress.
type natGatewayProvisioner interface {
	CreateNatGateway(ctx context.Context, input *ec2.CreateNatGatewayInput, accountID string) (*ec2.CreateNatGatewayOutput, error)
	DeleteNatGateway(ctx context.Context, input *ec2.DeleteNatGatewayInput, accountID string) (*ec2.DeleteNatGatewayOutput, error)
	DescribeNatGateways(ctx context.Context, input *ec2.DescribeNatGatewaysInput, accountID string) (*ec2.DescribeNatGatewaysOutput, error)
}

// CPVPCDeps bundles the EC2-family collaborators EnsureClusterCPVPC composes the
// managed control-plane VPC from. All calls run under the system account.
type CPVPCDeps struct {
	VPC vpcProvisioner
	IGW igwProvisioner
	RT  routeTableProvisioner
	NGW natGatewayProvisioner
	EIP eipProvisioner
	// NATSConn republishes vpc.delete for OVN topology GC (see
	// gcClusterCPVPCTopology). Nil is tolerated (GC is skipped, never blocking).
	NATSConn *nats.Conn
}

// cpVPCDeps adapts the service's EC2-family deps onto CPVPCDeps for the managed
// control-plane VPC build + teardown.
func (s *EKSServiceImpl) cpVPCDeps() CPVPCDeps {
	return CPVPCDeps{
		VPC:      s.deps.VPCMgr,
		IGW:      s.deps.IGW,
		RT:       s.deps.RouteTable,
		NGW:      s.deps.NATGW,
		EIP:      s.deps.EIP,
		NATSConn: s.deps.NATSConn,
	}
}

// ManagedCPVPCRefs is the resolved set of managed CP VPC resource IDs + CIDRs.
// EnsureClusterCPVPC returns it; the caller persists it into ClusterMeta for
// placement (public/private subnet selection) and teardown.
type ManagedCPVPCRefs struct {
	VpcID               string
	VpcCIDR             string
	IGWID               string
	PublicSubnetID      string
	PublicSubnetCIDR    string
	PublicRouteTableID  string
	PrivateSubnetIDs    []string
	PrivateSubnetCIDRs  []string
	PrivateRouteTableID string
	NatGatewayID        string
	NatEIPAllocationID  string
	NatEIPPublicIP      string
}

// cpVPCCIDRs derives the managed CP VPC CIDR + subnet CIDRs deterministically
// from the cluster name. The /22 is carved from 10.252.0.0/14; subnet 0 (.0/24)
// is public, subnets 1..maxCPPrivateSubnets are private.
func cpVPCCIDRs(clusterName string, privateCount int) (vpcCIDR, publicCIDR string, privateCIDRs []string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(clusterName))
	idx := int(h.Sum32() % 256) // 256 /22 blocks in a /14

	combined := idx * 4 // each /22 spans 4 third-octet values
	second := cpVPCSupernetSecondOctet + combined/256
	third := combined % 256

	vpcCIDR = fmt.Sprintf("10.%d.%d.0/22", second, third)
	publicCIDR = fmt.Sprintf("10.%d.%d.0/24", second, third)
	if privateCount < 1 {
		privateCount = 1
	}
	if privateCount > maxCPPrivateSubnets {
		privateCount = maxCPPrivateSubnets
	}
	for k := 0; k < privateCount; k++ {
		privateCIDRs = append(privateCIDRs, fmt.Sprintf("10.%d.%d.0/24", second, third+1+k))
	}
	return vpcCIDR, publicCIDR, privateCIDRs
}

// cpVPCAZ returns the AvailabilityZone name for subnet index k (cosmetic AWS
// parity; spinifex spreads CP by host placement group, not AZ).
func cpVPCAZ(region string, k int) string {
	return fmt.Sprintf("%s%c", region, 'a'+k)
}

func cpVPCTagSpec(resourceType, clusterName, role string) []*ec2.TagSpecification {
	return []*ec2.TagSpecification{{
		ResourceType: aws.String(resourceType),
		Tags: []*ec2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
			{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(role)},
		},
	}}
}

func cpClusterRoleFilters(clusterName, role string) []*ec2.Filter {
	return []*ec2.Filter{
		{Name: aws.String("tag:" + clusterEKSClusterTagKey), Values: aws.StringSlice([]string{clusterName})},
		{Name: aws.String("tag:" + clusterEKSRoleTagKey), Values: aws.StringSlice([]string{role})},
	}
}

// EnsureClusterCPVPC builds (idempotently) the managed control-plane VPC under
// accountID (the system account) for clusterName: a VPC, an attached IGW, a
// public subnet routed to the IGW, privateCount private subnet(s) routed to a
// NAT gateway, and the NAT gateway itself. Each resource is describe-or-create
// keyed on the cluster + role tags so a relaunch after partial failure converges
// rather than duplicating. The route-table associations publish the topology
// events the per-subnet egress subscribers consume, so the OVN policy split
// (public 1000 / NAT-GW reroute / private 1100 drop) falls out without any
// direct OVN mutation here.
func EnsureClusterCPVPC(ctx context.Context, deps CPVPCDeps, accountID, clusterName, region string, privateCount int) (*ManagedCPVPCRefs, error) {
	if clusterName == "" {
		return nil, errors.New("eks: EnsureClusterCPVPC empty cluster name")
	}
	vpcCIDR, publicCIDR, privateCIDRs := cpVPCCIDRs(clusterName, privateCount)
	refs := &ManagedCPVPCRefs{VpcCIDR: vpcCIDR, PublicSubnetCIDR: publicCIDR, PrivateSubnetCIDRs: privateCIDRs}

	vpcID, err := ensureCPVPC(ctx, deps.VPC, accountID, clusterName, vpcCIDR)
	if err != nil {
		return nil, err
	}
	refs.VpcID = vpcID

	if err := EnsureClusterIGW(ctx, deps.IGW, accountID, vpcID, clusterName); err != nil {
		return nil, err
	}
	igw, err := attachedVPCIGW(ctx, deps.IGW, accountID, vpcID)
	if err != nil {
		return nil, err
	}
	if igw == nil {
		return nil, fmt.Errorf("eks: cp vpc %s has no attached IGW after ensure", vpcID)
	}
	refs.IGWID = aws.StringValue(igw.InternetGatewayId)

	pubSubnet, err := ensureCPSubnet(ctx, deps.VPC, accountID, clusterName, vpcID, publicCIDR, clusterEKSRolePublicSubnet, cpVPCAZ(region, 0))
	if err != nil {
		return nil, err
	}
	refs.PublicSubnetID = pubSubnet

	for k, cidr := range privateCIDRs {
		priv, err := ensureCPSubnet(ctx, deps.VPC, accountID, clusterName, vpcID, cidr, clusterEKSRolePrivateSubnet, cpVPCAZ(region, k))
		if err != nil {
			return nil, err
		}
		refs.PrivateSubnetIDs = append(refs.PrivateSubnetIDs, priv)
	}

	// Public route table: 0.0.0.0/0 → IGW, associated to the public subnet. The
	// association publishes vpc.add-igw-route → EnsureSubnetEgress (1000 reroute).
	pubRT, err := ensureCPRouteTable(ctx, deps.RT, accountID, clusterName, vpcID, clusterEKSRolePublicRT,
		&ec2.CreateRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(refs.IGWID)},
		[]string{refs.PublicSubnetID})
	if err != nil {
		return nil, err
	}
	refs.PublicRouteTableID = pubRT

	// NAT gateway in the public subnet, fed by a dedicated EIP. The private CP
	// subnets route 0.0.0.0/0 → this NAT GW for egress-only internet (image pulls).
	natID, allocID, natIP, err := ensureCPNatGateway(ctx, deps, accountID, clusterName, refs.PublicSubnetID)
	if err != nil {
		return nil, err
	}
	refs.NatGatewayID = natID
	refs.NatEIPAllocationID = allocID
	refs.NatEIPPublicIP = natIP

	// Private route table: 0.0.0.0/0 → NAT GW, associated to every private subnet.
	// The association publishes vpc.add-nat-gateway → EnsureNATGatewaySubnetEgress.
	privRT, err := ensureCPRouteTable(ctx, deps.RT, accountID, clusterName, vpcID, clusterEKSRolePrivateRT,
		&ec2.CreateRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String(natID)},
		refs.PrivateSubnetIDs)
	if err != nil {
		return nil, err
	}
	refs.PrivateRouteTableID = privRT

	slog.InfoContext(ctx, "EnsureClusterCPVPC: managed control-plane VPC ready",
		"cluster", clusterName, "vpc", refs.VpcID, "publicSubnet", refs.PublicSubnetID,
		"privateSubnets", refs.PrivateSubnetIDs, "natgw", refs.NatGatewayID)
	return refs, nil
}

func ensureCPVPC(ctx context.Context, vpcp vpcProvisioner, accountID, clusterName, cidr string) (string, error) {
	out, err := vpcp.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: cpClusterRoleFilters(clusterName, clusterEKSRoleCPVPC)}, accountID)
	if err != nil {
		return "", fmt.Errorf("eks: describe cp vpc for %s: %w", clusterName, err)
	}
	if out != nil && len(out.Vpcs) > 0 {
		return aws.StringValue(out.Vpcs[0].VpcId), nil
	}
	created, err := vpcp.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cidr),
		TagSpecifications: cpVPCTagSpec("vpc", clusterName, clusterEKSRoleCPVPC),
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("eks: create cp vpc for %s: %w", clusterName, err)
	}
	if created == nil || created.Vpc == nil || aws.StringValue(created.Vpc.VpcId) == "" {
		return "", fmt.Errorf("eks: create cp vpc for %s: empty vpc id", clusterName)
	}
	return aws.StringValue(created.Vpc.VpcId), nil
}

func ensureCPSubnet(ctx context.Context, vpcp vpcProvisioner, accountID, clusterName, vpcID, cidr, role, az string) (string, error) {
	out, err := vpcp.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: append(
		cpClusterRoleFilters(clusterName, role),
		&ec2.Filter{Name: aws.String("cidr-block"), Values: aws.StringSlice([]string{cidr})},
	)}, accountID)
	if err != nil {
		return "", fmt.Errorf("eks: describe cp subnet %s (%s): %w", cidr, role, err)
	}
	if out != nil && len(out.Subnets) > 0 {
		return aws.StringValue(out.Subnets[0].SubnetId), nil
	}
	created, err := vpcp.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(vpcID),
		CidrBlock:         aws.String(cidr),
		AvailabilityZone:  aws.String(az),
		TagSpecifications: cpVPCTagSpec("subnet", clusterName, role),
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("eks: create cp subnet %s (%s): %w", cidr, role, err)
	}
	if created == nil || created.Subnet == nil || aws.StringValue(created.Subnet.SubnetId) == "" {
		return "", fmt.Errorf("eks: create cp subnet %s (%s): empty subnet id", cidr, role)
	}
	return aws.StringValue(created.Subnet.SubnetId), nil
}

// ensureCPRouteTable describe-or-creates the role-tagged route table, installs
// the default route, and associates it to subnetIDs. Idempotent: a re-run reuses
// the existing table and its associations (AssociateRouteTable is a no-op for an
// already-associated subnet at the service layer).
func ensureCPRouteTable(ctx context.Context, rtp routeTableProvisioner, accountID, clusterName, vpcID, role string, route *ec2.CreateRouteInput, subnetIDs []string) (string, error) {
	rtID, fresh, err := describeOrCreateCPRouteTable(ctx, rtp, accountID, clusterName, vpcID, role)
	if err != nil {
		return "", err
	}
	if fresh {
		route.RouteTableId = aws.String(rtID)
		if _, err := rtp.CreateRoute(ctx, route, accountID); err != nil {
			return "", fmt.Errorf("eks: create cp route (%s) on %s: %w", role, rtID, err)
		}
	}
	for _, sn := range subnetIDs {
		if sn == "" {
			continue
		}
		if _, err := rtp.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtID),
			SubnetId:     aws.String(sn),
		}, accountID); err != nil {
			return "", fmt.Errorf("eks: associate cp route table %s → subnet %s: %w", rtID, sn, err)
		}
	}
	return rtID, nil
}

func describeOrCreateCPRouteTable(ctx context.Context, rtp routeTableProvisioner, accountID, clusterName, vpcID, role string) (rtID string, fresh bool, err error) {
	out, err := rtp.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: cpClusterRoleFilters(clusterName, role)}, accountID)
	if err != nil {
		return "", false, fmt.Errorf("eks: describe cp route table (%s): %w", role, err)
	}
	if out != nil && len(out.RouteTables) > 0 {
		return aws.StringValue(out.RouteTables[0].RouteTableId), false, nil
	}
	created, err := rtp.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId:             aws.String(vpcID),
		TagSpecifications: cpVPCTagSpec("route-table", clusterName, role),
	}, accountID)
	if err != nil {
		return "", false, fmt.Errorf("eks: create cp route table (%s): %w", role, err)
	}
	if created == nil || created.RouteTable == nil || aws.StringValue(created.RouteTable.RouteTableId) == "" {
		return "", false, fmt.Errorf("eks: create cp route table (%s): empty id", role)
	}
	return aws.StringValue(created.RouteTable.RouteTableId), true, nil
}

func ensureCPNatGateway(ctx context.Context, deps CPVPCDeps, accountID, clusterName, publicSubnetID string) (natID, allocID, publicIP string, err error) {
	out, err := deps.NGW.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{Filter: append(
		cpClusterRoleFilters(clusterName, clusterEKSRoleCPNatGW),
		&ec2.Filter{Name: aws.String("state"), Values: aws.StringSlice([]string{"available", "pending"})},
	)}, accountID)
	if err != nil {
		return "", "", "", fmt.Errorf("eks: describe cp nat gateway for %s: %w", clusterName, err)
	}
	if out != nil && len(out.NatGateways) > 0 {
		ng := out.NatGateways[0]
		var alloc, ip string
		if len(ng.NatGatewayAddresses) > 0 {
			alloc = aws.StringValue(ng.NatGatewayAddresses[0].AllocationId)
			ip = aws.StringValue(ng.NatGatewayAddresses[0].PublicIp)
		}
		return aws.StringValue(ng.NatGatewayId), alloc, ip, nil
	}

	eip, err := deps.EIP.AllocateAddress(ctx, &ec2.AllocateAddressInput{
		Domain:            aws.String("vpc"),
		TagSpecifications: cpVPCTagSpec("elastic-ip", clusterName, clusterEKSRoleCPNatGW),
	}, accountID)
	if err != nil {
		return "", "", "", fmt.Errorf("eks: allocate cp nat gateway EIP for %s: %w", clusterName, err)
	}
	if eip == nil || aws.StringValue(eip.AllocationId) == "" {
		return "", "", "", fmt.Errorf("eks: allocate cp nat gateway EIP for %s: empty allocation", clusterName)
	}
	allocID = aws.StringValue(eip.AllocationId)
	publicIP = aws.StringValue(eip.PublicIp)

	created, err := deps.NGW.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId:          aws.String(publicSubnetID),
		AllocationId:      aws.String(allocID),
		TagSpecifications: cpVPCTagSpec("natgateway", clusterName, clusterEKSRoleCPNatGW),
	}, accountID)
	if err != nil {
		// Release the orphaned EIP so a failed NAT-GW create does not leak it.
		if _, relErr := deps.EIP.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)}, accountID); relErr != nil {
			slog.WarnContext(ctx, "ensureCPNatGateway: failed to release EIP after NAT-GW create failure", "alloc", allocID, "err", relErr)
		}
		return "", "", "", fmt.Errorf("eks: create cp nat gateway for %s: %w", clusterName, err)
	}
	if created == nil || created.NatGateway == nil || aws.StringValue(created.NatGateway.NatGatewayId) == "" {
		return "", "", "", fmt.Errorf("eks: create cp nat gateway for %s: empty id", clusterName)
	}
	return aws.StringValue(created.NatGateway.NatGatewayId), allocID, publicIP, nil
}

// DeleteClusterCPVPC tears down the managed control-plane VPC for clusterName in
// dependency order: NAT gateway (+ its EIP) → route tables → subnets → IGW →
// VPC. Tag-driven (not meta-driven) so a partial create still converges to a
// clean delete. Best-effort: every step is attempted and failures are logged;
// the final VPC delete failure is returned so the caller can surface a leak.
// Safe to call when nothing was provisioned (all describes return empty).
//
// knownRefs is the caller's last-persisted cp-vpc state record (ClusterMeta's
// ManagedCPVPC), used only as an OVN-GC target fallback when the tag-indexed
// EC2 VPC is already gone — the live describe can no longer name it, but the
// record still can. Pass nil when no such record exists (e.g. the billable
// reaper, which acts once the cluster meta itself is gone).
func DeleteClusterCPVPC(ctx context.Context, deps CPVPCDeps, accountID, clusterName string, knownRefs *ManagedCPVPC) error {
	if clusterName == "" {
		return errors.New("eks: DeleteClusterCPVPC empty cluster name")
	}

	// 1. Route tables: disassociate every subnet, then delete. Disassociation
	// publishes the egress-reroute removal for each subnet. Route tables go first
	// so the NAT gateway's live route refs are gone before its delete — the NAT
	// GW delete is guarded against live route forwards (rule #3), so deleting it
	// while the private RT still routes 0.0.0.0/0 to it would fail (and strand
	// its billable EIP).
	for _, role := range []string{clusterEKSRolePublicRT, clusterEKSRolePrivateRT} {
		rtOut, err := deps.RT.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: cpClusterRoleFilters(clusterName, role)}, accountID)
		if err != nil {
			slog.WarnContext(ctx, "DeleteClusterCPVPC: describe route tables failed", "role", role, "err", err)
			continue
		}
		if rtOut == nil {
			continue
		}
		for _, rt := range rtOut.RouteTables {
			rtID := aws.StringValue(rt.RouteTableId)
			for _, assoc := range rt.Associations {
				if aws.BoolValue(assoc.Main) {
					continue
				}
				if aID := aws.StringValue(assoc.RouteTableAssociationId); aID != "" {
					if _, err := deps.RT.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{AssociationId: aws.String(aID)}, accountID); err != nil {
						slog.WarnContext(ctx, "DeleteClusterCPVPC: disassociate route table failed", "assoc", aID, "err", err)
					}
				}
			}
			if _, err := deps.RT.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtID)}, accountID); err != nil {
				slog.WarnContext(ctx, "DeleteClusterCPVPC: delete route table failed", "rt", rtID, "err", err)
			}
		}
	}

	// 2. NAT gateway + its EIP. DeleteNatGateway publishes the SNAT removal and
	// disassociates the EIP; release follows so the billable address is reclaimed.
	if ngOut, err := deps.NGW.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
		Filter: cpClusterRoleFilters(clusterName, clusterEKSRoleCPNatGW),
	}, accountID); err != nil {
		slog.WarnContext(ctx, "DeleteClusterCPVPC: describe NAT gateways failed", "cluster", clusterName, "err", err)
	} else if ngOut != nil {
		for _, ng := range ngOut.NatGateways {
			if state := aws.StringValue(ng.State); state == "deleted" || state == "deleting" {
				continue
			}
			ngID := aws.StringValue(ng.NatGatewayId)
			if _, err := deps.NGW.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(ngID)}, accountID); err != nil {
				slog.WarnContext(ctx, "DeleteClusterCPVPC: delete NAT gateway failed", "natgw", ngID, "err", err)
			}
			for _, addr := range ng.NatGatewayAddresses {
				if alloc := aws.StringValue(addr.AllocationId); alloc != "" {
					if _, err := deps.EIP.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(alloc)}, accountID); err != nil {
						slog.WarnContext(ctx, "DeleteClusterCPVPC: release NAT EIP failed", "alloc", alloc, "err", err)
					}
				}
			}
		}
	}

	// 3. Subnets (public + private).
	for _, role := range []string{clusterEKSRolePublicSubnet, clusterEKSRolePrivateSubnet} {
		snOut, err := deps.VPC.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: cpClusterRoleFilters(clusterName, role)}, accountID)
		if err != nil {
			slog.WarnContext(ctx, "DeleteClusterCPVPC: describe subnets failed", "role", role, "err", err)
			continue
		}
		if snOut == nil {
			continue
		}
		for _, sn := range snOut.Subnets {
			snID := aws.StringValue(sn.SubnetId)
			if _, err := deps.VPC.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(snID)}, accountID); err != nil {
				slog.WarnContext(ctx, "DeleteClusterCPVPC: delete subnet failed", "subnet", snID, "err", err)
			}
		}
	}

	// 4. VPC + its IGW. Resolve the VPC first so DeleteClusterIGW can detach.
	vpcOut, err := deps.VPC.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: cpClusterRoleFilters(clusterName, clusterEKSRoleCPVPC)}, accountID)
	if err != nil {
		return fmt.Errorf("eks: describe cp vpc for delete (%s): %w", clusterName, err)
	}

	// The tag-indexed VPC is already gone — a prior re-drive completed the EC2
	// delete, or it was reclaimed out-of-band. Idempotent success: nothing left
	// to delete here. Its OVN logical router/subnets/egress-drop policies may
	// still be orphaned if that prior delete's fire-and-forget vpc.delete event
	// never reached vpcd (e.g. NATS was down), so GC still runs below against
	// knownRefs — the only surviving reference to its identity.
	if vpcOut == nil || len(vpcOut.Vpcs) == 0 {
		gcClusterCPVPCTopology(ctx, deps.NATSConn, clusterName, cpVPCGCTarget(knownRefs))
		return nil
	}
	vpcID := aws.StringValue(vpcOut.Vpcs[0].VpcId)

	if err := DeleteClusterIGW(ctx, deps.IGW, accountID, vpcID, clusterName); err != nil {
		slog.WarnContext(ctx, "DeleteClusterCPVPC: delete IGW failed", "vpc", vpcID, "err", err)
	}
	if _, err := deps.VPC.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, accountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("eks: delete cp vpc %s: %w", vpcID, err)
	}
	gcClusterCPVPCTopology(ctx, deps.NATSConn, clusterName, vpcID)
	slog.InfoContext(ctx, "DeleteClusterCPVPC: managed control-plane VPC removed", "cluster", clusterName, "vpc", vpcID)
	return nil
}

// cpVPCGCTarget resolves the best-known VpcId for OVN GC when the tag-indexed
// EC2 record is already gone: the persisted (internal cp-vpc state record)
// VpcId, or "" if none was ever recorded (nil knownRefs).
func cpVPCGCTarget(knownRefs *ManagedCPVPC) string {
	if knownRefs == nil {
		return ""
	}
	return knownRefs.VpcId
}

// gcClusterCPVPCTopology republishes vpc.delete for vpcID so vpcd's topology
// manager gets another chance to remove the CP VPC's OVN logical router,
// subnets, DHCP options, and SubnetEgressPriorityDrop policies (owned by the
// router row, so OVSDB cascades their removal with it). That cleanup normally
// runs only as a side effect of a *live* DeleteVpc KV mutation — a re-drive
// against an already-gone VPC does not mutate the KV again, so a lost or
// never-delivered event would otherwise orphan the OVN state forever.
// Best-effort and idempotent: a nil conn or empty vpcID is a no-op, and vpcd
// tolerates re-deleting an already-absent router.
func gcClusterCPVPCTopology(ctx context.Context, nc *nats.Conn, clusterName, vpcID string) {
	if nc == nil || vpcID == "" {
		return
	}
	utils.PublishEvent(nc, "vpc.delete", struct {
		VpcId     string `json:"vpc_id"`
		CidrBlock string `json:"cidr_block"`
		VNI       int64  `json:"vni"`
	}{VpcId: vpcID})
	slog.InfoContext(ctx, "DeleteClusterCPVPC: republished vpc.delete for OVN GC", "cluster", clusterName, "vpc", vpcID)
}
