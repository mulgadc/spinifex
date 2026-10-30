package daemon

import (
	"context"
	"net/netip"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sqsTestRegion = "ap-southeast-2"

func sqsWorkerNetworkFixture(t *testing.T, cfg *config.SQSConfig) (*sqsWorkerNetwork, *ec2vpc.VPCServiceImpl) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)
	return newSQSWorkerNetwork(svc, cfg), svc
}

// The SQS default supernet is disjoint from every other system VPC default.
func TestSQSSystemVPCSpec_DefaultIsDisjointAndOverridable(t *testing.T) {
	spec := sqsSystemVPCSpec(nil, sqsTestRegion)
	sqs := netip.MustParsePrefix(spec.Supernet)
	assert.Equal(t, config.SQSDefaultSystemVPCSupernet, spec.Supernet)
	for _, other := range []string{config.RDSDefaultSystemVPCSupernet, config.BedrockDefaultSystemVPCSupernet, "10.252.0.0/14"} {
		assert.False(t, sqs.Overlaps(netip.MustParsePrefix(other)), "overlaps %s", other)
	}
	assert.Equal(t, "10.236.0.0/14", sqsSystemVPCSpec(&config.SQSConfig{SystemVPCSupernet: "10.236.0.0/14"}, sqsTestRegion).Supernet)
}

// The worker gets one ENI in the private subnet, in the VPC default security
// group, with a NIC config whose gateway is the subnet router address. The
// VPC has exactly one subnet and no internet gateway.
func TestSQSWorkerNetwork_PrivateShape(t *testing.T) {
	n, svc := sqsWorkerNetworkFixture(t, nil)
	ctx := t.Context()
	net, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.NoError(t, err)

	subnet := netip.MustParsePrefix(sqsSystemVPCSpec(nil, sqsTestRegion).Supernet)
	ip := netip.MustParseAddr(net.ENIIP)
	assert.True(t, subnet.Contains(ip))
	prefix := netip.MustParsePrefix(net.CIDR)
	assert.Equal(t, ip, prefix.Addr())
	gw, bits, err := topologyGateway(prefix)
	require.NoError(t, err)
	assert.Equal(t, gw, net.Gateway)
	assert.Equal(t, 24, bits)

	eni := describeENI(t, svc, net.ENIID)
	assert.Equal(t, net.SubnetID, aws.StringValue(eni.SubnetId))
	assert.Equal(t, net.ENIMac, aws.StringValue(eni.MacAddress))
	vpcID := aws.StringValue(eni.VpcId)
	sgs, err := svc.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
		{Name: aws.String("group-name"), Values: aws.StringSlice([]string{"default"})},
	}}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, sgs.SecurityGroups, 1)
	require.Len(t, eni.Groups, 1)
	assert.Equal(t, aws.StringValue(sgs.SecurityGroups[0].GroupId), aws.StringValue(eni.Groups[0].GroupId))

	subs, err := svc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []*ec2.Filter{
		{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})}}}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	assert.Len(t, subs.Subnets, 1)
	vpcs, err := svc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	assert.Len(t, vpcs.Vpcs, 1)
}

// A retry of a claim reuses its one ENI, concurrent first calls for a claim
// create only one, and a new claim gets its own ENI in the same subnet.
func TestSQSWorkerNetwork_OneENIPerClaim(t *testing.T) {
	n, svc := sqsWorkerNetworkFixture(t, nil)
	ctx := t.Context()
	first, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.NoError(t, err)

	again, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.NoError(t, err)
	assert.Equal(t, first, again)

	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Go(func() {
			net, err := n.WorkerNetwork(context.Background(), sqsTestRegion, "claim-c")
			if err == nil {
				got[i] = net.ENIID
			}
		})
	}
	wg.Wait()
	for _, id := range got {
		assert.NotEmpty(t, id)
		assert.Equal(t, got[0], id, "concurrent calls for one new claim must share one ENI")
	}

	other, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-b")
	require.NoError(t, err)
	assert.NotEqual(t, first.ENIID, other.ENIID)
	assert.Equal(t, first.SubnetID, other.SubnetID)

	all, err := svc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	assert.Len(t, all.NetworkInterfaces, 3)
}

// Two complete ENIs for one claim are refused rather than chosen between.
func TestSQSWorkerNetwork_RefusesADuplicatedClaim(t *testing.T) {
	n, svc := sqsWorkerNetworkFixture(t, nil)
	ctx := t.Context()
	net, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.NoError(t, err)
	_, err = svc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{SubnetId: aws.String(net.SubnetID),
		Description: aws.String("SQS worker interface for claim-a")}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	_, err = n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.ErrorContains(t, err, "refusing to choose")
}

// A caller cancelled while another allocation holds the lock gives up and
// allocates nothing.
func TestSQSWorkerNetwork_CancelledWaiterAllocatesNothing(t *testing.T) {
	n, svc := sqsWorkerNetworkFixture(t, nil)
	n.sem <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := n.WorkerNetwork(ctx, sqsTestRegion, "claim-a")
	require.ErrorIs(t, err, context.Canceled)
	<-n.sem
	vpcs, err := svc.DescribeVpcs(t.Context(), &ec2.DescribeVpcsInput{}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	assert.Empty(t, vpcs.Vpcs)
}

func TestSQSWorkerNetwork_RefusesIncompleteInput(t *testing.T) {
	n, _ := sqsWorkerNetworkFixture(t, nil)
	_, err := n.WorkerNetwork(t.Context(), "", "claim-a")
	require.Error(t, err)
	_, err = n.WorkerNetwork(t.Context(), sqsTestRegion, "")
	require.Error(t, err)
	_, err = (&sqsWorkerNetwork{}).WorkerNetwork(t.Context(), sqsTestRegion, "claim-a")
	require.Error(t, err)
}

// strayENIVPC reports every ENI at an address outside its subnet.
type strayENIVPC struct {
	*ec2vpc.VPCServiceImpl

	stray string
}

func (v strayENIVPC) DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, acct string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	out, err := v.VPCServiceImpl.DescribeNetworkInterfaces(ctx, in, acct)
	if out != nil {
		for _, e := range out.NetworkInterfaces {
			e.PrivateIpAddress = aws.String(v.stray)
		}
	}
	return out, err
}

// An ENI whose address is outside the private subnet is refused.
func TestSQSWorkerNetwork_RefusesAnAddressOutsideTheSubnet(t *testing.T) {
	n, svc := sqsWorkerNetworkFixture(t, nil)
	_, err := n.WorkerNetwork(t.Context(), sqsTestRegion, "claim-a")
	require.NoError(t, err)
	stray := newSQSWorkerNetwork(strayENIVPC{VPCServiceImpl: svc, stray: "192.0.2.10"}, nil)
	_, err = stray.WorkerNetwork(t.Context(), sqsTestRegion, "claim-a")
	require.ErrorContains(t, err, "is not in subnet")
}

func describeENI(t *testing.T, svc *ec2vpc.VPCServiceImpl, id string) *ec2.NetworkInterface {
	t.Helper()
	out, err := svc.DescribeNetworkInterfaces(t.Context(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: aws.StringSlice([]string{id})}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	return out.NetworkInterfaces[0]
}

// topologyGateway is the subnet router address computed independently of the
// adapter: the network address plus one.
func topologyGateway(p netip.Prefix) (string, int, error) {
	b := p.Masked().Addr().As4()
	b[3]++
	return netip.AddrFrom4(b).String(), p.Bits(), nil
}
