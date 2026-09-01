package handlers_systemvpc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEC2 is an in-memory stand-in for the whole EC2 VPC family: resources
// remember their creation tags and describes honour this package's filters. It
// counts creates so idempotency is observable, and records the calling account.
type fakeEC2 struct {
	mu   sync.Mutex
	seq  int
	accs map[string]int

	vpcs    map[string]*ec2.Vpc
	subnets map[string]*ec2.Subnet
	rts     map[string]*ec2.RouteTable
	ngws    map[string]*ec2.NatGateway
	igws    map[string]*ec2.InternetGateway
	// igwIntent is igwID -> vpcID for attaches not yet confirmed.
	igwIntent map[string]string
	sgs       map[string]*ec2.SecurityGroup
	eips      map[string]string

	// routes records the default route installed on each route table, keyed by
	// route-table id, as "<target-kind>:<target-id>".
	routes map[string]string

	creates map[string]int
}

var (
	_ VPCProvisioner        = (*fakeEC2)(nil)
	_ RouteTableProvisioner = (*fakeEC2)(nil)
	_ NATGatewayProvisioner = (*fakeEC2)(nil)
	_ EIPProvisioner        = (*fakeEC2)(nil)
	_ IGWProvisioner        = (*fakeEC2)(nil)
	_ SGProvisioner         = (*fakeEC2)(nil)
)

func newFakeEC2() *fakeEC2 {
	return &fakeEC2{
		accs:      map[string]int{},
		vpcs:      map[string]*ec2.Vpc{},
		subnets:   map[string]*ec2.Subnet{},
		rts:       map[string]*ec2.RouteTable{},
		ngws:      map[string]*ec2.NatGateway{},
		igws:      map[string]*ec2.InternetGateway{},
		igwIntent: map[string]string{},
		sgs:       map[string]*ec2.SecurityGroup{},
		eips:      map[string]string{},
		routes:    map[string]string{},
		creates:   map[string]int{},
	}
}

func (f *fakeEC2) deps() Deps {
	return Deps{VPC: f, SG: f, IGW: f, RT: f, NGW: f, EIP: f}
}

// id mints a unique AWS-shaped resource id and records the create + account.
func (f *fakeEC2) id(op, prefix, accountID string) string {
	f.seq++
	f.creates[op]++
	f.accs[accountID]++
	return fmt.Sprintf("%s-%04d", prefix, f.seq)
}

// tagsFrom flattens a TagSpecification list into the tag set stored on the
// created resource.
func tagsFrom(specs []*ec2.TagSpecification) []*ec2.Tag {
	if len(specs) == 0 {
		return nil
	}
	return specs[0].Tags
}

// matches applies EC2 filter semantics: every filter must match at least one of
// the resource's values for that filter's name. "tag:<key>" reads the resource's
// tags; anything else reads attrs.
func matches(filters []*ec2.Filter, resTags []*ec2.Tag, attrs map[string]string) bool {
	for _, f := range filters {
		name := aws.StringValue(f.Name)
		var have []string
		if key, ok := strings.CutPrefix(name, "tag:"); ok {
			for _, t := range resTags {
				if aws.StringValue(t.Key) == key {
					have = append(have, aws.StringValue(t.Value))
				}
			}
		} else if v, ok := attrs[name]; ok {
			have = []string{v}
		}

		var hit bool
		for _, want := range aws.StringValueSlice(f.Values) {
			for _, got := range have {
				if got == want {
					hit = true
				}
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func (f *fakeEC2) CreateVpc(_ context.Context, in *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vpc := &ec2.Vpc{
		VpcId:     aws.String(f.id("vpc", "vpc", accountID)),
		CidrBlock: in.CidrBlock,
		Tags:      tagsFrom(in.TagSpecifications),
	}
	f.vpcs[aws.StringValue(vpc.VpcId)] = vpc
	return &ec2.CreateVpcOutput{Vpc: vpc}, nil
}

func (f *fakeEC2) DeleteVpc(_ context.Context, in *ec2.DeleteVpcInput, _ string) (*ec2.DeleteVpcOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vpcs, aws.StringValue(in.VpcId))
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ string) (*ec2.DescribeVpcsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeVpcsOutput{}
	for _, v := range f.vpcs {
		if matches(in.Filters, v.Tags, map[string]string{"vpc-id": aws.StringValue(v.VpcId)}) {
			out.Vpcs = append(out.Vpcs, v)
		}
	}
	return out, nil
}

func (f *fakeEC2) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sn := &ec2.Subnet{
		SubnetId:         aws.String(f.id("subnet", "subnet", accountID)),
		VpcId:            in.VpcId,
		CidrBlock:        in.CidrBlock,
		AvailabilityZone: in.AvailabilityZone,
		Tags:             tagsFrom(in.TagSpecifications),
	}
	f.subnets[aws.StringValue(sn.SubnetId)] = sn
	return &ec2.CreateSubnetOutput{Subnet: sn}, nil
}

func (f *fakeEC2) DeleteSubnet(_ context.Context, in *ec2.DeleteSubnetInput, _ string) (*ec2.DeleteSubnetOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subnets, aws.StringValue(in.SubnetId))
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ string) (*ec2.DescribeSubnetsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeSubnetsOutput{}
	for _, sn := range f.subnets {
		if matches(in.Filters, sn.Tags, map[string]string{
			"cidr-block": aws.StringValue(sn.CidrBlock),
			"vpc-id":     aws.StringValue(sn.VpcId),
		}) {
			out.Subnets = append(out.Subnets, sn)
		}
	}
	return out, nil
}

func (f *fakeEC2) CreateRouteTable(_ context.Context, in *ec2.CreateRouteTableInput, accountID string) (*ec2.CreateRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt := &ec2.RouteTable{
		RouteTableId: aws.String(f.id("route-table", "rtb", accountID)),
		VpcId:        in.VpcId,
		Tags:         tagsFrom(in.TagSpecifications),
	}
	f.rts[aws.StringValue(rt.RouteTableId)] = rt
	return &ec2.CreateRouteTableOutput{RouteTable: rt}, nil
}

func (f *fakeEC2) DeleteRouteTable(_ context.Context, in *ec2.DeleteRouteTableInput, _ string) (*ec2.DeleteRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rts, aws.StringValue(in.RouteTableId))
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *fakeEC2) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ string) (*ec2.DescribeRouteTablesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeRouteTablesOutput{}
	for _, rt := range f.rts {
		if matches(in.Filters, rt.Tags, map[string]string{"vpc-id": aws.StringValue(rt.VpcId)}) {
			out.RouteTables = append(out.RouteTables, rt)
		}
	}
	return out, nil
}

func (f *fakeEC2) CreateRoute(_ context.Context, in *ec2.CreateRouteInput, accountID string) (*ec2.CreateRouteOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates["route"]++
	f.accs[accountID]++
	target := "gateway:" + aws.StringValue(in.GatewayId)
	if in.NatGatewayId != nil {
		target = "natgw:" + aws.StringValue(in.NatGatewayId)
	}
	f.routes[aws.StringValue(in.RouteTableId)] = aws.StringValue(in.DestinationCidrBlock) + "->" + target
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeEC2) AssociateRouteTable(_ context.Context, in *ec2.AssociateRouteTableInput, accountID string) (*ec2.AssociateRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rt, ok := f.rts[aws.StringValue(in.RouteTableId)]
	if !ok {
		return nil, fmt.Errorf("no such route table %s", aws.StringValue(in.RouteTableId))
	}
	// The real service is AWS-faithful and rejects a subnet that already carries
	// an explicit association, on any table in the VPC. Ensure has to absorb that
	// itself, so the fake must reproduce it rather than quietly succeeding.
	for _, table := range f.rts {
		for _, a := range table.Associations {
			if aws.StringValue(a.SubnetId) == aws.StringValue(in.SubnetId) {
				return nil, errors.New(awserrors.ErrorResourceAlreadyAssociated)
			}
		}
	}
	assocID := f.id("assoc", "rtbassoc", accountID)
	rt.Associations = append(rt.Associations, &ec2.RouteTableAssociation{
		RouteTableAssociationId: aws.String(assocID),
		RouteTableId:            in.RouteTableId,
		SubnetId:                in.SubnetId,
	})
	return &ec2.AssociateRouteTableOutput{AssociationId: aws.String(assocID)}, nil
}

func (f *fakeEC2) DisassociateRouteTable(_ context.Context, in *ec2.DisassociateRouteTableInput, _ string) (*ec2.DisassociateRouteTableOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rt := range f.rts {
		kept := rt.Associations[:0]
		for _, a := range rt.Associations {
			if aws.StringValue(a.RouteTableAssociationId) != aws.StringValue(in.AssociationId) {
				kept = append(kept, a)
			}
		}
		rt.Associations = kept
	}
	return &ec2.DisassociateRouteTableOutput{}, nil
}

func (f *fakeEC2) CreateNatGateway(_ context.Context, in *ec2.CreateNatGatewayInput, accountID string) (*ec2.CreateNatGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ng := &ec2.NatGateway{
		NatGatewayId: aws.String(f.id("natgw", "nat", accountID)),
		SubnetId:     in.SubnetId,
		State:        aws.String("available"),
		Tags:         tagsFrom(in.TagSpecifications),
		NatGatewayAddresses: []*ec2.NatGatewayAddress{{
			AllocationId: in.AllocationId,
			PublicIp:     aws.String(f.eips[aws.StringValue(in.AllocationId)]),
		}},
	}
	f.ngws[aws.StringValue(ng.NatGatewayId)] = ng
	return &ec2.CreateNatGatewayOutput{NatGateway: ng}, nil
}

func (f *fakeEC2) DeleteNatGateway(_ context.Context, in *ec2.DeleteNatGatewayInput, _ string) (*ec2.DeleteNatGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ng, ok := f.ngws[aws.StringValue(in.NatGatewayId)]; ok {
		ng.State = aws.String("deleted")
	}
	return &ec2.DeleteNatGatewayOutput{}, nil
}

func (f *fakeEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ string) (*ec2.DescribeNatGatewaysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeNatGatewaysOutput{}
	for _, ng := range f.ngws {
		if matches(in.Filter, ng.Tags, map[string]string{"state": aws.StringValue(ng.State)}) {
			out.NatGateways = append(out.NatGateways, ng)
		}
	}
	return out, nil
}

func (f *fakeEC2) AllocateAddress(_ context.Context, _ *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	alloc := f.id("eip", "eipalloc", accountID)
	ip := fmt.Sprintf("198.51.100.%d", len(f.eips)+1)
	f.eips[alloc] = ip
	return &ec2.AllocateAddressOutput{AllocationId: aws.String(alloc), PublicIp: aws.String(ip)}, nil
}

func (f *fakeEC2) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ string) (*ec2.ReleaseAddressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.eips, aws.StringValue(in.AllocationId))
	return &ec2.ReleaseAddressOutput{}, nil
}

func (f *fakeEC2) CreateInternetGateway(_ context.Context, in *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	igw := &ec2.InternetGateway{
		InternetGatewayId: aws.String(f.id("igw", "igw", accountID)),
		Tags:              tagsFrom(in.TagSpecifications),
	}
	f.igws[aws.StringValue(igw.InternetGatewayId)] = igw
	return &ec2.CreateInternetGatewayOutput{InternetGateway: igw}, nil
}

// Attaching is asynchronous in production: the record names the VPC straight
// away, but DescribeInternetGateways reports no attachment until a reconcile
// pass confirms it. The fake models both phases, so code that reads the
// describe projection where it needs intent fails here rather than in a nightly.
func (f *fakeEC2) AttachInternetGateway(_ context.Context, in *ec2.AttachInternetGatewayInput, _ string) (*ec2.AttachInternetGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	igwID := aws.StringValue(in.InternetGatewayId)
	if _, ok := f.igws[igwID]; !ok {
		return nil, fmt.Errorf("no such igw %s", igwID)
	}
	if f.igwIntent == nil {
		f.igwIntent = map[string]string{}
	}
	f.igwIntent[igwID] = aws.StringValue(in.VpcId)
	return &ec2.AttachInternetGatewayOutput{}, nil
}

// confirmIGW is the reconcile pass: it makes the pending attach observable.
func (f *fakeEC2) confirmIGW(igwID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if igw, ok := f.igws[igwID]; ok {
		igw.Attachments = []*ec2.InternetGatewayAttachment{
			{VpcId: aws.String(f.igwIntent[igwID]), State: aws.String("available")},
		}
	}
}

func (f *fakeEC2) AttachmentIntent(_ context.Context, _, vpcID string) (*ec2.InternetGateway, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for igwID, attached := range f.igwIntent {
		if attached != vpcID {
			continue
		}
		igw, ok := f.igws[igwID]
		if !ok {
			continue
		}
		out := *igw
		out.Attachments = []*ec2.InternetGatewayAttachment{
			{VpcId: aws.String(vpcID), State: aws.String("available")},
		}
		return &out, nil
	}
	return nil, nil
}

func (f *fakeEC2) DetachInternetGateway(_ context.Context, in *ec2.DetachInternetGatewayInput, _ string) (*ec2.DetachInternetGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	igwID := aws.StringValue(in.InternetGatewayId)
	if igw, ok := f.igws[igwID]; ok {
		igw.Attachments = nil
	}
	delete(f.igwIntent, igwID)
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *fakeEC2) DeleteInternetGateway(_ context.Context, in *ec2.DeleteInternetGatewayInput, _ string) (*ec2.DeleteInternetGatewayOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.igws, aws.StringValue(in.InternetGatewayId))
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

func (f *fakeEC2) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ string) (*ec2.DescribeInternetGatewaysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeInternetGatewaysOutput{}
	for _, igw := range f.igws {
		var vpcID string
		if len(igw.Attachments) > 0 {
			vpcID = aws.StringValue(igw.Attachments[0].VpcId)
		}
		if matches(in.Filters, igw.Tags, map[string]string{"attachment.vpc-id": vpcID}) {
			out.InternetGateways = append(out.InternetGateways, igw)
		}
	}
	return out, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ string) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ec2.DescribeSecurityGroupsOutput{}
	for _, sg := range f.sgs {
		if matches(in.Filters, sg.Tags, map[string]string{"vpc-id": aws.StringValue(sg.VpcId)}) {
			out.SecurityGroups = append(out.SecurityGroups, sg)
		}
	}
	return out, nil
}

func (f *fakeEC2) DeleteSecurityGroup(_ context.Context, in *ec2.DeleteSecurityGroupInput, _ string) (*ec2.DeleteSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sgs, aws.StringValue(in.GroupId))
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// testSpec is a two-private-subnet spec for an arbitrary component, distinct in
// every tag key from the second owner used by the isolation test.
func testSpec(name string) Spec {
	return Spec{
		Owner: Owner{
			Name:        name,
			ManagedBy:   tags.ManagedByEKS,
			OwnerTagKey: "spinifex:test-owner",
			RoleTagKey:  "spinifex:test-role",
		},
		Region:         "ap-southeast-2",
		RolePrefix:     "tst",
		Supernet:       "10.252.0.0/14",
		PrivateSubnets: 2,
	}
}

// tagValue reads one tag off a resource's tag set.
func tagValue(resTags []*ec2.Tag, key string) string {
	for _, t := range resTags {
		if aws.StringValue(t.Key) == key {
			return aws.StringValue(t.Value)
		}
	}
	return ""
}

func TestSpecRolesAreNamespacedByPrefix(t *testing.T) {
	roles := Spec{RolePrefix: "rds"}.Roles()
	assert.Equal(t, Roles{
		VPC:           "rds-vpc",
		PublicSubnet:  "rds-public",
		PrivateSubnet: "rds-private",
		PublicRT:      "rds-public-rt",
		PrivateRT:     "rds-private-rt",
		NatGW:         "rds-natgw",
	}, roles, "role values must be prefix-namespaced, so two components' system VPCs stay distinguishable in one deployment")
}

func TestSpecCIDRsAreDeterministicAndCarved(t *testing.T) {
	spec := testSpec("cp-demo")

	vpcCIDR, publicCIDR, privateCIDRs, err := spec.cidrs()
	require.NoError(t, err)

	again, _, againPrivate, err := spec.cidrs()
	require.NoError(t, err)
	assert.Equal(t, vpcCIDR, again, "the address space is derived from the name, so it must survive a process restart unchanged")
	assert.Equal(t, privateCIDRs, againPrivate)

	assert.True(t, strings.HasSuffix(vpcCIDR, "/22"), "each system VPC gets a /22 out of the supernet, got %s", vpcCIDR)
	assert.Equal(t, strings.TrimSuffix(vpcCIDR, "/22")+"/24", publicCIDR, "the public subnet is the /22's first /24")
	assert.Len(t, privateCIDRs, 2, "PrivateSubnets is honoured")
	assert.NotContains(t, privateCIDRs, publicCIDR, "the private subnets must not overlap the public one")

	// The whole /22 is inside the supernet: with a /14 base of 10.252, the
	// second octet can only run 252..255.
	assert.True(t, strings.HasPrefix(vpcCIDR, "10.25"), "the /22 must stay inside the 10.252.0.0/14 supernet, got %s", vpcCIDR)

	other, _, _, err := testSpec("cp-other").cidrs()
	require.NoError(t, err)
	assert.NotEqual(t, vpcCIDR, other, "different names must hash to different blocks")
}

func TestSpecPrivateSubnetCountIsClamped(t *testing.T) {
	spec := testSpec("clamp")

	spec.PrivateSubnets = 0
	_, _, none, err := spec.cidrs()
	require.NoError(t, err)
	assert.Len(t, none, 1, "an unset count still yields the one private subnet system VMs are placed in")

	spec.PrivateSubnets = 99
	_, _, many, err := spec.cidrs()
	require.NoError(t, err)
	assert.Len(t, many, maxPrivateSubnets, "the count is bounded by the /24s the /22 has room for")
}

func TestSpecValidateRejectsUnusableSpecs(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"no name":        func(s *Spec) { s.Name = "" },
		"no managed-by":  func(s *Spec) { s.ManagedBy = "" },
		"no owner tag":   func(s *Spec) { s.OwnerTagKey = "" },
		"no role tag":    func(s *Spec) { s.RoleTagKey = "" },
		"no role prefix": func(s *Spec) { s.RolePrefix = "" },
		"bad supernet":   func(s *Spec) { s.Supernet = "not-a-cidr" },
		// A supernet that is not a /14 would let the 256-block hash index walk
		// out of the component's allotted space and into a neighbour's.
		"wrong prefix length": func(s *Spec) { s.Supernet = "10.252.0.0/16" },
		"host bits set":       func(s *Spec) { s.Supernet = "10.252.0.1/14" },
		"ipv6 supernet":       func(s *Spec) { s.Supernet = "fd00::/14" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec("valid")
			mutate(&spec)
			assert.Error(t, spec.validate())

			// Nothing may be created for a spec that cannot address or tag itself.
			f := newFakeEC2()
			_, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
			assert.Error(t, err)
			assert.Empty(t, f.creates, "a rejected spec must not have created anything")
		})
	}
}

func TestEnsureBuildsTheFullTopology(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	refs, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)

	assert.NotEmpty(t, refs.VpcID)
	assert.NotEmpty(t, refs.IGWID)
	assert.NotEmpty(t, refs.PublicSubnetID)
	assert.Len(t, refs.PrivateSubnetIDs, 2)
	assert.NotEmpty(t, refs.NatGatewayID)
	assert.Equal(t, f.eips[refs.NatEIPAllocationID], refs.NatEIPPublicIP, "the reported NAT address must be the EIP that was allocated")

	// Public egress is via the IGW, private egress via the NAT gateway. These
	// two routes are what the OVN topology subscribers key the per-subnet egress
	// policy off, so they are the point of building the VPC through the EC2 APIs.
	assert.Equal(t, "0.0.0.0/0->gateway:"+refs.IGWID, f.routes[refs.PublicRouteTableID])
	assert.Equal(t, "0.0.0.0/0->natgw:"+refs.NatGatewayID, f.routes[refs.PrivateRouteTableID])

	privRT := f.rts[refs.PrivateRouteTableID]
	require.Len(t, privRT.Associations, 2, "every private subnet must be associated, or a VM placed there has no egress")

	// Ownership: the tags are the only handle a later Ensure, Delete or reaper
	// has on these resources.
	vpc := f.vpcs[refs.VpcID]
	assert.Equal(t, tags.ManagedByEKS, tagValue(vpc.Tags, tags.ManagedByKey))
	assert.Equal(t, "cp-demo", tagValue(vpc.Tags, spec.OwnerTagKey))
	assert.Equal(t, spec.Roles().VPC, tagValue(vpc.Tags, spec.RoleTagKey))
	assert.Equal(t, refs.VpcCIDR, aws.StringValue(vpc.CidrBlock))

	assert.Equal(t, spec.az(0), aws.StringValue(f.subnets[refs.PublicSubnetID].AvailabilityZone))

	// Everything was created under the account Ensure was given. A resource that
	// landed in another account could not be found again by any later describe,
	// which all run as the system account.
	assert.Equal(t, []string{"000000000000"}, slices.Collect(maps.Keys(f.accs)))
}

// Ensure attaches an IGW and then re-reads it in the next breath. Attaching is
// asynchronous, so that lookup has to see the request rather than waiting for a
// reconcile pass, or every first-time EKS/RDS/Bedrock VPC fails to build.
func TestEnsureSucceedsWhileTheAttachIsUnconfirmed(t *testing.T) {
	f := newFakeEC2()

	refs, err := Ensure(t.Context(), f.deps(), testSpec("cp-demo"), "000000000000")
	require.NoError(t, err, "Ensure must not require the attach to be confirmed first")
	require.NotEmpty(t, refs.IGWID)

	// The AWS-facing projection still reports nothing, which is the point:
	// Ensure is reading intent, not the confirmed attachment.
	out, err := f.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, "000000000000")
	require.NoError(t, err)
	require.Len(t, out.InternetGateways, 1)
	assert.Empty(t, out.InternetGateways[0].Attachments, "an unconfirmed attach must not be reported")

	f.confirmIGW(refs.IGWID)
	out, err = f.DescribeInternetGateways(t.Context(), &ec2.DescribeInternetGatewaysInput{}, "000000000000")
	require.NoError(t, err)
	require.Len(t, out.InternetGateways, 1)
	assert.Len(t, out.InternetGateways[0].Attachments, 1, "confirming makes the attachment observable")
}

func TestEnsureIsIdempotent(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	first, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)
	createsAfterFirst := maps.Clone(f.creates)

	second, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)

	assert.Equal(t, first, second, "a re-run must converge on the same resources rather than build a parallel set")
	assert.Equal(t, createsAfterFirst, f.creates, "a re-run must create nothing")
	assert.Len(t, f.rts[first.PrivateRouteTableID].Associations, 2, "re-associating must not duplicate associations")
}

func TestEnsureRejectsASubnetParkedOnAnotherRouteTable(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	first, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)

	// Move a private subnet onto the public table, as an operator or a competing
	// component sharing this VPC could. AssociateRouteTable reports the subnet as
	// already associated either way, so tolerating that blindly would report the
	// VPC ready while the subnet has no route to the NAT gateway.
	privRT, pubRT := f.rts[first.PrivateRouteTableID], f.rts[first.PublicRouteTableID]
	moved := privRT.Associations[0]
	privRT.Associations = privRT.Associations[1:]
	pubRT.Associations = append(pubRT.Associations, moved)

	_, err = Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.Error(t, err, "a private subnet on someone else's route table must fail loudly, not be reported ready without egress")
	assert.ErrorContains(t, err, aws.StringValue(moved.SubnetId))
}

func TestEnsureConvergesAfterPartialFailure(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	// A create that died after the VPC and its IGW: the second call must adopt
	// them by tag rather than build a second VPC in a second address block.
	vpcID, err := ensureVPC(t.Context(), f, spec, "000000000000", "10.253.0.0/22")
	require.NoError(t, err)
	require.NoError(t, EnsureIGW(t.Context(), f, spec.Owner, "000000000000", vpcID))

	refs, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)
	assert.Equal(t, vpcID, refs.VpcID, "the half-built VPC must be adopted, not duplicated")
	assert.Equal(t, 1, f.creates["vpc"])
	assert.Equal(t, 1, f.creates["igw"])
}

func TestEnsureOwnersDoNotSeeEachOther(t *testing.T) {
	f := newFakeEC2()

	eksSpec := testSpec("cp-demo")
	rdsSpec := Spec{
		Owner: Owner{
			Name:        "rds-system-ap-southeast-2",
			ManagedBy:   tags.ManagedByRDS,
			OwnerTagKey: "spinifex:rds-system-vpc",
			RoleTagKey:  "spinifex:rds-role",
		},
		Region:         "ap-southeast-2",
		RolePrefix:     "rds",
		Supernet:       "10.248.0.0/14",
		PrivateSubnets: 1,
	}

	eksRefs, err := Ensure(t.Context(), f.deps(), eksSpec, "000000000000")
	require.NoError(t, err)
	rdsRefs, err := Ensure(t.Context(), f.deps(), rdsSpec, "000000000000")
	require.NoError(t, err)

	assert.NotEqual(t, eksRefs.VpcID, rdsRefs.VpcID, "distinct owners must get distinct VPCs")
	assert.NotEqual(t, eksRefs.NatGatewayID, rdsRefs.NatGatewayID)
	assert.False(t, strings.HasPrefix(rdsRefs.VpcCIDR, "10.252."), "the components' supernets must not overlap, or an address cannot name its owner")

	// One owner's teardown must leave the other's topology untouched — this is
	// the property that makes a shared builder safe for two components.
	require.NoError(t, Delete(t.Context(), f.deps(), eksSpec, "000000000000", eksRefs.VpcID))

	assert.NotContains(t, f.vpcs, eksRefs.VpcID)
	assert.Contains(t, f.vpcs, rdsRefs.VpcID, "deleting the EKS system VPC must not reach the RDS one")
	assert.Contains(t, f.subnets, rdsRefs.PrivateSubnetIDs[0])
	assert.Contains(t, f.rts, rdsRefs.PrivateRouteTableID)
	assert.Contains(t, f.igws, rdsRefs.IGWID)
	assert.Equal(t, "available", aws.StringValue(f.ngws[rdsRefs.NatGatewayID].State))

	// And the deleted owner's own re-Ensure rebuilds from scratch.
	rebuilt, err := Ensure(t.Context(), f.deps(), eksSpec, "000000000000")
	require.NoError(t, err)
	assert.NotEqual(t, eksRefs.VpcID, rebuilt.VpcID)
}

func TestDeleteReclaimsTheBillableAddress(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	refs, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)
	require.Contains(t, f.eips, refs.NatEIPAllocationID)

	require.NoError(t, Delete(t.Context(), f.deps(), spec, "000000000000", refs.VpcID))

	assert.Equal(t, "deleted", aws.StringValue(f.ngws[refs.NatGatewayID].State))
	assert.NotContains(t, f.eips, refs.NatEIPAllocationID, "an EIP left allocated after teardown bills forever")
	assert.Empty(t, f.rts, "route tables must go before the NAT gateway they forward to")
	assert.Empty(t, f.subnets)
	assert.Empty(t, f.vpcs)
}

// TestDeleteSweepsResidualSecurityGroups locks in the residual-SG sweep in
// Delete (systemvpc.go): DeleteVpc rejects a VPC that still owns a non-default
// SG, and a partial create can leave one untagged so the role-tagged teardown
// never sees it. The sweep instead lists by vpc-id and removes everything
// except "default", which cannot be deleted on its own and goes with the VPC
// cascade.
func TestDeleteSweepsResidualSecurityGroups(t *testing.T) {
	f := newFakeEC2()
	spec := testSpec("cp-demo")

	refs, err := Ensure(t.Context(), f.deps(), spec, "000000000000")
	require.NoError(t, err)

	defaultSG := &ec2.SecurityGroup{
		GroupId:   aws.String("sg-default-0001"),
		GroupName: aws.String("default"),
		VpcId:     aws.String(refs.VpcID),
	}
	f.sgs[aws.StringValue(defaultSG.GroupId)] = defaultSG

	residualSG := &ec2.SecurityGroup{
		GroupId:   aws.String("sg-residual-0001"),
		GroupName: aws.String("web"),
		VpcId:     aws.String(refs.VpcID),
	}
	f.sgs[aws.StringValue(residualSG.GroupId)] = residualSG

	require.NoError(t, Delete(t.Context(), f.deps(), spec, "000000000000", refs.VpcID))

	assert.NotContains(t, f.sgs, aws.StringValue(residualSG.GroupId),
		"a non-default SG the tagged sweep missed must still be reclaimed")
	assert.Contains(t, f.sgs, aws.StringValue(defaultSG.GroupId),
		"the default SG is not deletable on its own; it goes with the VPC cascade")
}

func TestDeleteOnNothingProvisionedSucceeds(t *testing.T) {
	f := newFakeEC2()
	// Teardown is re-driven on a timer, so a delete after a successful delete —
	// or one that never had anything to delete — must be a quiet success.
	assert.NoError(t, Delete(t.Context(), f.deps(), testSpec("cp-demo"), "000000000000", ""))
}

func TestEnsureReleasesTheEIPWhenTheNATGatewayFails(t *testing.T) {
	f := newFakeEC2()
	deps := f.deps()
	deps.NGW = failingNGW{f}

	_, err := Ensure(t.Context(), deps, testSpec("cp-demo"), "000000000000")
	require.Error(t, err)
	assert.Empty(t, f.eips, "an EIP allocated for a NAT gateway that never came up must not be stranded")
}

// failingNGW passes describes through to the fake but refuses to create, which
// is the window in which an already-allocated EIP can be orphaned.
type failingNGW struct{ *fakeEC2 }

var _ NATGatewayProvisioner = failingNGW{}

func (failingNGW) CreateNatGateway(context.Context, *ec2.CreateNatGatewayInput, string) (*ec2.CreateNatGatewayOutput, error) {
	return nil, fmt.Errorf("nat gateway capacity exhausted")
}

func TestEnsureIGWReusesAnExistingGatewayUntagged(t *testing.T) {
	f := newFakeEC2()
	owner := testSpec("cp-demo").Owner

	// A customer-provisioned IGW already on the VPC: adopted as-is, never
	// retagged, so DeleteIGW later declines to remove somebody else's gateway.
	created, err := f.CreateInternetGateway(t.Context(), &ec2.CreateInternetGatewayInput{}, "000000000000")
	require.NoError(t, err)
	igwID := aws.StringValue(created.InternetGateway.InternetGatewayId)
	_, err = f.AttachInternetGateway(t.Context(), &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String("vpc-customer"),
	}, "000000000000")
	require.NoError(t, err)

	require.NoError(t, EnsureIGW(t.Context(), f, owner, "000000000000", "vpc-customer"))
	assert.Equal(t, 1, f.creates["igw"], "an attached IGW must be reused, not supplemented")

	require.NoError(t, DeleteIGW(t.Context(), f, owner, "000000000000", "vpc-customer"))
	assert.Contains(t, f.igws, igwID, "an IGW this owner did not create must survive its teardown")
}

func TestEnsureIGWRejectsAnUnidentifiedRequest(t *testing.T) {
	f := newFakeEC2()
	owner := testSpec("cp-demo").Owner

	assert.Error(t, EnsureIGW(t.Context(), f, owner, "000000000000", ""))
	assert.Error(t, EnsureIGW(t.Context(), f, Owner{}, "000000000000", "vpc-1"))
	assert.Empty(t, f.creates, "an unidentified IGW could never be found again to delete")
}
