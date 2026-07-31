// Package handlers_systemvpc builds the managed VPCs Spinifex platform
// components run their own VMs in — the analogue of AWS's hidden managed-account
// VPCs. A component asks for one by name; it is owned by the system account and
// carries a public subnet routed to an IGW plus NAT-routed private subnets.
//
// The topology is composed from the real EC2 VPC-family APIs rather than direct
// OVN mutation, so per-subnet egress gating is wired by the existing topology
// subscribers off the route-table events those APIs publish.
//
// Every lookup is a describe-or-create keyed on the owner + role tags a Spec
// carries, so two components never see each other's resources and a relaunch
// after partial failure converges rather than duplicating.
package handlers_systemvpc

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/netip"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// supernetBits is the prefix length every component's supernet must carry. A /14
// holds the 256 /22 blocks nameHash indexes into.
const supernetBits = 14

// maxPrivateSubnets caps the private subnets carved per VPC, bounded by the /22:
// one public /24 plus three private /24s.
const maxPrivateSubnets = 3

// Owner is the tag identity stamped on every resource in a managed system VPC.
// It is what makes one component's tag-driven sweeps and orphan reapers blind to
// another's resources, so no two components may share an OwnerTagKey.
type Owner struct {
	// Name keys this particular VPC: it is the OwnerTagKey value and the seed
	// the address space is hashed from, so it must be stable for the VPC's life.
	Name string
	// ManagedBy is the tags.ManagedBy* value identifying the owning component.
	ManagedBy string
	// OwnerTagKey holds Name; RoleTagKey holds the per-resource role.
	OwnerTagKey string
	RoleTagKey  string
}

// Spec is an Owner plus the addressing and shape the VPC is built with. The same
// Spec must be passed to Ensure and Delete: a Spec differing on Name, either tag
// key or the supernet cannot find what a previous call created.
type Spec struct {
	Owner

	// Region names the cosmetic AvailabilityZone each subnet reports.
	Region string
	// RolePrefix namespaces the role tag values ("cp" → "cp-vpc", "cp-public"),
	// so the role tag stays readable when a deployment holds several components'
	// system VPCs.
	RolePrefix string
	// Supernet is the IPv4 /14 this VPC's /22 is carved from. Components must
	// not share one: a collision is survivable, but leaves an operator unable to
	// tell from an address which component owns it.
	Supernet string
	// PrivateSubnets is how many private subnets to carve, clamped to 1..3.
	PrivateSubnets int
}

// Roles are the role-tag values a Spec stamps on the resources it builds.
type Roles struct {
	VPC           string
	PublicSubnet  string
	PrivateSubnet string
	PublicRT      string
	PrivateRT     string
	NatGW         string
}

// Roles returns the spec's namespaced role-tag values. Exported so a caller's
// own teardown or test can filter on the same values Ensure wrote.
func (s Spec) Roles() Roles {
	return Roles{
		VPC:           s.RolePrefix + "-vpc",
		PublicSubnet:  s.RolePrefix + "-public",
		PrivateSubnet: s.RolePrefix + "-private",
		PublicRT:      s.RolePrefix + "-public-rt",
		PrivateRT:     s.RolePrefix + "-private-rt",
		NatGW:         s.RolePrefix + "-natgw",
	}
}

// VPCProvisioner is the narrow VPC + subnet surface a system VPC needs.
// The daemon adapts the concrete VPC service onto this.
type VPCProvisioner interface {
	CreateVpc(ctx context.Context, input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error)
	DeleteVpc(ctx context.Context, input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error)
	DescribeVpcs(ctx context.Context, input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error)
	CreateSubnet(ctx context.Context, input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error)
	DeleteSubnet(ctx context.Context, input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
}

// RouteTableProvisioner is the narrow route-table surface needed to express
// public (0/0→IGW) and private (0/0→NAT GW) egress.
type RouteTableProvisioner interface {
	CreateRouteTable(ctx context.Context, input *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error)
	DeleteRouteTable(ctx context.Context, input *ec2.DeleteRouteTableInput, accountID string) (*ec2.DeleteRouteTableOutput, error)
	DescribeRouteTables(ctx context.Context, input *ec2.DescribeRouteTablesInput, accountID string) (*ec2.DescribeRouteTablesOutput, error)
	CreateRoute(ctx context.Context, input *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error)
	AssociateRouteTable(ctx context.Context, input *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error)
	DisassociateRouteTable(ctx context.Context, input *ec2.DisassociateRouteTableInput, accountID string) (*ec2.DisassociateRouteTableOutput, error)
}

// NATGatewayProvisioner is the narrow NAT-gateway surface that gives the private
// subnets egress.
type NATGatewayProvisioner interface {
	CreateNatGateway(ctx context.Context, input *ec2.CreateNatGatewayInput, accountID string) (*ec2.CreateNatGatewayOutput, error)
	DeleteNatGateway(ctx context.Context, input *ec2.DeleteNatGatewayInput, accountID string) (*ec2.DeleteNatGatewayOutput, error)
	DescribeNatGateways(ctx context.Context, input *ec2.DescribeNatGatewaysInput, accountID string) (*ec2.DescribeNatGatewaysOutput, error)
}

// EIPProvisioner is the narrow EIP surface backing the NAT gateway's address.
type EIPProvisioner interface {
	AllocateAddress(ctx context.Context, input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(ctx context.Context, input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error)
}

// SGProvisioner is the security-group surface Delete uses to clear groups left
// in the VPC that the tag-driven sweep cannot see.
type SGProvisioner interface {
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error)
}

// Deps bundles the EC2-family collaborators a system VPC is composed from. All
// calls run under the accountID passed to Ensure/Delete (the system account).
type Deps struct {
	VPC VPCProvisioner
	// SG is used only by Delete. Nil is tolerated.
	SG  SGProvisioner
	IGW IGWProvisioner
	RT  RouteTableProvisioner
	NGW NATGatewayProvisioner
	EIP EIPProvisioner
	// NATSConn republishes vpc.delete for OVN topology GC (see gcTopology).
	// Nil is tolerated (GC is skipped, never blocking).
	NATSConn *nats.Conn
}

// Refs is the resolved set of system VPC resource IDs + CIDRs. Ensure returns
// it; the caller persists it for placement (public/private subnet selection)
// and teardown.
type Refs struct {
	VpcID               string
	VpcCIDR             string
	IGWID               string
	PublicSubnetID      string
	PublicSubnetCIDR    string
	PrivateRouteTableID string
	PublicRouteTableID  string
	PrivateSubnetIDs    []string
	PrivateSubnetCIDRs  []string
	NatGatewayID        string
	NatEIPAllocationID  string
	NatEIPPublicIP      string
}

// cidrs derives the VPC CIDR + subnet CIDRs deterministically from the spec's
// name. The /22 is carved from the spec's /14 supernet; subnet 0 is public,
// subnets 1..PrivateSubnets are private. A hash collision is non-fatal.
func (s Spec) cidrs() (vpcCIDR, publicCIDR string, privateCIDRs []string, err error) {
	base, err := s.supernetBase()
	if err != nil {
		return "", "", nil, err
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(s.Name))
	idx := int(h.Sum32() % 256) // 256 /22 blocks in a /14

	combined := idx * 4 // each /22 spans 4 third-octet values
	second := int(base[1]) + combined/256
	third := combined % 256

	vpcCIDR = fmt.Sprintf("%d.%d.%d.0/22", base[0], second, third)
	publicCIDR = fmt.Sprintf("%d.%d.%d.0/24", base[0], second, third)
	privateCount := min(max(s.PrivateSubnets, 1), maxPrivateSubnets)
	for k := range privateCount {
		privateCIDRs = append(privateCIDRs, fmt.Sprintf("%d.%d.%d.0/24", base[0], second, third+1+k))
	}
	return vpcCIDR, publicCIDR, privateCIDRs, nil
}

// supernetBase validates the spec's supernet and returns its network address.
// The /14 is required so the 256-block hash index cannot walk past the space
// the component was allotted.
func (s Spec) supernetBase() ([4]byte, error) {
	prefix, err := netip.ParsePrefix(s.Supernet)
	if err != nil {
		return [4]byte{}, fmt.Errorf("systemvpc: %s: parse supernet %q: %w", s.Name, s.Supernet, err)
	}
	if !prefix.Addr().Is4() || prefix.Bits() != supernetBits {
		return [4]byte{}, fmt.Errorf("systemvpc: %s: supernet %q must be an IPv4 /%d", s.Name, s.Supernet, supernetBits)
	}
	if prefix.Masked() != prefix {
		return [4]byte{}, fmt.Errorf("systemvpc: %s: supernet %q has host bits set", s.Name, s.Supernet)
	}
	return prefix.Addr().As4(), nil
}

// az returns the AvailabilityZone name for subnet index k (cosmetic AWS parity;
// spinifex spreads placement by host placement group, not AZ).
func (s Spec) az(k int) string {
	return fmt.Sprintf("%s%c", s.Region, 'a'+k)
}

// tagSpec builds the owner + role tag set stamped on a created resource.
func (s Spec) tagSpec(resourceType, role string) []*ec2.TagSpecification {
	return []*ec2.TagSpecification{{
		ResourceType: aws.String(resourceType),
		Tags: []*ec2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(s.ManagedBy)},
			{Key: aws.String(s.OwnerTagKey), Value: aws.String(s.Name)},
			{Key: aws.String(s.RoleTagKey), Value: aws.String(role)},
		},
	}}
}

// roleFilters is the describe-side counterpart of tagSpec: it matches only the
// resources this owner created for this role.
func (s Spec) roleFilters(role string) []*ec2.Filter {
	return []*ec2.Filter{
		{Name: aws.String("tag:" + s.OwnerTagKey), Values: aws.StringSlice([]string{s.Name})},
		{Name: aws.String("tag:" + s.RoleTagKey), Values: aws.StringSlice([]string{role})},
	}
}

// validate rejects a Spec that could not address or tag its resources. Checked
// up front so a half-specified component fails before it creates anything.
func (s Spec) validate() error {
	switch {
	case s.Name == "":
		return errors.New("systemvpc: empty name")
	case s.ManagedBy == "":
		return fmt.Errorf("systemvpc: %s: empty managed-by value", s.Name)
	case s.OwnerTagKey == "" || s.RoleTagKey == "":
		return fmt.Errorf("systemvpc: %s: empty owner or role tag key", s.Name)
	case s.RolePrefix == "":
		return fmt.Errorf("systemvpc: %s: empty role prefix", s.Name)
	}
	_, err := s.supernetBase()
	return err
}

// Ensure idempotently builds the managed system VPC described by spec: a VPC, an
// attached IGW, an IGW-routed public subnet, NAT-routed private subnets, and the
// NAT gateway itself. Each resource is describe-or-create on the role tags.
//
// The route-table associations publish the topology events the per-subnet egress
// subscribers consume, so the OVN policy split falls out without any direct OVN
// mutation here.
func Ensure(ctx context.Context, deps Deps, spec Spec, accountID string) (*Refs, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	vpcCIDR, publicCIDR, privateCIDRs, err := spec.cidrs()
	if err != nil {
		return nil, err
	}
	roles := spec.Roles()
	refs := &Refs{VpcCIDR: vpcCIDR, PublicSubnetCIDR: publicCIDR, PrivateSubnetCIDRs: privateCIDRs}

	vpcID, err := ensureVPC(ctx, deps.VPC, spec, accountID, vpcCIDR)
	if err != nil {
		return nil, err
	}
	refs.VpcID = vpcID

	if err := EnsureIGW(ctx, deps.IGW, spec.Owner, accountID, vpcID); err != nil {
		return nil, err
	}
	igw, err := AttachedIGW(ctx, deps.IGW, accountID, vpcID)
	if err != nil {
		return nil, err
	}
	if igw == nil {
		return nil, fmt.Errorf("systemvpc: %s: vpc %s has no attached IGW after ensure", spec.Name, vpcID)
	}
	refs.IGWID = aws.StringValue(igw.InternetGatewayId)

	pubSubnet, err := ensureSubnet(ctx, deps.VPC, spec, accountID, vpcID, publicCIDR, roles.PublicSubnet, spec.az(0))
	if err != nil {
		return nil, err
	}
	refs.PublicSubnetID = pubSubnet

	for k, cidr := range privateCIDRs {
		priv, err := ensureSubnet(ctx, deps.VPC, spec, accountID, vpcID, cidr, roles.PrivateSubnet, spec.az(k))
		if err != nil {
			return nil, err
		}
		refs.PrivateSubnetIDs = append(refs.PrivateSubnetIDs, priv)
	}

	// Public route table: 0.0.0.0/0 → IGW, associated to the public subnet. The
	// association publishes vpc.add-igw-route → EnsureSubnetEgress (1000 reroute).
	pubRT, err := ensureRouteTable(ctx, deps.RT, spec, accountID, vpcID, roles.PublicRT,
		&ec2.CreateRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String(refs.IGWID)},
		[]string{refs.PublicSubnetID})
	if err != nil {
		return nil, err
	}
	refs.PublicRouteTableID = pubRT

	// NAT gateway in the public subnet, fed by a dedicated EIP. The private
	// subnets route 0.0.0.0/0 → this NAT GW for egress-only internet.
	natID, allocID, natIP, err := ensureNatGateway(ctx, deps, spec, accountID, refs.PublicSubnetID)
	if err != nil {
		return nil, err
	}
	refs.NatGatewayID = natID
	refs.NatEIPAllocationID = allocID
	refs.NatEIPPublicIP = natIP

	// Private route table: 0.0.0.0/0 → NAT GW, associated to every private subnet.
	// The association publishes vpc.add-nat-gateway → EnsureNATGatewaySubnetEgress.
	privRT, err := ensureRouteTable(ctx, deps.RT, spec, accountID, vpcID, roles.PrivateRT,
		&ec2.CreateRouteInput{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String(natID)},
		refs.PrivateSubnetIDs)
	if err != nil {
		return nil, err
	}
	refs.PrivateRouteTableID = privRT

	slog.InfoContext(ctx, "systemvpc: managed system VPC ready",
		"name", spec.Name, "managedBy", spec.ManagedBy, "vpc", refs.VpcID,
		"publicSubnet", refs.PublicSubnetID, "privateSubnets", refs.PrivateSubnetIDs,
		"natgw", refs.NatGatewayID)
	return refs, nil
}

func ensureVPC(ctx context.Context, vpcp VPCProvisioner, spec Spec, accountID, cidr string) (string, error) {
	role := spec.Roles().VPC
	out, err := vpcp.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: spec.roleFilters(role)}, accountID)
	if err != nil {
		return "", fmt.Errorf("systemvpc: describe vpc for %s: %w", spec.Name, err)
	}
	if out != nil && len(out.Vpcs) > 0 {
		return aws.StringValue(out.Vpcs[0].VpcId), nil
	}
	created, err := vpcp.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cidr),
		TagSpecifications: spec.tagSpec("vpc", role),
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("systemvpc: create vpc for %s: %w", spec.Name, err)
	}
	if created == nil || created.Vpc == nil || aws.StringValue(created.Vpc.VpcId) == "" {
		return "", fmt.Errorf("systemvpc: create vpc for %s: empty vpc id", spec.Name)
	}
	return aws.StringValue(created.Vpc.VpcId), nil
}

func ensureSubnet(ctx context.Context, vpcp VPCProvisioner, spec Spec, accountID, vpcID, cidr, role, az string) (string, error) {
	out, err := vpcp.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: append(
		spec.roleFilters(role),
		&ec2.Filter{Name: aws.String("cidr-block"), Values: aws.StringSlice([]string{cidr})},
	)}, accountID)
	if err != nil {
		return "", fmt.Errorf("systemvpc: describe subnet %s (%s): %w", cidr, role, err)
	}
	if out != nil && len(out.Subnets) > 0 {
		return aws.StringValue(out.Subnets[0].SubnetId), nil
	}
	created, err := vpcp.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(vpcID),
		CidrBlock:         aws.String(cidr),
		AvailabilityZone:  aws.String(az),
		TagSpecifications: spec.tagSpec("subnet", role),
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("systemvpc: create subnet %s (%s): %w", cidr, role, err)
	}
	if created == nil || created.Subnet == nil || aws.StringValue(created.Subnet.SubnetId) == "" {
		return "", fmt.Errorf("systemvpc: create subnet %s (%s): empty subnet id", cidr, role)
	}
	return aws.StringValue(created.Subnet.SubnetId), nil
}

// ensureRouteTable describe-or-creates the role-tagged route table, installs the
// default route, and associates it to subnetIDs. Idempotent: a re-run reuses the
// existing table and re-drives the associations.
func ensureRouteTable(ctx context.Context, rtp RouteTableProvisioner, spec Spec, accountID, vpcID, role string, route *ec2.CreateRouteInput, subnetIDs []string) (string, error) {
	rt, fresh, err := describeOrCreateRouteTable(ctx, rtp, spec, accountID, vpcID, role)
	if err != nil {
		return "", err
	}
	rtID := aws.StringValue(rt.RouteTableId)
	if fresh {
		route.RouteTableId = aws.String(rtID)
		if _, err := rtp.CreateRoute(ctx, route, accountID); err != nil {
			return "", fmt.Errorf("systemvpc: create route (%s) on %s: %w", role, rtID, err)
		}
	}
	for _, sn := range subnetIDs {
		if sn == "" {
			continue
		}
		_, err := rtp.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			RouteTableId: aws.String(rtID),
			SubnetId:     aws.String(sn),
		}, accountID)
		if err == nil {
			continue
		}
		// AssociateRouteTable rejects a subnet holding an explicit association on
		// any table in the VPC. Tolerate that only when the association is this
		// table's — a subnet parked on someone else's table has no egress, and for
		// a shared system VPC every Ensure after the first takes this path.
		if !awserrors.IsErrorCode(err, awserrors.ErrorResourceAlreadyAssociated) || !associatedWith(rt, sn) {
			return "", fmt.Errorf("systemvpc: associate route table %s → subnet %s: %w", rtID, sn, err)
		}
	}
	return rtID, nil
}

// associatedWith reports whether subnetID already holds an explicit association
// on rt, as of the describe ensureRouteTable adopted it from.
func associatedWith(rt *ec2.RouteTable, subnetID string) bool {
	for _, assoc := range rt.Associations {
		if aws.StringValue(assoc.SubnetId) == subnetID && !aws.BoolValue(assoc.Main) {
			return true
		}
	}
	return false
}

// describeOrCreateRouteTable returns the role-tagged route table, adopting an
// existing one where possible. The table is returned whole rather than by ID so
// the caller can read the associations the describe already carried.
func describeOrCreateRouteTable(ctx context.Context, rtp RouteTableProvisioner, spec Spec, accountID, vpcID, role string) (rt *ec2.RouteTable, fresh bool, err error) {
	out, err := rtp.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: spec.roleFilters(role)}, accountID)
	if err != nil {
		return nil, false, fmt.Errorf("systemvpc: describe route table (%s): %w", role, err)
	}
	if out != nil && len(out.RouteTables) > 0 {
		return out.RouteTables[0], false, nil
	}
	created, err := rtp.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{
		VpcId:             aws.String(vpcID),
		TagSpecifications: spec.tagSpec("route-table", role),
	}, accountID)
	if err != nil {
		return nil, false, fmt.Errorf("systemvpc: create route table (%s): %w", role, err)
	}
	if created == nil || created.RouteTable == nil || aws.StringValue(created.RouteTable.RouteTableId) == "" {
		return nil, false, fmt.Errorf("systemvpc: create route table (%s): empty id", role)
	}
	return created.RouteTable, true, nil
}

func ensureNatGateway(ctx context.Context, deps Deps, spec Spec, accountID, publicSubnetID string) (natID, allocID, publicIP string, err error) {
	role := spec.Roles().NatGW
	out, err := deps.NGW.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{Filter: append(
		spec.roleFilters(role),
		&ec2.Filter{Name: aws.String("state"), Values: aws.StringSlice([]string{"available", "pending"})},
	)}, accountID)
	if err != nil {
		return "", "", "", fmt.Errorf("systemvpc: describe nat gateway for %s: %w", spec.Name, err)
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
		TagSpecifications: spec.tagSpec("elastic-ip", role),
	}, accountID)
	if err != nil {
		return "", "", "", fmt.Errorf("systemvpc: allocate nat gateway EIP for %s: %w", spec.Name, err)
	}
	if eip == nil || aws.StringValue(eip.AllocationId) == "" {
		return "", "", "", fmt.Errorf("systemvpc: allocate nat gateway EIP for %s: empty allocation", spec.Name)
	}
	allocID = aws.StringValue(eip.AllocationId)
	publicIP = aws.StringValue(eip.PublicIp)

	created, err := deps.NGW.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId:          aws.String(publicSubnetID),
		AllocationId:      aws.String(allocID),
		TagSpecifications: spec.tagSpec("natgateway", role),
	}, accountID)
	if err != nil {
		// Release the orphaned EIP so a failed NAT-GW create does not leak it.
		if _, relErr := deps.EIP.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)}, accountID); relErr != nil {
			slog.WarnContext(ctx, "systemvpc: failed to release EIP after NAT-GW create failure", "alloc", allocID, "err", relErr)
		}
		return "", "", "", fmt.Errorf("systemvpc: create nat gateway for %s: %w", spec.Name, err)
	}
	if created == nil || created.NatGateway == nil || aws.StringValue(created.NatGateway.NatGatewayId) == "" {
		return "", "", "", fmt.Errorf("systemvpc: create nat gateway for %s: empty id", spec.Name)
	}
	return aws.StringValue(created.NatGateway.NatGatewayId), allocID, publicIP, nil
}

// Delete tears down the managed system VPC in dependency order: NAT gateway
// (+ its EIP) → route tables → subnets → IGW → VPC. Tag-driven, so a partial
// create still converges, and best-effort apart from the final VPC delete.
//
// gcFallbackVpcID is the caller's last-persisted VpcId, used as an OVN-GC target
// when the tag-indexed EC2 VPC is already gone. Pass "" when no such record
// exists, e.g. an orphan reaper acting after the owning record.
func Delete(ctx context.Context, deps Deps, spec Spec, accountID, gcFallbackVpcID string) error {
	if err := spec.validate(); err != nil {
		return err
	}
	roles := spec.Roles()

	// 1. Route tables: disassociate every subnet, then delete. They go first so
	// the NAT gateway's live route refs are gone before its own delete, which is
	// guarded against live forwards and would otherwise strand its billable EIP.
	for _, role := range []string{roles.PublicRT, roles.PrivateRT} {
		rtOut, err := deps.RT.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: spec.roleFilters(role)}, accountID)
		if err != nil {
			slog.WarnContext(ctx, "systemvpc Delete: describe route tables failed", "role", role, "err", err)
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
						slog.WarnContext(ctx, "systemvpc Delete: disassociate route table failed", "assoc", aID, "err", err)
					}
				}
			}
			if _, err := deps.RT.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{RouteTableId: aws.String(rtID)}, accountID); err != nil {
				slog.WarnContext(ctx, "systemvpc Delete: delete route table failed", "rt", rtID, "err", err)
			}
		}
	}

	// 2. NAT gateway + its EIP. DeleteNatGateway publishes the SNAT removal and
	// disassociates the EIP; release follows so the billable address is reclaimed.
	if ngOut, err := deps.NGW.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{
		Filter: spec.roleFilters(roles.NatGW),
	}, accountID); err != nil {
		slog.WarnContext(ctx, "systemvpc Delete: describe NAT gateways failed", "name", spec.Name, "err", err)
	} else if ngOut != nil {
		for _, ng := range ngOut.NatGateways {
			if state := aws.StringValue(ng.State); state == "deleted" || state == "deleting" {
				continue
			}
			ngID := aws.StringValue(ng.NatGatewayId)
			if _, err := deps.NGW.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: aws.String(ngID)}, accountID); err != nil {
				slog.WarnContext(ctx, "systemvpc Delete: delete NAT gateway failed", "natgw", ngID, "err", err)
			}
			for _, addr := range ng.NatGatewayAddresses {
				if alloc := aws.StringValue(addr.AllocationId); alloc != "" {
					if _, err := deps.EIP.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(alloc)}, accountID); err != nil {
						slog.WarnContext(ctx, "systemvpc Delete: release NAT EIP failed", "alloc", alloc, "err", err)
					}
				}
			}
		}
	}

	// 3. Subnets (public + private).
	for _, role := range []string{roles.PublicSubnet, roles.PrivateSubnet} {
		snOut, err := deps.VPC.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: spec.roleFilters(role)}, accountID)
		if err != nil {
			slog.WarnContext(ctx, "systemvpc Delete: describe subnets failed", "role", role, "err", err)
			continue
		}
		if snOut == nil {
			continue
		}
		for _, sn := range snOut.Subnets {
			snID := aws.StringValue(sn.SubnetId)
			if _, err := deps.VPC.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(snID)}, accountID); err != nil {
				slog.WarnContext(ctx, "systemvpc Delete: delete subnet failed", "subnet", snID, "err", err)
			}
		}
	}

	// 4. VPC + its IGW. Resolve the VPC first so DeleteIGW can detach.
	vpcOut, err := deps.VPC.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: spec.roleFilters(roles.VPC)}, accountID)
	if err != nil {
		return fmt.Errorf("systemvpc: describe vpc for delete (%s): %w", spec.Name, err)
	}

	// The tag-indexed VPC is already gone, so this is an idempotent success. Its
	// OVN state may still be orphaned if that delete's fire-and-forget vpc.delete
	// never reached vpcd, so GC runs against gcFallbackVpcID.
	if vpcOut == nil || len(vpcOut.Vpcs) == 0 {
		gcTopology(ctx, deps.NATSConn, spec.Name, gcFallbackVpcID)
		return nil
	}
	vpcID := aws.StringValue(vpcOut.Vpcs[0].VpcId)

	if err := DeleteIGW(ctx, deps.IGW, spec.Owner, accountID, vpcID); err != nil {
		slog.WarnContext(ctx, "systemvpc Delete: delete IGW failed", "vpc", vpcID, "err", err)
	}

	// Sweep residual subnets by VPC identity, not by role tag: a create that
	// failed before tagging leaves subnets the tagged sweep cannot see, and
	// DeleteVpc then rejects with DependencyViolation on every re-drive.
	if snOut, err := deps.VPC.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})}},
	}, accountID); err != nil {
		slog.WarnContext(ctx, "systemvpc Delete: describe residual subnets failed", "vpc", vpcID, "err", err)
	} else if snOut != nil {
		for _, sn := range snOut.Subnets {
			snID := aws.StringValue(sn.SubnetId)
			slog.InfoContext(ctx, "systemvpc Delete: removing residual subnet the tagged sweep missed",
				"name", spec.Name, "vpc", vpcID, "subnet", snID)
			if _, err := deps.VPC.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(snID)}, accountID); err != nil {
				slog.WarnContext(ctx, "systemvpc Delete: delete residual subnet failed", "subnet", snID, "err", err)
			}
		}
	}

	// Same for security groups: DeleteVpc rejects any non-default SG, and the
	// tagged teardown misses ones a partial create never tagged.
	if deps.SG != nil {
		if sgOut, err := deps.SG.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
			Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})}},
		}, accountID); err != nil {
			slog.WarnContext(ctx, "systemvpc Delete: describe residual SGs failed", "vpc", vpcID, "err", err)
		} else if sgOut != nil {
			for _, sg := range sgOut.SecurityGroups {
				// The default SG goes with the VPC cascade and cannot be
				// deleted on its own; it is not what blocks DeleteVpc.
				if aws.StringValue(sg.GroupName) == "default" {
					continue
				}
				sgID := aws.StringValue(sg.GroupId)
				slog.InfoContext(ctx, "systemvpc Delete: removing residual SG the tagged sweep missed",
					"name", spec.Name, "vpc", vpcID, "sg", sgID)
				if _, err := deps.SG.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)}, accountID); err != nil {
					slog.WarnContext(ctx, "systemvpc Delete: delete residual SG failed", "sg", sgID, "err", err)
				}
			}
		}
	}

	if _, err := deps.VPC.DeleteVpc(ctx, &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, accountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("systemvpc: delete vpc %s: %w", vpcID, err)
	}
	gcTopology(ctx, deps.NATSConn, spec.Name, vpcID)
	slog.InfoContext(ctx, "systemvpc Delete: managed system VPC removed", "name", spec.Name, "vpc", vpcID)
	return nil
}

// gcTopology republishes vpc.delete for vpcID so vpcd's topology manager gets
// another chance to remove the VPC's OVN logical router and everything OVSDB
// cascades with it.
//
// That cleanup normally runs only off a live DeleteVpc KV mutation, so without
// this a lost event would orphan the OVN state forever. Best-effort and
// idempotent: vpcd tolerates re-deleting an absent router.
func gcTopology(ctx context.Context, nc *nats.Conn, name, vpcID string) {
	if nc == nil || vpcID == "" {
		return
	}
	utils.PublishEvent(nc, "vpc.delete", struct {
		VpcId     string `json:"vpc_id"`
		CidrBlock string `json:"cidr_block"`
		VNI       int64  `json:"vni"`
	}{VpcId: vpcID})
	slog.InfoContext(ctx, "systemvpc Delete: republished vpc.delete for OVN GC", "name", name, "vpc", vpcID)
}
