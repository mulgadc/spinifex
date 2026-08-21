// Exercises unexported host/port resolution internals with no exported
// surface to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testApplianceIdentifier = "ochre-vector-pg"
	testApplianceSubnet     = "subnet-appliance-0001"
	testApplianceEndpoint   = "10.244.1.9"
	testApplianceENIID      = "eni-appliance-0001"
	testApplianceSG         = "sg-appliance-0001"
)

// fakeVPC is an in-memory vpcProvisioner: every CreateNetworkInterface call
// returns a fresh ENI ID/IP, and records are retained so
// DescribeNetworkInterfaces can answer both the tag-filtered appliance-ENI
// lookup and the description-filtered daemon-ENI describe-or-create.
// subnets backs DescribeSubnets.
type fakeVPC struct {
	mu         sync.Mutex
	nextID     int
	created    []string
	sgModifies []string
	records    map[string]*ec2.NetworkInterface
	subnets    []*ec2.Subnet
}

var _ vpcProvisioner = (*fakeVPC)(nil)

func (f *fakeVPC) CreateNetworkInterface(_ context.Context, in *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	eniID := fmt.Sprintf("eni-%d", id)
	f.created = append(f.created, eniID)
	ni := &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		PrivateIpAddress:   aws.String(fmt.Sprintf("10.244.1.%d", id%250+10)),
		MacAddress:         aws.String("02:00:00:00:00:01"),
		SubnetId:           in.SubnetId,
		Description:        in.Description,
		Groups:             groupIdentifiers(in.Groups),
	}
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = ni
	return &ec2.CreateNetworkInterfaceOutput{NetworkInterface: ni}, nil
}

// ModifyNetworkInterfaceAttribute records the call and re-associates the
// stored ENI's security groups, mirroring the in-place SG replace the real
// service does.
func (f *fakeVPC) ModifyNetworkInterfaceAttribute(_ context.Context, in *ec2.ModifyNetworkInterfaceAttributeInput, _ string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := aws.StringValue(in.NetworkInterfaceId)
	f.sgModifies = append(f.sgModifies, id)
	if ni, ok := f.records[id]; ok && in.Groups != nil {
		ni.Groups = groupIdentifiers(in.Groups)
	}
	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

// groupIdentifiers maps SG IDs to the describe-shaped GroupIdentifier slice.
func groupIdentifiers(ids []*string) []*ec2.GroupIdentifier {
	if len(ids) == 0 {
		return nil
	}
	out := make([]*ec2.GroupIdentifier, 0, len(ids))
	for _, id := range ids {
		out = append(out, &ec2.GroupIdentifier{GroupId: id})
	}
	return out
}

// putRecord seeds an ENI the fake did not mint, so a test can present the
// lookup with a record it would not otherwise produce.
func (f *fakeVPC) putRecord(eniID, subnetID, description, ip, mac string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		SubnetId:           aws.String(subnetID),
		Description:        aws.String(description),
		PrivateIpAddress:   aws.String(ip),
		MacAddress:         aws.String(mac),
	}
}

// putApplianceENI seeds the appliance's own customer ENI, tagged the way
// handlers/rds tags it at launch, so resolveApplianceTarget's tag-filtered
// lookup can find it.
func (f *fakeVPC) putApplianceENI(eniID, identifier, subnetID, ip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		SubnetId:           aws.String(subnetID),
		PrivateIpAddress:   aws.String(ip),
		Groups:             []*ec2.GroupIdentifier{{GroupId: aws.String(testApplianceSG)}},
		TagSet: []*ec2.Tag{
			{Key: aws.String(applianceInstanceTagKey), Value: aws.String(identifier)},
		},
	}
}

// putTaggedENIWithDescription seeds an appliance-tagged ENI that also carries
// a description, so a test can present both the endpoint ENI and the
// management NIC that share the rds-db-instance tag at launch.
func (f *fakeVPC) putTaggedENIWithDescription(eniID, identifier, subnetID, ip, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.records == nil {
		f.records = map[string]*ec2.NetworkInterface{}
	}
	f.records[eniID] = &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(eniID),
		SubnetId:           aws.String(subnetID),
		PrivateIpAddress:   aws.String(ip),
		Description:        aws.String(description),
		Groups:             []*ec2.GroupIdentifier{{GroupId: aws.String(testApplianceSG)}},
		TagSet: []*ec2.Tag{
			{Key: aws.String(applianceInstanceTagKey), Value: aws.String(identifier)},
		},
	}
}

// tagValue returns the value of key in tags, or "" if absent.
func tagValue(tags []*ec2.Tag, key string) string {
	for _, t := range tags {
		if aws.StringValue(t.Key) == key {
			return aws.StringValue(t.Value)
		}
	}
	return ""
}

func (f *fakeVPC) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*ec2.NetworkInterface
	for _, ni := range f.records {
		match := true
		for _, filter := range in.Filters {
			name := aws.StringValue(filter.Name)
			var got string
			switch {
			case name == "subnet-id":
				got = aws.StringValue(ni.SubnetId)
			case name == "description":
				got = aws.StringValue(ni.Description)
			case strings.HasPrefix(name, "tag:"):
				got = tagValue(ni.TagSet, strings.TrimPrefix(name, "tag:"))
			default:
				return nil, fmt.Errorf("fakeVPC: unsupported ENI filter %q", name)
			}
			if !slices.Contains(aws.StringValueSlice(filter.Values), got) {
				match = false
				break
			}
		}
		if match {
			out = append(out, ni)
		}
	}
	return &ec2.DescribeNetworkInterfacesOutput{NetworkInterfaces: out}, nil
}

func (f *fakeVPC) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ string) (*ec2.DescribeSubnetsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(in.Filters) == 0 {
		return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
	}
	var out []*ec2.Subnet
	for _, sn := range f.subnets {
		match := true
		for _, filter := range in.Filters {
			var got string
			switch aws.StringValue(filter.Name) {
			case "subnet-id":
				got = aws.StringValue(sn.SubnetId)
			case "vpc-id":
				got = aws.StringValue(sn.VpcId)
			default:
				return nil, fmt.Errorf("fakeVPC: unsupported subnet filter %q", aws.StringValue(filter.Name))
			}
			if !slices.Contains(aws.StringValueSlice(filter.Values), got) {
				match = false
				break
			}
		}
		if match {
			out = append(out, sn)
		}
	}
	return &ec2.DescribeSubnetsOutput{Subnets: out}, nil
}

// fakeHostPort records the host-side ports ensureApplianceHostPort/
// removeApplianceHostPort asked for.
type fakeHostPort struct {
	mu         sync.Mutex
	ensured    []hostPortCall
	removed    []string
	failEnsure error
	failRemove error
}

var _ hostPortPlumber = (*fakeHostPort)(nil)

type hostPortCall struct {
	eniID string
	mac   string
	addr  string
}

func (f *fakeHostPort) EnsureVPCHostPort(eniID, mac, addr string) error {
	f.mu.Lock()
	f.ensured = append(f.ensured, hostPortCall{eniID: eniID, mac: mac, addr: addr})
	f.mu.Unlock()
	return f.failEnsure
}

func (f *fakeHostPort) RemoveVPCHostPort(eniID string) error {
	f.mu.Lock()
	f.removed = append(f.removed, eniID)
	f.mu.Unlock()
	return f.failRemove
}

func (f *fakeHostPort) calls() []hostPortCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ensured)
}

func (f *fakeHostPort) removals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// hostPortHarness bundles every HostPortDeps fake so a test can reach into
// any of them.
type hostPortHarness struct {
	vpc      *fakeVPC
	hostPort *fakeHostPort
}

func newHostPortHarness() *hostPortHarness {
	return &hostPortHarness{
		vpc:      &fakeVPC{},
		hostPort: &fakeHostPort{},
	}
}

func (h *hostPortHarness) deps() HostPortDeps {
	return HostPortDeps{
		VPC:      h.vpc,
		HostPort: h.hostPort,
		NodeID:   testNodeID,
	}
}

// putSubnet seeds a subnet in the harness's VPC covering the given CIDR.
func (h *hostPortHarness) putSubnet(subnetID, cidr string) {
	h.vpc.mu.Lock()
	h.vpc.subnets = append(h.vpc.subnets, &ec2.Subnet{
		SubnetId:  aws.String(subnetID),
		CidrBlock: aws.String(cidr),
	})
	h.vpc.mu.Unlock()
}

// putApplianceENI seeds the appliance's own tagged customer ENI in subnetID,
// so resolveApplianceTarget's tag-filtered describe finds it.
func (h *hostPortHarness) putApplianceENI(identifier, subnetID, ip string) {
	h.vpc.putApplianceENI(testApplianceENIID, identifier, subnetID, ip)
}

const testNodeID = "node-a"

func TestEnsureApplianceHostPort_CreatesENIAndHostPort(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	dialIP, eniID, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)

	assert.Equal(t, testApplianceEndpoint, dialIP, "the dial IP should be the appliance's own ENI address")
	require.Len(t, h.vpc.created, 1, "one daemon ENI should be minted")
	require.NotEmpty(t, eniID)
	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, eniID, calls[0].eniID)
	assert.Equal(t, "02:00:00:00:00:01", calls[0].mac)
}

// The daemon's host-port ENI must join the appliance's security group, or the
// customer ENI's self-referencing SG silently drops its traffic to pg.
func TestEnsureApplianceHostPort_DaemonENIJoinsApplianceSG(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	_, eniID, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)

	h.vpc.mu.Lock()
	defer h.vpc.mu.Unlock()
	require.Contains(t, h.vpc.records, eniID)
	got := []string{}
	for _, g := range h.vpc.records[eniID].Groups {
		got = append(got, aws.StringValue(g.GroupId))
	}
	assert.Equal(t, []string{testApplianceSG}, got, "the created daemon ENI must carry the appliance SG")
}

// A daemon ENI that predates SG inheritance (reused by description, no groups)
// must be re-associated in place rather than left unauthorized.
func TestEnsureApplianceHostPort_ReusedENIGetsApplianceSG(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	h.vpc.putRecord("eni-legacy-daemon", testApplianceSubnet, daemonPortDescription(testNodeID), "10.244.1.50", "02:00:00:00:00:09")

	_, eniID, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)
	assert.Equal(t, "eni-legacy-daemon", eniID, "the existing daemon ENI should be reused")
	assert.Empty(t, h.vpc.created, "reuse must not mint a new ENI")

	h.vpc.mu.Lock()
	defer h.vpc.mu.Unlock()
	assert.Contains(t, h.vpc.sgModifies, "eni-legacy-daemon", "the reused ENI must be re-associated to the appliance SG")
	got := []string{}
	for _, g := range h.vpc.records["eni-legacy-daemon"].Groups {
		got = append(got, aws.StringValue(g.GroupId))
	}
	assert.Equal(t, []string{testApplianceSG}, got)
}

// The port must carry the ENI address at the SUBNET's prefix length: a /32
// addresses the port and still leaves the appliance unreachable, which is
// the entire reason the port exists.
func TestEnsureApplianceHostPort_UsesSubnetPrefixLength(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	_, _, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)

	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.True(t, strings.HasSuffix(calls[0].addr, "/24"),
		"host port address %q should carry the subnet's /24, not a host route", calls[0].addr)
	assert.False(t, strings.HasPrefix(calls[0].addr, "10.244.1.0/"),
		"host port address %q must be the ENI's own address, not the network address", calls[0].addr)
}

// One ENI per node, not one per connect: without describe-or-create every
// Connect would leak an address out of the appliance's subnet.
func TestEnsureApplianceHostPort_ReusesTheNodesExistingENI(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	deps := h.deps()

	firstIP, firstENI, err := ensureApplianceHostPort(t.Context(), deps, testApplianceIdentifier)
	require.NoError(t, err)
	secondIP, secondENI, err := ensureApplianceHostPort(t.Context(), deps, testApplianceIdentifier)
	require.NoError(t, err)

	assert.Equal(t, firstIP, secondIP, "the dial IP should resolve the same both times")
	assert.Equal(t, firstENI, secondENI, "the second ensure should reuse the first ENI")
	assert.Len(t, h.vpc.created, 1, "the second ensure should reuse the first ENI")
	calls := h.hostPort.calls()
	require.Len(t, calls, 2, "the host port is still re-driven, since a reboot clears it")
	assert.Equal(t, calls[0], calls[1], "both ensures should install the same port")
}

// Two daemons sharing one address would fight over the same OVN logical
// port, so the node identity has to reach the ENI lookup.
func TestEnsureApplianceHostPort_DistinctENIPerNode(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	a := h.deps()
	a.NodeID = "node-a"
	b := h.deps()
	b.NodeID = "node-b"

	_, eniA, err := ensureApplianceHostPort(t.Context(), a, testApplianceIdentifier)
	require.NoError(t, err)
	_, eniB, err := ensureApplianceHostPort(t.Context(), b, testApplianceIdentifier)
	require.NoError(t, err)

	assert.NotEqual(t, eniA, eniB)
	assert.Len(t, h.vpc.created, 2, "each node should own its own ENI")
}

func TestEnsureApplianceHostPort_RequiresPlumberNodeIDAndVPC(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	noPlumber := h.deps()
	noPlumber.HostPort = nil
	_, _, err := ensureApplianceHostPort(t.Context(), noPlumber, testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host-port plumber")

	noNode := h.deps()
	noNode.NodeID = ""
	_, _, err = ensureApplianceHostPort(t.Context(), noNode, testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node id")

	noVPC := h.deps()
	noVPC.VPC = nil
	_, _, err = ensureApplianceHostPort(t.Context(), noVPC, testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VPC provider")

	assert.Empty(t, h.vpc.created, "no refusal should have minted an ENI")
}

func TestEnsureApplianceHostPort_PropagatesPlumberFailure(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	h.hostPort.failEnsure = errors.New("ovs-vsctl exploded")

	_, _, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ovs-vsctl exploded")
}

// An ENI record with no address or MAC cannot back a host port, so the
// lookup must fall through and mint a usable one rather than install a
// broken port.
func TestEnsureApplianceHostPort_SkipsIncompleteENIRecords(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)
	h.vpc.putRecord("eni-broken", testApplianceSubnet, daemonPortDescription(testNodeID), "", "")

	_, eniID, err := ensureApplianceHostPort(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)
	assert.NotEqual(t, "eni-broken", eniID)
	calls := h.hostPort.calls()
	require.Len(t, calls, 1)
	assert.NotEmpty(t, calls[0].mac)
}

func TestDaemonPortDescriptionCarriesNodeID(t *testing.T) {
	assert.Contains(t, daemonPortDescription("node-a"), "node-a")
	assert.NotEqual(t, daemonPortDescription("node-a"), daemonPortDescription("node-b"))
}

// --- tag-filtered appliance ENI resolution ---

func TestResolveApplianceTarget_FindsTheTaggedApplianceENI(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	dialIP, subnetID, cidr, groupIDs, err := resolveApplianceTarget(t.Context(), h.deps(), testApplianceIdentifier)
	require.NoError(t, err)
	assert.Equal(t, testApplianceEndpoint, dialIP)
	assert.Equal(t, testApplianceSubnet, subnetID)
	assert.Equal(t, "10.244.1.0/24", cidr)
	assert.Equal(t, []string{testApplianceSG}, groupIDs, "the appliance SG must be surfaced so the daemon ENI can join it")
}

// The endpoint ENI and the management NIC share the rds-db-instance tag, but
// only the endpoint ENI (described "RDS endpoint ENI for ...") is reachable
// from the daemon. The resolver must pick it by description regardless of the
// order DescribeNetworkInterfaces returns them -- map iteration is unordered,
// so a repeat loop exercises both orderings.
func TestResolveApplianceTarget_PrefersEndpointENIOverManagementNIC(t *testing.T) {
	const mgmtSubnet = "subnet-mgmt-0001"
	const mgmtIP = "10.251.185.4"
	for range 16 {
		h := newHostPortHarness()
		h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
		h.putSubnet(mgmtSubnet, "10.251.185.0/24")
		h.vpc.putTaggedENIWithDescription("eni-mgmt", testApplianceIdentifier, mgmtSubnet, mgmtIP,
			"RDS management NIC for "+testApplianceIdentifier)
		h.vpc.putTaggedENIWithDescription("eni-endpoint", testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint,
			endpointENIDescriptionPrefix+testApplianceIdentifier)

		dialIP, subnetID, cidr, _, err := resolveApplianceTarget(t.Context(), h.deps(), testApplianceIdentifier)
		require.NoError(t, err)
		require.Equal(t, testApplianceEndpoint, dialIP, "must dial the endpoint ENI, never the management NIC")
		require.Equal(t, testApplianceSubnet, subnetID)
		require.Equal(t, "10.244.1.0/24", cidr)
	}
}

// An ENI tagged for a different identifier must never match.
func TestResolveApplianceTarget_IgnoresOtherIdentifiersTags(t *testing.T) {
	h := newHostPortHarness()
	h.putSubnet(testApplianceSubnet, "10.244.1.0/24")
	h.putApplianceENI("some-other-db", testApplianceSubnet, testApplianceEndpoint)

	_, _, _, _, err := resolveApplianceTarget(t.Context(), h.deps(), testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no customer ENI found")
}

func TestResolveApplianceTarget_ErrorsWhenSubnetHasNoCIDR(t *testing.T) {
	h := newHostPortHarness()
	h.putApplianceENI(testApplianceIdentifier, testApplianceSubnet, testApplianceEndpoint)

	_, _, _, _, err := resolveApplianceTarget(t.Context(), h.deps(), testApplianceIdentifier)
	require.Error(t, err)
}

func TestResolveApplianceTarget_PropagatesDescribeFailure(t *testing.T) {
	h := newHostPortHarness()
	failing := &failingVPC{fakeVPC: h.vpc, failDescribeENIs: errors.New("nats timeout")}
	deps := h.deps()
	deps.VPC = failing

	_, _, _, _, err := resolveApplianceTarget(t.Context(), deps, testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nats timeout")
}

func TestResolveApplianceTarget_RequiresVPC(t *testing.T) {
	h := newHostPortHarness()
	noVPC := h.deps()
	noVPC.VPC = nil
	_, _, _, _, err := resolveApplianceTarget(t.Context(), noVPC, testApplianceIdentifier)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VPC provider")
}

// failingVPC wraps a fakeVPC to inject a DescribeNetworkInterfaces failure,
// so resolveApplianceTarget's error propagation can be tested without a real
// EC2 fault.
type failingVPC struct {
	*fakeVPC

	failDescribeENIs error
}

var _ vpcProvisioner = (*failingVPC)(nil)

func (f *failingVPC) DescribeNetworkInterfaces(ctx context.Context, in *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if f.failDescribeENIs != nil {
		return nil, f.failDescribeENIs
	}
	return f.fakeVPC.DescribeNetworkInterfaces(ctx, in, accountID)
}

// --- teardown ---

func TestRemoveApplianceHostPort_NoopWithoutHostPortOrENI(t *testing.T) {
	h := newHostPortHarness()
	require.NoError(t, removeApplianceHostPort(HostPortDeps{}, "eni-1"))
	require.NoError(t, removeApplianceHostPort(h.deps(), ""))
	assert.Empty(t, h.hostPort.removals())
}

func TestRemoveApplianceHostPort_RemovesTheInstalledPort(t *testing.T) {
	h := newHostPortHarness()
	require.NoError(t, removeApplianceHostPort(h.deps(), "eni-42"))
	assert.Equal(t, []string{"eni-42"}, h.hostPort.removals())
}

func TestRemoveApplianceHostPort_PropagatesPlumberFailure(t *testing.T) {
	h := newHostPortHarness()
	h.hostPort.failRemove = errors.New("br-int is gone")
	err := removeApplianceHostPort(h.deps(), "eni-42")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "br-int is gone")
}
