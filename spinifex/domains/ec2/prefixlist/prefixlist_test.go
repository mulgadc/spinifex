package prefixlist

import (
	"context"
	"net/netip"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const region = "ap-southeast-2"

func node(advertise, predastoreHost string) config.Config {
	return config.Config{AdvertiseIP: advertise, Predastore: config.PredastoreConfig{Host: predastoreHost}}
}

func cluster(nodes map[string]config.Config) *config.ClusterConfig {
	return &config.ClusterConfig{Node: "n1", Nodes: nodes}
}

// The ID is AWS's 8-hex form for a managed list, and depends on the region alone.
func TestID_IsStablePerRegion(t *testing.T) {
	assert.Regexp(t, `^pl-[0-9a-f]{8}$`, ID(region))
	assert.NotEqual(t, ID(region), ID("us-east-1"))
	assert.Equal(t, "com.amazonaws.ap-southeast-2.s3", ServiceName(region))
}

// The entries are every node's IPv4 service address with its gate port, and a
// node joining changes them on the next read, with nothing written.
func TestEntries_FollowTheNodeSet(t *testing.T) {
	nodes := map[string]config.Config{
		"n1": node("10.0.0.2", "0.0.0.0:8443"),
		"n2": node("10.0.0.10", "0.0.0.0:9443"),
		"n3": node("fd00::3", ""),      // IPv6: no /32 to list
		"n4": node("not-an-ip", ""),    // a hostname has no address
		"n5": node("10.0.0.2", ""),     // a duplicate address is listed once
		"n6": node("", ""),             // no service host at all
		"n7": node("10.0.0.7", "nope"), // an unparseable host falls back to the default port
	}
	cc := cluster(nodes)
	assert.Equal(t, []Entry{
		{Addr: mustAddr("10.0.0.2"), GatePort: 8443},
		{Addr: mustAddr("10.0.0.7"), GatePort: DefaultGatePort},
		{Addr: mustAddr("10.0.0.10"), GatePort: 9443},
	}, Entries(cc, ""))

	nodes["n8"] = node("10.0.0.8", "")
	assert.Len(t, Entries(cc, ""), 4)
	assert.Nil(t, Entries(nil, ""))
}

// The local node's br-mgmt address is listed wherever DNS publishes it, and
// the list holds exactly the IPv4 addresses DNS publishes.
func TestEntries_MatchTheDNSAddresses(t *testing.T) {
	cc := cluster(map[string]config.Config{
		"n1": node("", "0.0.0.0:8443"),
		"n2": node("10.0.0.10", ""),
	})
	got := Entries(cc, config.DefaultMgmtBridgeIP)
	assert.Equal(t, []Entry{
		{Addr: mustAddr("10.0.0.10"), GatePort: DefaultGatePort},
		{Addr: mustAddr(config.DefaultMgmtBridgeIP), GatePort: 8443},
	}, got)
	var dns []netip.Addr
	for _, ep := range config.ServiceEndpoints(cc, config.DefaultMgmtBridgeIP) {
		dns = append(dns, ep.Addr)
	}
	require.Len(t, got, len(dns))
	for i, e := range got {
		assert.Equal(t, dns[i], e.Addr)
	}
}

func newService(nodes map[string]config.Config) *ServiceImpl {
	cc := cluster(nodes)
	return NewServiceImpl(region, "", func() *config.ClusterConfig { return cc })
}

func TestDescribeManagedPrefixLists_RendersTheAWSManagedShape(t *testing.T) {
	s := newService(map[string]config.Config{"n1": node("10.0.0.2", "")})
	out, err := s.DescribeManagedPrefixLists(context.Background(), &ec2.DescribeManagedPrefixListsInput{}, "000000000001")
	require.NoError(t, err)
	require.Len(t, out.PrefixLists, 1)
	pl := out.PrefixLists[0]
	assert.Equal(t, ID(region), *pl.PrefixListId)
	assert.Equal(t, "arn:aws:ec2:ap-southeast-2:aws:prefix-list/"+ID(region), *pl.PrefixListArn)
	assert.Equal(t, "AWS", *pl.OwnerId)
	assert.Equal(t, "create-complete", *pl.State)
	assert.Equal(t, "IPv4", *pl.AddressFamily)
	assert.Nil(t, pl.Version, "AWS renders no version for a managed list")
	assert.Nil(t, pl.MaxEntries)
	assert.Empty(t, pl.Tags)

	for _, f := range []*ec2.Filter{
		{Name: aws.String("owner-id"), Values: aws.StringSlice([]string{"000000000001"})},
		{Name: aws.String("prefix-list-name"), Values: aws.StringSlice([]string{"com.amazonaws.ap-southeast-2.dynamodb"})},
	} {
		out, err = s.DescribeManagedPrefixLists(context.Background(), &ec2.DescribeManagedPrefixListsInput{Filters: []*ec2.Filter{f}}, "000000000001")
		require.NoError(t, err)
		assert.Empty(t, out.PrefixLists, *f.Name)
	}
}

func TestDescribePrefixLists(t *testing.T) {
	s := newService(map[string]config.Config{"n1": node("10.0.0.2", ""), "n2": node("10.0.0.3", "")})
	out, err := s.DescribePrefixLists(context.Background(), &ec2.DescribePrefixListsInput{PrefixListIds: aws.StringSlice([]string{ID(region)})}, "")
	require.NoError(t, err)
	require.Len(t, out.PrefixLists, 1)
	assert.Equal(t, []string{"10.0.0.2/32", "10.0.0.3/32"}, aws.StringValueSlice(out.PrefixLists[0].Cidrs))

	_, err = s.DescribePrefixLists(context.Background(), &ec2.DescribePrefixListsInput{PrefixListIds: aws.StringSlice([]string{"pl-00000000"})}, "")
	require.ErrorContains(t, err, "InvalidPrefixListID.NotFound")

	_, err = s.DescribePrefixLists(context.Background(), &ec2.DescribePrefixListsInput{
		Filters: []*ec2.Filter{{Name: aws.String("no-such-filter"), Values: aws.StringSlice([]string{"x"})}},
	}, "")
	require.ErrorContains(t, err, "no-such-filter")
}

func TestGetManagedPrefixListEntries(t *testing.T) {
	s := newService(map[string]config.Config{"n1": node("10.0.0.2", ""), "n2": node("10.0.0.3", ""), "n3": node("10.0.0.4", "")})
	ctx := context.Background()

	out, err := s.GetManagedPrefixListEntries(ctx, &ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(ID(region)), MaxResults: aws.Int64(2)}, "")
	require.NoError(t, err)
	require.Len(t, out.Entries, 2)
	assert.Nil(t, out.Entries[0].Description, "entries carry Cidr only")
	require.NotNil(t, out.NextToken)
	rest, err := s.GetManagedPrefixListEntries(ctx, &ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(ID(region)), NextToken: out.NextToken}, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.4/32"}, []string{*rest.Entries[0].Cidr})
	assert.Nil(t, rest.NextToken)

	_, err = s.GetManagedPrefixListEntries(ctx, &ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(ID(region)), TargetVersion: aws.Int64(1)}, "")
	require.ErrorContains(t, err, "Version is not supported for an AWS-managed prefix list.")
	require.ErrorContains(t, err, "UnsupportedOperation")

	_, err = s.GetManagedPrefixListEntries(ctx, &ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String("pl-00000000")}, "")
	require.ErrorContains(t, err, "The prefix list (pl-00000000) does not exist.")
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

// While gossiped holds, the reads drop this node's bridge address and list
// only what every node derives from the node set.
func TestReads_UseTheGossipedDerivationWhenSet(t *testing.T) {
	cc := cluster(map[string]config.Config{
		"n1": node("", "0.0.0.0:8443"),
		"n2": node("10.0.0.10", ""),
	})
	s := NewServiceImpl(region, config.DefaultMgmtBridgeIP, func() *config.ClusterConfig { return cc })
	gossiped := false
	s.SetGossiped(func(context.Context) bool { return gossiped })
	read := func() []string {
		out, err := s.GetManagedPrefixListEntries(context.Background(), &ec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(ID(region))}, "000000000001")
		require.NoError(t, err)
		var cidrs []string
		for _, e := range out.Entries {
			cidrs = append(cidrs, *e.Cidr)
		}
		described, err := s.DescribePrefixLists(context.Background(), &ec2.DescribePrefixListsInput{}, "000000000001")
		require.NoError(t, err)
		require.Len(t, described.PrefixLists, 1)
		assert.ElementsMatch(t, cidrs, aws.StringValueSlice(described.PrefixLists[0].Cidrs))
		return cidrs
	}

	assert.ElementsMatch(t, []string{"10.0.0.10/32", config.DefaultMgmtBridgeIP + "/32"}, read())
	gossiped = true
	assert.Equal(t, []string{"10.0.0.10/32"}, read())
}

// The one-list reads still refuse a MaxResults outside their range and a token
// this service did not issue.
func TestDescribePrefixLists_ValidatePaging(t *testing.T) {
	s := newService(map[string]config.Config{"n1": node("10.0.0.2", "")})
	ctx := context.Background()
	bad := aws.String("5")

	_, err := s.DescribePrefixLists(ctx, &ec2.DescribePrefixListsInput{MaxResults: aws.Int64(4)}, "")
	require.ErrorContains(t, err, "Value ( 4 ) for parameter maxResults is invalid. Expecting a value greater than or equal to 5.")
	_, err = s.DescribePrefixLists(ctx, &ec2.DescribePrefixListsInput{NextToken: bad}, "")
	require.ErrorContains(t, err, "Value ( 5 ) for parameter NextToken is invalid. The token is invalid.")

	_, err = s.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{MaxResults: aws.Int64(101)}, "")
	require.ErrorContains(t, err, "Value ( 101 ) for parameter maxResults is invalid. Expecting a value between 1 and 100.")
	_, err = s.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{NextToken: bad}, "")
	require.ErrorContains(t, err, "Value ( 5 ) for parameter NextToken is invalid. The token is invalid.")
	out, err := s.DescribeManagedPrefixLists(ctx, &ec2.DescribeManagedPrefixListsInput{MaxResults: aws.Int64(1)}, "")
	require.NoError(t, err)
	assert.Len(t, out.PrefixLists, 1)
}
