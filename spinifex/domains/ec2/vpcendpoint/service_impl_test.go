package vpcendpoint

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreate_RendersAWSGatewayShape(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), RouteTableIds: aws.StringSlice([]string{rtbA2, rtbA1, rtbA1}),
		ClientToken: aws.String("tok"),
	}, testAccount)
	require.NoError(t, err)
	e := out.VpcEndpoint
	assert.Equal(t, "tok", aws.StringValue(out.ClientToken))
	assert.True(t, strings.HasPrefix(aws.StringValue(e.VpcEndpointId), "vpce-"))
	assert.Len(t, strings.TrimPrefix(aws.StringValue(e.VpcEndpointId), "vpce-"), 17)
	assert.Equal(t, StateAvailable, aws.StringValue(e.State), "AWS returns a gateway endpoint available")
	assert.Equal(t, TypeGateway, aws.StringValue(e.VpcEndpointType))
	assert.Equal(t, []string{rtbA1, rtbA2}, aws.StringValueSlice(e.RouteTableIds), "a table named twice is taken once")
	assert.Equal(t, DefaultPolicy, aws.StringValue(e.PolicyDocument))
	assert.Equal(t, ec2.DnsRecordIpTypeServiceDefined, aws.StringValue(e.DnsOptions.DnsRecordIpType))
	assert.Equal(t, "ipv4", aws.StringValue(e.IpAddressType))
	assert.Equal(t, testAccount, aws.StringValue(e.OwnerId))
	assert.False(t, aws.BoolValue(e.PrivateDnsEnabled))
	assert.False(t, aws.BoolValue(e.RequesterManaged))
	assert.NotNil(t, e.SubnetIds)
	assert.NotNil(t, e.Groups)
	assert.NotNil(t, e.NetworkInterfaceIds)
	assert.NotNil(t, e.DnsEntries)
	assert.Nil(t, e.Tags, "create renders Tags only when the request tagged the endpoint")

	d := f.describe(t, aws.StringValue(e.VpcEndpointId))
	assert.NotNil(t, d.Tags, "describe always renders Tags")
	assert.Equal(t, []string{rtbA1, rtbA2}, aws.StringValueSlice(d.RouteTableIds))
}

func TestCreate_TagsAndDnsOptions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service),
		DnsOptions: &ec2.DnsOptionsSpecification{DnsRecordIpType: aws.String("ipv4")},
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeVpcEndpoint),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("s3")}},
		}},
	}, testAccount)
	require.NoError(t, err)
	assert.Equal(t, "ipv4", aws.StringValue(out.VpcEndpoint.DnsOptions.DnsRecordIpType), "AWS echoes the given DnsRecordIpType")
	require.Len(t, out.VpcEndpoint.Tags, 1)
	assert.Equal(t, "s3", aws.StringValue(out.VpcEndpoint.Tags[0].Value))
	assert.Empty(t, out.VpcEndpoint.RouteTableIds, "an endpoint with no tables is accepted")
}

func TestCreate_Refusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        ec2.CreateVpcEndpointInput
		code, msg string
	}{
		{"no VPC", ec2.CreateVpcEndpointInput{ServiceName: aws.String(s3Service)},
			awserrors.ErrorInvalidParameter, "vpc_id is a required member"},
		{"unknown VPC", ec2.CreateVpcEndpointInput{VpcId: aws.String("vpc-0f000000000000009"), ServiceName: aws.String(s3Service)},
			awserrors.ErrorInvalidVpcIdNotFound, "The Vpc Id vpc-0f000000000000009 does not exist"},
		{"malformed VPC", ec2.CreateVpcEndpointInput{VpcId: aws.String("vpc-bogus"), ServiceName: aws.String(s3Service)},
			awserrors.ErrorInvalidVpcIdNotFound, "The Vpc Id vpc-bogus does not exist"},
		{"lowercase type", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), VpcEndpointType: aws.String("gateway")},
			awserrors.ErrorInvalidVpcEndpointType, "Unrecognized VPC endpoint type gateway, expected one of [Gateway, Interface, GatewayLoadBalancer]"},
		{"unknown service", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String("com.amazonaws.ap-southeast-2.nonexistent")},
			awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service 'com.amazonaws.ap-southeast-2.nonexistent' does not exist"},
		{"other Region's S3", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String("com.amazonaws.us-east-1.s3")},
			awserrors.ErrorInvalidParameter, "Endpoint type (Gateway) does not match available service types ([Interface])."},
		{"interface-only service", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String("com.amazonaws.ap-southeast-2.sts")},
			awserrors.ErrorInvalidParameter, "Endpoint type (Gateway) does not match available service types ([Interface])."},
		{"interface endpoint for an interface-only service", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String("com.amazonaws.ap-southeast-2.sts"), VpcEndpointType: aws.String("Interface")},
			awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service 'com.amazonaws.ap-southeast-2.sts' does not exist"},
		{"other Region's interface-only service", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String("com.amazonaws.us-east-1.sts")},
			awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service 'com.amazonaws.us-east-1.sts' does not exist"},
		{"interface S3", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), VpcEndpointType: aws.String("Interface")},
			awserrors.ErrorInvalidParameter, "Endpoint type (Interface) does not match available service types ([Gateway])."},
		{"subnets", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), SubnetIds: aws.StringSlice([]string{"subnet-1"})},
			awserrors.ErrorInvalidParameter, "Subnet IDs are only supported for Interface and GatewayLoadBalancer type VPC Endpoints."},
		{"groups", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), SecurityGroupIds: aws.StringSlice([]string{"sg-1"})},
			awserrors.ErrorInvalidParameter, "Security Group IDs are only supported for Interface type VPC Endpoint."},
		{"private DNS", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), PrivateDnsEnabled: aws.Bool(true)},
			awserrors.ErrorInvalidParameter, "Private DNS is only supported for Interface type VPC Endpoint."},
		{"subnet configurations", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), SubnetConfigurations: []*ec2.SubnetConfiguration{{}}},
			awserrors.ErrorInvalidParameter, "SubnetConfigurations are only supported for Interface type VPC Endpoint."},
		{"ipv6", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), IpAddressType: aws.String("ipv6")},
			awserrors.ErrorInvalidParameter, ""},
		{"dualstack", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), IpAddressType: aws.String("dualstack")},
			awserrors.ErrorInvalidParameter, ""},
		{"unknown table", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), RouteTableIds: aws.StringSlice([]string{"rtb-bogus"})},
			awserrors.ErrorInvalidRouteTableIdNotFound, "The Route Table Id 'rtb-bogus' does not exist."},
		{"table in another VPC", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), RouteTableIds: aws.StringSlice([]string{rtbB1})},
			awserrors.ErrorInvalidParameter, rtbB1 + " is not in " + vpcA},
		{"policy not JSON", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), PolicyDocument: aws.String("{not json")},
			awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy"},
		{"policy without Principal", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service),
			PolicyDocument: aws.String(`{"Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`)},
			awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy"},
		{"policy without Statement", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), PolicyDocument: aws.String(`{"Version":"2012-10-17"}`)},
			awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy"},
		{"policy too long", ec2.CreateVpcEndpointInput{VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service),
			PolicyDocument: aws.String(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*","Sid":"` + strings.Repeat("x", maxPolicyLength) + `"}]}`)},
			awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, err := f.svc.CreateVpcEndpoint(t.Context(), &tc.in, testAccount)
			requireCode(t, err, tc.code, tc.msg)
			assert.Empty(t, f.keys(t), "a refused create writes nothing")
		})
	}
}

// The Terraform provider sends PrivateDnsEnabled=false on every gateway create,
// and AWS accepts it on create and modify; only true is refused.
func TestGateway_PrivateDnsFalseIsAccepted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), RouteTableIds: aws.StringSlice([]string{rtbA1}),
		PrivateDnsEnabled: aws.Bool(false),
	}, testAccount)
	require.NoError(t, err)
	require.NotNil(t, out.VpcEndpoint.PrivateDnsEnabled, "AWS renders an explicit false")
	assert.False(t, *out.VpcEndpoint.PrivateDnsEnabled)

	id := aws.StringValue(out.VpcEndpoint.VpcEndpointId)
	mod, err := f.svc.ModifyVpcEndpoint(t.Context(), &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PrivateDnsEnabled: aws.Bool(false)}, testAccount)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(mod.Return))
	d := f.describe(t, id)
	require.NotNil(t, d.PrivateDnsEnabled, "AWS renders an explicit false")
	assert.False(t, *d.PrivateDnsEnabled)
}

func TestCreate_DefaultPolicyGivenExplicitly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, doc := range []string{
		DefaultPolicy,
		`{"Version":"2012-10-17","Statement":{"Principal":"*","Effect":"Allow","Action":["*"],"Resource":"*"}}`,
	} {
		out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
			VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), PolicyDocument: aws.String(doc),
		}, testAccount)
		require.NoError(t, err, doc)
		assert.Equal(t, DefaultPolicy, aws.StringValue(out.VpcEndpoint.PolicyDocument))
	}
}

func TestCreate_InactiveServiceDoesNotExist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.svc.SetActivation(nil, nil)
	_, err := f.createIn(t, vpcA, rtbA1)
	requireCode(t, err, awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service '"+s3Service+"' does not exist")
	assert.Empty(t, f.keys(t))

	out, err := f.svc.DescribeVpcEndpointServices(t.Context(), &ec2.DescribeVpcEndpointServicesInput{}, testAccount)
	require.NoError(t, err)
	assert.Empty(t, out.ServiceDetails, "the service is not listed until it is offered")
	_, err = f.svc.DescribeVpcEndpointServices(t.Context(), &ec2.DescribeVpcEndpointServicesInput{ServiceNames: aws.StringSlice([]string{s3Service})}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidServiceName, "")

	got, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{
		VpcEndpointIds: aws.StringSlice([]string{"vpce-0123456789abcdef0"}),
		Filters:        []*ec2.Filter{{Name: aws.String("no-such-filter"), Values: aws.StringSlice([]string{"x"})}},
	}, testAccount)
	require.NoError(t, err, "without activation the inventory answers as it did before endpoints existed")
	assert.Empty(t, got.VpcEndpoints)
	assert.NotNil(t, got.VpcEndpoints)
}

func TestCreate_OneEndpointRoutePerTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := f.create(t, rtbA1)
	_, err := f.createIn(t, vpcA, rtbA2, rtbA1)
	requireCode(t, err, awserrors.ErrorRouteAlreadyExists,
		"route table "+rtbA1+" already has a route with destination-prefix-list-id "+prefixlist.ID(testRegion))
	second := f.create(t, rtbA2)
	assert.NotEqual(t, first, second, "a second S3 endpoint in the VPC on another table is accepted")
	assert.Equal(t, first, f.routeOn(t, rtbA1))
	assert.Equal(t, second, f.routeOn(t, rtbA2))
}

func TestCreate_AccountLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for range accountLimit {
		f.create(t)
	}
	_, err := f.createIn(t, vpcB)
	requireCode(t, err, awserrors.ErrorVpcEndpointLimitExceeded, "The maximum number of VPC endpoints has been reached.")

	out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{}, testAccount)
	require.NoError(t, err)
	f.del(t, aws.StringValue(out.VpcEndpoints[0].VpcEndpointId))
	f.create(t)
}

func TestModify_Tables(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	other := f.create(t, rtbA3)
	gen := func() int64 {
		rec, _, err := f.svc.load(t.Context(), testAccount, id)
		require.NoError(t, err)
		return rec.Generation
	}
	g0 := gen()

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id),
		AddRouteTableIds: aws.StringSlice([]string{rtbA2}), RemoveRouteTableIds: aws.StringSlice([]string{rtbA1})}))
	assert.Equal(t, []string{rtbA2}, aws.StringValueSlice(f.describe(t, id).RouteTableIds))
	assert.Empty(t, f.routeOn(t, rtbA1), "a removed table loses the route")
	assert.Equal(t, id, f.routeOn(t, rtbA2))
	assert.Greater(t, gen(), g0, "an accepted modify advances the generation")

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA2})}),
		"adding a table the endpoint holds succeeds")
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), RemoveRouteTableIds: aws.StringSlice([]string{rtbA1})}),
		awserrors.ErrorInvalidParameter, "")
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA3})}),
		awserrors.ErrorRouteAlreadyExists, "")
	assert.Equal(t, other, f.routeOn(t, rtbA3))
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbB1})}),
		awserrors.ErrorInvalidParameter, rtbB1+" is not in "+vpcA)
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{"rtb-0f000000000000009"})}),
		awserrors.ErrorInvalidRouteTableIdNotFound, "")
}

func TestModify_Refusals(t *testing.T) {
	f := newFixture(t)
	id := f.create(t)
	cases := []struct {
		name      string
		in        ec2.ModifyVpcEndpointInput
		code, msg string
	}{
		{"unknown", ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String("vpce-0f000000000000009")},
			awserrors.ErrorInvalidVpcEndpointIdNotFound, "The Vpc Endpoint Id 'vpce-0f000000000000009' does not exist"},
		{"policy and reset", ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(DefaultPolicy), ResetPolicy: aws.Bool(true)},
			awserrors.ErrorInvalidParameter, "Please specify either a reset flag or a policy document."},
		{"subnets", ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddSubnetIds: aws.StringSlice([]string{"subnet-1"})},
			awserrors.ErrorInvalidParameter, "Subnet IDs are only supported for Interface and GatewayLoadBalancer type VPC Endpoints."},
		{"private DNS", ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PrivateDnsEnabled: aws.Bool(true)},
			awserrors.ErrorInvalidParameter, "Private DNS is only supported for Interface type VPC Endpoint."},
		{"policy not JSON", ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String("{nope")},
			awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireCode(t, f.modify(t, &tc.in), tc.code, tc.msg)
		})
	}
	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), ResetPolicy: aws.Bool(true)}))
	assert.Equal(t, DefaultPolicy, aws.StringValue(f.describe(t, id).PolicyDocument))
}

func TestDelete_BatchShape(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)

	out := f.del(t, id, "vpce-bogus", id)
	require.Len(t, out.Unsuccessful, 1, "a repeated ID is deleted once")
	u := out.Unsuccessful[0]
	assert.Equal(t, "vpce-bogus", aws.StringValue(u.ResourceId))
	assert.Equal(t, awserrors.ErrorInvalidVpcEndpointNotFound, aws.StringValue(u.Error.Code))
	assert.Equal(t, "user "+testAccount+" does not own a resource vpce-bogus", aws.StringValue(u.Error.Message))

	_, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{VpcEndpointIds: aws.StringSlice([]string{id})}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidVpcEndpointIdNotFound, "The Vpc Endpoint Id '"+id+"' does not exist")
	assert.Empty(t, f.routeOn(t, rtbA1), "a deleted endpoint's route is gone at once")
	assert.Len(t, f.del(t, id).Unsuccessful, 1, "deleting it again reports it unsuccessful")
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id)}), awserrors.ErrorInvalidVpcEndpointIdNotFound, "")

	_, err = f.svc.DeleteVpcEndpoints(t.Context(), &ec2.DeleteVpcEndpointsInput{}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidParameter, "vpc_endpoint_id is a required member")
}

func TestDelete_ImmediateReplacementOnTheSameTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	old := f.create(t, rtbA1)
	f.del(t, old)
	replacement := f.create(t, rtbA1)
	assert.Equal(t, replacement, f.routeOn(t, rtbA1), "a delete followed at once by a create on the same table succeeds, as a Terraform replacement does")
}

func TestDelete_ForeignEndpointIsNotOwned(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	out, err := f.svc.DeleteVpcEndpoints(t.Context(), &ec2.DeleteVpcEndpointsInput{VpcEndpointIds: aws.StringSlice([]string{id})}, otherAccount)
	require.NoError(t, err)
	require.Len(t, out.Unsuccessful, 1)
	assert.Equal(t, id, f.routeOn(t, rtbA1), "another account cannot delete the endpoint")
}

func TestDescribe_FiltersAndErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := f.create(t, rtbA1)
	e, err := f.createIn(t, vpcB, rtbB1)
	require.NoError(t, err)
	b := aws.StringValue(e.VpcEndpointId)
	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{Resources: aws.StringSlice([]string{b}), Tags: []*ec2.Tag{{Key: aws.String("team"), Value: aws.String("data")}}}, testAccount))

	ids := func(filters ...*ec2.Filter) []string {
		out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{Filters: filters}, testAccount)
		require.NoError(t, err)
		var got []string
		for _, e := range out.VpcEndpoints {
			got = append(got, aws.StringValue(e.VpcEndpointId))
		}
		return got
	}
	filter := func(name string, values ...string) *ec2.Filter {
		return &ec2.Filter{Name: aws.String(name), Values: aws.StringSlice(values)}
	}
	assert.ElementsMatch(t, []string{a, b}, ids())
	assert.Equal(t, []string{a}, ids(filter("vpc-id", vpcA)))
	assert.Equal(t, []string{b}, ids(filter("tag:team", "data")))
	assert.Equal(t, []string{b}, ids(filter("tag-key", "team")))
	assert.ElementsMatch(t, []string{a, b}, ids(filter("vpc-endpoint-state", "available"), filter("vpc-endpoint-type", "Gateway"), filter("service-name", s3Service)))
	assert.Empty(t, ids(filter("vpc-endpoint-type", "Interface")))
	assert.Empty(t, ids(filter("ip-address-type", "ipv6")))

	_, err = f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{Filters: []*ec2.Filter{filter("no-such-filter", "x")}}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidFilter, "Unsupported filter attribute: [no-such-filter]")
	_, err = f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{VpcEndpointIds: aws.StringSlice([]string{"vpce-bogus"})}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidVpcEndpointIdNotFound, "The Vpc Endpoint Id 'vpce-bogus' does not exist")

	out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{}, otherAccount)
	require.NoError(t, err)
	assert.Empty(t, out.VpcEndpoints, "another account sees none of the caller's endpoints")
}

func TestDescribe_DeletedTableStopsRendering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA2)
	f.tables.drop(testAccount, rtbA2)
	assert.Equal(t, []string{rtbA1}, aws.StringValueSlice(f.describe(t, id).RouteTableIds),
		"a deleted table leaves RouteTableIds at once, as on AWS")
}

func TestDescribeServices_ListsTheGatewayService(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	out, err := f.svc.DescribeVpcEndpointServices(t.Context(), &ec2.DescribeVpcEndpointServicesInput{}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.ServiceDetails, 1)
	d := out.ServiceDetails[0]
	assert.Equal(t, s3Service, aws.StringValue(d.ServiceName))
	assert.Equal(t, []string{s3Service}, aws.StringValueSlice(out.ServiceNames))
	require.Len(t, d.ServiceType, 1)
	assert.Equal(t, TypeGateway, aws.StringValue(d.ServiceType[0].ServiceType))
	assert.Equal(t, "amazon", aws.StringValue(d.Owner))
	assert.True(t, aws.BoolValue(d.VpcEndpointPolicySupported))
	assert.False(t, aws.BoolValue(d.AcceptanceRequired))
	assert.Equal(t, []string{"ipv4"}, aws.StringValueSlice(d.SupportedIpAddressTypes))
	assert.Equal(t, []string{"s3." + testRegion + ".services.internal"}, aws.StringValueSlice(d.BaseEndpointDnsNames))
	assert.Equal(t, ServiceID(testRegion), aws.StringValue(d.ServiceId))
	assert.True(t, strings.HasPrefix(aws.StringValue(d.ServiceId), "vpce-svc-"))

	empty := func(filters ...*ec2.Filter) bool {
		out, err := f.svc.DescribeVpcEndpointServices(t.Context(), &ec2.DescribeVpcEndpointServicesInput{Filters: filters}, testAccount)
		require.NoError(t, err)
		return len(out.ServiceDetails) == 0
	}
	assert.True(t, empty(&ec2.Filter{Name: aws.String("service-type"), Values: aws.StringSlice([]string{"Interface"})}),
		"an interface S3 data source finds nothing")
	assert.False(t, empty(&ec2.Filter{Name: aws.String("service-type"), Values: aws.StringSlice([]string{"Gateway"})}))
	assert.False(t, empty(&ec2.Filter{Name: aws.String("service-name"), Values: aws.StringSlice([]string{s3Service})}))

	_, err = f.svc.DescribeVpcEndpointServices(t.Context(), &ec2.DescribeVpcEndpointServicesInput{
		ServiceNames: aws.StringSlice([]string{"com.amazonaws.ap-southeast-2.nonexistent"}),
	}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service 'com.amazonaws.ap-southeast-2.nonexistent' does not exist")
}

func TestTags_MirrorAndRemove(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t)
	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{id, "vpc-0a000000000000001"}),
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccount))
	require.Len(t, f.describe(t, id).Tags, 1)
	require.NoError(t, f.svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("k")}},
	}, testAccount))
	assert.Empty(t, f.describe(t, id).Tags)
	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, otherAccount), "another account's tag on an endpoint it does not own is skipped")
	assert.Empty(t, f.describe(t, id).Tags)
}

func TestVpcBlocksDeletion(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcA)
	require.NoError(t, err)
	assert.False(t, blocked)

	id := f.create(t)
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcA)
	require.NoError(t, err)
	assert.True(t, blocked, "an endpoint with no tables still blocks DeleteVpc, as on AWS")
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcB)
	require.NoError(t, err)
	assert.False(t, blocked, "an endpoint blocks only its own VPC")

	f.del(t, id)
	blocked, err = f.svc.VpcBlocksDeletion(context.Background(), testAccount, vpcA)
	require.NoError(t, err)
	assert.False(t, blocked)
}

// A create holds the feature-level admission until its record is written, so a
// rollback waiting on in-flight work cannot pass while an endpoint is landing.
func TestCreate_HoldsAdmissionUntilWritten(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var keysAtRelease []string
	f.gate.onDone = func() { keysAtRelease = f.keys(t) }
	id := f.create(t, rtbA1)
	assert.Contains(t, keysAtRelease, recordKey(testAccount, id), "admission released before the record was written")
	assert.Zero(t, f.gate.held, "admission not released")
}

func TestCreate_AdmissionRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.gate.closed = true
	_, err := f.createIn(t, vpcA, rtbA1)
	requireCode(t, err, awserrors.ErrorInvalidServiceName, "")

	f.gate.closed, f.gate.err = false, errors.New("kv down")
	_, err = f.createIn(t, vpcA, rtbA1)
	requireCode(t, err, awserrors.ErrorServerInternal, "")
	assert.Empty(t, f.keys(t), "a refused create writes nothing")
	assert.Zero(t, f.gate.held, "a refusal still releases admission")
}

// With the level closed, as during a rollback, an endpoint cannot gain a route,
// but can still lose one or change its policy, so customers can drain it.
func TestModify_AddingATableNeedsAdmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA3)
	f.gate.closed = true

	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id),
		AddRouteTableIds: aws.StringSlice([]string{rtbA2})}), awserrors.ErrorServiceUnavailable, "")
	assert.Empty(t, f.routeOn(t, rtbA2))

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id),
		RemoveRouteTableIds: aws.StringSlice([]string{rtbA3}), ResetPolicy: aws.Bool(true)}))
	assert.Equal(t, []string{rtbA1}, aws.StringValueSlice(f.describe(t, id).RouteTableIds))
	assert.Zero(t, f.gate.held)

	f.gate.closed = false
	var routeAtRelease string
	f.gate.onDone = func() { routeAtRelease = f.routeOn(t, rtbA2) }
	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id),
		AddRouteTableIds: aws.StringSlice([]string{rtbA2})}))
	assert.Equal(t, id, routeAtRelease, "admission released before the table's claim was written")
}

// The rollback drain counts every account's available endpoints and creates
// still in flight, and nothing a delete or an abandoned create left.
func TestCountLive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	count := func() int {
		n, err := CountLive(t.Context(), f.js, f.clock.now())
		require.NoError(t, err)
		return n
	}
	assert.Zero(t, count(), "an empty bucket holds none")

	f.create(t, rtbA1)
	gone := f.create(t, rtbA2)
	f.del(t, gone)
	require.NoError(t, putJSON(t.Context(), f.svc.kv, recordKey(otherAccount, "vpce-0aaaaaaaaaaaaaaa1"),
		&EndpointRecord{ID: "vpce-0aaaaaaaaaaaaaaa1", AccountID: otherAccount, State: StatePending, CreatedAt: f.clock.now()}))
	assert.Equal(t, 2, count(), "the available endpoint and the create in flight; not the deleted one")

	f.clock.t = f.clock.t.Add(claimGrace)
	assert.Equal(t, 1, count(), "a pending record past the grace is an abandoned create")
}

// readOnly allows reads of one bucket, as customers scope a gateway endpoint.
const readOnly = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",` +
	`"Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::reports","arn:aws:s3:::reports/*"]}]}`

// Once every node enforces endpoint policies, a custom policy is stored as
// written and described back, and a reset returns the default.
func TestPolicy_CustomPolicyIsStoredOnceEnforced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	policies := f.gate

	out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service), PolicyDocument: aws.String(readOnly),
	}, testAccount)
	require.NoError(t, err)
	id := aws.StringValue(out.VpcEndpoint.VpcEndpointId)
	assert.Equal(t, readOnly, aws.StringValue(out.VpcEndpoint.PolicyDocument))
	assert.Equal(t, readOnly, aws.StringValue(f.describe(t, id).PolicyDocument))

	deny := `{"Statement":{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"*"}}`
	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(deny)}))
	assert.Equal(t, deny, aws.StringValue(f.describe(t, id).PolicyDocument))

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), ResetPolicy: aws.Bool(true)}))
	assert.Equal(t, DefaultPolicy, aws.StringValue(f.describe(t, id).PolicyDocument))
	assert.Zero(t, policies.held, "admission not released")
}

// A custom policy write holds the policy admission until the policy is stored,
// so a rollback waiting on in-flight work cannot pass while one is landing.
func TestPolicy_WriteHoldsAdmissionUntilStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	policies := f.gate
	id := f.create(t, rtbA1)
	var atRelease string
	policies.onDone = func() { atRelease = aws.StringValue(f.describe(t, id).PolicyDocument) }

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(readOnly)}))
	assert.Equal(t, readOnly, atRelease, "admission released before the policy was stored")
	assert.Equal(t, 1, policies.maxHeld)

	// The default needs no admission: it is what every node already enforces.
	policies.closed = true
	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(DefaultPolicy)}))
	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), ResetPolicy: aws.Bool(true)}))
	assert.Zero(t, policies.held)
}

// While a rollback below the level is written, a custom policy is refused on
// modify and nothing is stored; a create is refused before it as for any.
func TestPolicy_RefusedUntilEveryNodeEnforces(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	policies := f.gate
	policies.closed = true

	_, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcB), ServiceName: aws.String(s3Service), PolicyDocument: aws.String(readOnly),
	}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidServiceName, "")
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(readOnly)}),
		awserrors.ErrorServiceUnavailable, "")
	assert.Equal(t, DefaultPolicy, aws.StringValue(f.describe(t, id).PolicyDocument))

	policies.closed, policies.err = false, errors.New("kv down")
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(readOnly)}),
		awserrors.ErrorServerInternal, "")
	assert.Zero(t, policies.held, "a refusal still releases admission")
	assert.Equal(t, DefaultPolicy, aws.StringValue(f.describe(t, id).PolicyDocument))
}

// A policy the S3 gate would not enforce as written is refused at write, so no
// stored policy silently fails closed there.
func TestPolicy_RefusesWhatTheGateWouldNotEnforce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	unreadable := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"},` +
		`{"Effect":"Deny","Principal":"*","Action":"*","Resource":"*","Condition":{"NotIpAddress":{"aws:VpcSourceIp":"nope"}}}]}`
	requireCode(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), PolicyDocument: aws.String(unreadable)}),
		awserrors.ErrorInvalidPolicyDocument, "")
	assert.Equal(t, DefaultPolicy, aws.StringValue(f.describe(t, id).PolicyDocument))
}
