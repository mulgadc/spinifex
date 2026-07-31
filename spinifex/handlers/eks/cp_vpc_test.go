package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fakes for the managed control-plane VPC collaborators. They model
// describe-or-create idempotency by storing each resource with its create tags,
// so a re-describe under the same filters returns it rather than a duplicate.

var (
	_ vpcProvisioner        = (*fakeVPCProvisioner)(nil)
	_ routeTableProvisioner = (*fakeRouteTableProvisioner)(nil)
	_ natGatewayProvisioner = (*fakeNatGatewayProvisioner)(nil)
)

func cpvpcTagsFromSpecs(specs []*ec2.TagSpecification) map[string]string {
	m := map[string]string{}
	for _, s := range specs {
		if s == nil {
			continue
		}
		for _, t := range s.Tags {
			m[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
		}
	}
	return m
}

func ec2TagsFromMap(m map[string]string) []*ec2.Tag {
	out := make([]*ec2.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// cpvpcMatchTagFilters reports whether tagMap satisfies every `tag:KEY` filter.
// Non-tag filters are ignored here (scalar filters are checked separately).
func cpvpcMatchTagFilters(tagMap map[string]string, filters []*ec2.Filter) bool {
	for _, f := range filters {
		name := aws.StringValue(f.Name)
		if !strings.HasPrefix(name, "tag:") {
			continue
		}
		key := strings.TrimPrefix(name, "tag:")
		got, ok := tagMap[key]
		if !ok {
			return false
		}
		match := false
		for _, v := range f.Values {
			if aws.StringValue(v) == got {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}

// cpvpcMatchScalarFilter returns true when no filter named `name` is present, or
// one is present and value is among its values.
func cpvpcMatchScalarFilter(filters []*ec2.Filter, name, value string) bool {
	for _, fl := range filters {
		if aws.StringValue(fl.Name) != name {
			continue
		}
		for _, v := range fl.Values {
			if aws.StringValue(v) == value {
				return true
			}
		}
		return false
	}
	return true
}

// --- VPC + subnets ---

type fakeCPVPC struct {
	id   string
	cidr string
	tags map[string]string
}

type fakeCPSubnet struct {
	id   string
	vpc  string
	cidr string
	tags map[string]string
}

type fakeVPCProvisioner struct {
	vpcs    []*fakeCPVPC
	subnets []*fakeCPSubnet
	nextVPC int
	nextSub int

	createVpcCalls    []*ec2.CreateVpcInput
	createVpcAccts    []string
	createSubnetCalls []*ec2.CreateSubnetInput
	deleteVpcCalls    []*ec2.DeleteVpcInput
	deleteSubnetCalls []*ec2.DeleteSubnetInput

	createVpcErr    error
	createSubnetErr error
	deleteVpcErr    error
}

func newFakeVPCProvisioner() *fakeVPCProvisioner { return &fakeVPCProvisioner{} }

func (f *fakeVPCProvisioner) DescribeVpcs(_ context.Context, input *ec2.DescribeVpcsInput, _ string) (*ec2.DescribeVpcsOutput, error) {
	var out []*ec2.Vpc
	for _, v := range f.vpcs {
		if !cpvpcMatchTagFilters(v.tags, input.Filters) {
			continue
		}
		out = append(out, &ec2.Vpc{VpcId: aws.String(v.id), CidrBlock: aws.String(v.cidr), Tags: ec2TagsFromMap(v.tags)})
	}
	return &ec2.DescribeVpcsOutput{Vpcs: out}, nil
}

func (f *fakeVPCProvisioner) CreateVpc(_ context.Context, input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error) {
	f.createVpcCalls = append(f.createVpcCalls, input)
	f.createVpcAccts = append(f.createVpcAccts, accountID)
	if f.createVpcErr != nil {
		return nil, f.createVpcErr
	}
	f.nextVPC++
	v := &fakeCPVPC{
		id:   fmt.Sprintf("vpc-cp%04d", f.nextVPC),
		cidr: aws.StringValue(input.CidrBlock),
		tags: cpvpcTagsFromSpecs(input.TagSpecifications),
	}
	f.vpcs = append(f.vpcs, v)
	return &ec2.CreateVpcOutput{Vpc: &ec2.Vpc{VpcId: aws.String(v.id), CidrBlock: aws.String(v.cidr)}}, nil
}

func (f *fakeVPCProvisioner) DeleteVpc(_ context.Context, input *ec2.DeleteVpcInput, _ string) (*ec2.DeleteVpcOutput, error) {
	f.deleteVpcCalls = append(f.deleteVpcCalls, input)
	if f.deleteVpcErr != nil {
		return nil, f.deleteVpcErr
	}
	id := aws.StringValue(input.VpcId)
	kept := f.vpcs[:0]
	for _, v := range f.vpcs {
		if v.id != id {
			kept = append(kept, v)
		}
	}
	f.vpcs = kept
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *fakeVPCProvisioner) DescribeSubnets(_ context.Context, input *ec2.DescribeSubnetsInput, _ string) (*ec2.DescribeSubnetsOutput, error) {
	var out []*ec2.Subnet
	for _, sn := range f.subnets {
		if !cpvpcMatchTagFilters(sn.tags, input.Filters) {
			continue
		}
		if !cpvpcMatchScalarFilter(input.Filters, "cidr-block", sn.cidr) {
			continue
		}
		out = append(out, &ec2.Subnet{SubnetId: aws.String(sn.id), VpcId: aws.String(sn.vpc), CidrBlock: aws.String(sn.cidr), Tags: ec2TagsFromMap(sn.tags)})
	}
	return &ec2.DescribeSubnetsOutput{Subnets: out}, nil
}

func (f *fakeVPCProvisioner) CreateSubnet(_ context.Context, input *ec2.CreateSubnetInput, _ string) (*ec2.CreateSubnetOutput, error) {
	f.createSubnetCalls = append(f.createSubnetCalls, input)
	if f.createSubnetErr != nil {
		return nil, f.createSubnetErr
	}
	f.nextSub++
	sn := &fakeCPSubnet{
		id:   fmt.Sprintf("subnet-cp%04d", f.nextSub),
		vpc:  aws.StringValue(input.VpcId),
		cidr: aws.StringValue(input.CidrBlock),
		tags: cpvpcTagsFromSpecs(input.TagSpecifications),
	}
	f.subnets = append(f.subnets, sn)
	return &ec2.CreateSubnetOutput{Subnet: &ec2.Subnet{SubnetId: aws.String(sn.id), VpcId: input.VpcId, CidrBlock: input.CidrBlock}}, nil
}

func (f *fakeVPCProvisioner) DeleteSubnet(_ context.Context, input *ec2.DeleteSubnetInput, _ string) (*ec2.DeleteSubnetOutput, error) {
	f.deleteSubnetCalls = append(f.deleteSubnetCalls, input)
	id := aws.StringValue(input.SubnetId)
	kept := f.subnets[:0]
	for _, sn := range f.subnets {
		if sn.id != id {
			kept = append(kept, sn)
		}
	}
	f.subnets = kept
	return &ec2.DeleteSubnetOutput{}, nil
}

// --- Route tables ---

type fakeCPRouteTable struct {
	id     string
	vpc    string
	tags   map[string]string
	assocs []*ec2.RouteTableAssociation
}

type fakeRouteTableProvisioner struct {
	tables    []*fakeCPRouteTable
	nextRT    int
	nextAssoc int

	createCalls  []*ec2.CreateRouteTableInput
	createRoutes []*ec2.CreateRouteInput
	assocCalls   []*ec2.AssociateRouteTableInput
	disassoc     []*ec2.DisassociateRouteTableInput
	deleteCalls  []*ec2.DeleteRouteTableInput

	createErr error
}

func newFakeRouteTableProvisioner() *fakeRouteTableProvisioner { return &fakeRouteTableProvisioner{} }

func (f *fakeRouteTableProvisioner) find(id string) *fakeCPRouteTable {
	for _, rt := range f.tables {
		if rt.id == id {
			return rt
		}
	}
	return nil
}

func (f *fakeRouteTableProvisioner) DescribeRouteTables(_ context.Context, input *ec2.DescribeRouteTablesInput, _ string) (*ec2.DescribeRouteTablesOutput, error) {
	var out []*ec2.RouteTable
	for _, rt := range f.tables {
		if !cpvpcMatchTagFilters(rt.tags, input.Filters) {
			continue
		}
		out = append(out, &ec2.RouteTable{
			RouteTableId: aws.String(rt.id),
			VpcId:        aws.String(rt.vpc),
			Tags:         ec2TagsFromMap(rt.tags),
			Associations: rt.assocs,
		})
	}
	return &ec2.DescribeRouteTablesOutput{RouteTables: out}, nil
}

func (f *fakeRouteTableProvisioner) CreateRouteTable(_ context.Context, input *ec2.CreateRouteTableInput, _ string) (*ec2.CreateRouteTableOutput, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextRT++
	rt := &fakeCPRouteTable{
		id:   fmt.Sprintf("rtb-cp%04d", f.nextRT),
		vpc:  aws.StringValue(input.VpcId),
		tags: cpvpcTagsFromSpecs(input.TagSpecifications),
	}
	f.tables = append(f.tables, rt)
	return &ec2.CreateRouteTableOutput{RouteTable: &ec2.RouteTable{RouteTableId: aws.String(rt.id), VpcId: input.VpcId}}, nil
}

func (f *fakeRouteTableProvisioner) DeleteRouteTable(_ context.Context, input *ec2.DeleteRouteTableInput, _ string) (*ec2.DeleteRouteTableOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	id := aws.StringValue(input.RouteTableId)
	kept := f.tables[:0]
	for _, rt := range f.tables {
		if rt.id != id {
			kept = append(kept, rt)
		}
	}
	f.tables = kept
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *fakeRouteTableProvisioner) CreateRoute(_ context.Context, input *ec2.CreateRouteInput, _ string) (*ec2.CreateRouteOutput, error) {
	f.createRoutes = append(f.createRoutes, input)
	return &ec2.CreateRouteOutput{Return: aws.Bool(true)}, nil
}

func (f *fakeRouteTableProvisioner) AssociateRouteTable(_ context.Context, input *ec2.AssociateRouteTableInput, _ string) (*ec2.AssociateRouteTableOutput, error) {
	f.assocCalls = append(f.assocCalls, input)
	f.nextAssoc++
	assocID := fmt.Sprintf("rtbassoc-cp%04d", f.nextAssoc)
	if rt := f.find(aws.StringValue(input.RouteTableId)); rt != nil {
		rt.assocs = append(rt.assocs, &ec2.RouteTableAssociation{
			RouteTableAssociationId: aws.String(assocID),
			RouteTableId:            input.RouteTableId,
			SubnetId:                input.SubnetId,
			Main:                    aws.Bool(false),
		})
	}
	return &ec2.AssociateRouteTableOutput{AssociationId: aws.String(assocID)}, nil
}

func (f *fakeRouteTableProvisioner) DisassociateRouteTable(_ context.Context, input *ec2.DisassociateRouteTableInput, _ string) (*ec2.DisassociateRouteTableOutput, error) {
	f.disassoc = append(f.disassoc, input)
	id := aws.StringValue(input.AssociationId)
	for _, rt := range f.tables {
		kept := rt.assocs[:0]
		for _, a := range rt.assocs {
			if aws.StringValue(a.RouteTableAssociationId) != id {
				kept = append(kept, a)
			}
		}
		rt.assocs = kept
	}
	return &ec2.DisassociateRouteTableOutput{}, nil
}

// --- NAT gateway ---

type fakeCPNatGateway struct {
	id    string
	tags  map[string]string
	state string
	addrs []*ec2.NatGatewayAddress
}

type fakeNatGatewayProvisioner struct {
	gws     []*fakeCPNatGateway
	nextNGW int

	createCalls []*ec2.CreateNatGatewayInput
	deleteCalls []*ec2.DeleteNatGatewayInput

	createErr error

	// routeGuard, when set, models the live rule #3 guard: DeleteNatGateway is
	// blocked (DependencyViolation) while any route table in routeGuard still
	// exists — i.e. while a route table can forward to this NAT GW.
	routeGuard *fakeRouteTableProvisioner
}

func newFakeNatGatewayProvisioner() *fakeNatGatewayProvisioner { return &fakeNatGatewayProvisioner{} }

func (f *fakeNatGatewayProvisioner) DescribeNatGateways(_ context.Context, input *ec2.DescribeNatGatewaysInput, _ string) (*ec2.DescribeNatGatewaysOutput, error) {
	var out []*ec2.NatGateway
	for _, ng := range f.gws {
		if !cpvpcMatchTagFilters(ng.tags, input.Filter) {
			continue
		}
		if !cpvpcMatchScalarFilter(input.Filter, "state", ng.state) {
			continue
		}
		out = append(out, &ec2.NatGateway{
			NatGatewayId:        aws.String(ng.id),
			State:               aws.String(ng.state),
			Tags:                ec2TagsFromMap(ng.tags),
			NatGatewayAddresses: ng.addrs,
		})
	}
	return &ec2.DescribeNatGatewaysOutput{NatGateways: out}, nil
}

func (f *fakeNatGatewayProvisioner) CreateNatGateway(_ context.Context, input *ec2.CreateNatGatewayInput, _ string) (*ec2.CreateNatGatewayOutput, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextNGW++
	ng := &fakeCPNatGateway{
		id:    fmt.Sprintf("nat-cp%04d", f.nextNGW),
		tags:  cpvpcTagsFromSpecs(input.TagSpecifications),
		state: "available",
		addrs: []*ec2.NatGatewayAddress{{
			AllocationId: input.AllocationId,
			PublicIp:     aws.String("203.0.113.50"),
		}},
	}
	f.gws = append(f.gws, ng)
	return &ec2.CreateNatGatewayOutput{NatGateway: &ec2.NatGateway{NatGatewayId: aws.String(ng.id), State: aws.String(ng.state)}}, nil
}

func (f *fakeNatGatewayProvisioner) DeleteNatGateway(_ context.Context, input *ec2.DeleteNatGatewayInput, _ string) (*ec2.DeleteNatGatewayOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	if f.routeGuard != nil && len(f.routeGuard.tables) > 0 {
		return nil, errors.New(awserrors.ErrorDependencyViolation)
	}
	id := aws.StringValue(input.NatGatewayId)
	for _, ng := range f.gws {
		if ng.id == id {
			ng.state = "deleted"
		}
	}
	return &ec2.DeleteNatGatewayOutput{}, nil
}

// --- Address-space ownership ---

// TestCPVPCTagValuesAreFrozen pins the values live CP VPCs already carry. Every
// other test in the package seeds its fake and filters through cpVPCRoles, so
// only a literal assertion catches a change to the prefix or the supernet — and
// a change to either orphans every deployed cluster from its own teardown,
// stranding a billable NAT gateway and hanging DeleteCluster in DELETING.
func TestCPVPCTagValuesAreFrozen(t *testing.T) {
	assert.Equal(t, handlers_systemvpc.Roles{
		VPC:           "cp-vpc",
		PublicSubnet:  "cp-public",
		PrivateSubnet: "cp-private",
		PublicRT:      "cp-public-rt",
		PrivateRT:     "cp-private-rt",
		NatGW:         "cp-natgw",
	}, cpVPCRoles, "these role tags are baked into live deployments; DeleteClusterCPVPC and EKSBillableReaper find resources by them")
	assert.Equal(t, "10.252.0.0/14", cpVPCSupernet, "existing clusters' CIDRs are hashed out of this block")
}

func TestCPVPCSupernetIsDisjointFromOtherComponents(t *testing.T) {
	supernet, err := netip.ParsePrefix(cpVPCSupernet)
	require.NoError(t, err)
	assert.Equal(t, supernet.Masked(), supernet, "a supernet with host bits set is rejected by the builder")
	assert.Equal(t, 14, supernet.Bits(), "the builder carves a /22 out of a /14 and requires that shape")

	// Every other component that builds a system VPC out of the same builder
	// gets its own block, so an operator can tell from an address which
	// component owns it.
	rds := netip.MustParsePrefix(config.RDSDefaultSystemVPCSupernet)
	assert.False(t, supernet.Overlaps(rds),
		"the EKS control-plane space %s must not overlap the RDS system VPC space %s", supernet, rds)
}
