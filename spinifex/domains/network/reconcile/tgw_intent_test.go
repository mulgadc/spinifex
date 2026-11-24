package reconcile

import (
	"context"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2projection "github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

var tgwEpoch = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

func linkedAttachment(id, vpcID string, slot, minute int) ec2projection.TransitGatewayAttachment {
	base := netip.MustParseAddr("169.254.64.0").As4()
	base[3] = byte(slot * 4)
	vpcAddr := netip.AddrFrom4(base).Next()
	return ec2projection.TransitGatewayAttachment{
		ID: id, AccountID: testAcct, TransitGatewayID: testTGW, VpcID: vpcID, SubnetIDs: []string{"subnet-" + vpcID},
		Generation: 1, CreatedAt: tgwEpoch.Add(time.Duration(minute) * time.Minute),
		VPCAddr: vpcAddr, TGWAddr: vpcAddr.Next(), Available: true,
	}
}

func classify(vpcs map[string]topology.VPCSpec, atts []ec2projection.TransitGatewayAttachment, gws ...ec2projection.TransitGateway) TransitGatewayIntent {
	if gws == nil {
		gws = []ec2projection.TransitGateway{{ID: testTGW, AccountID: testAcct, Available: true}}
	}
	vpcIDs := map[string]struct{}{}
	azs := map[string]string{}
	for id := range vpcs {
		vpcIDs[id] = struct{}{}
		azs["subnet-"+id] = "us-east-1a"
	}
	out := newTransitGatewayIntent(azs)
	classifyTransitGateways(&out, gws, atts, vpcs, vpcIDs)
	return out
}

func holders(ti TransitGatewayIntent) []string {
	var out []string
	for id, link := range ti.TGWLinks {
		if link.Holds {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func vpcSpecs(cidrs map[string]string) map[string]topology.VPCSpec {
	out := map[string]topology.VPCSpec{}
	for id, c := range cidrs {
		out[id] = topology.VPCSpec{VPCID: id, CIDR: netip.MustParsePrefix(c)}
	}
	return out
}

// Of two attachments with identical ranges the earlier holds the route, in
// either read order; a range that only overlaps is propagated as well.
func TestClassifyTransitGateways_OverlappingRanges(t *testing.T) {
	vpcs := vpcSpecs(map[string]string{"vpc-a": "10.0.0.0/16", "vpc-b": "10.0.0.0/16", "vpc-c": "10.0.1.0/24"})
	atts := []ec2projection.TransitGatewayAttachment{
		linkedAttachment("tgw-attach-b", "vpc-b", 2, 1),
		linkedAttachment("tgw-attach-a", "vpc-a", 1, 2),
		linkedAttachment("tgw-attach-c", "vpc-c", 3, 3),
	}
	want := []string{"tgw-attach-b", "tgw-attach-c"}
	if got := holders(classify(vpcs, atts)); !slices.Equal(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
	slices.Reverse(atts)
	if got := holders(classify(vpcs, atts)); !slices.Equal(got, want) {
		t.Fatalf("holders in reverse order = %v, want %v", got, want)
	}
}

// When the holder is deleted, the next earliest live attachment takes the
// range; a later reattachment of the first VPC does not take it back.
func TestClassifyTransitGateways_HolderDeleted(t *testing.T) {
	vpcs := vpcSpecs(map[string]string{"vpc-a": "10.0.0.0/16", "vpc-b": "10.0.0.0/16"})
	first := linkedAttachment("tgw-attach-a", "vpc-a", 1, 1)
	first.Available, first.CleanupPending = false, true
	second := linkedAttachment("tgw-attach-b", "vpc-b", 2, 2)
	ti := classify(vpcs, []ec2projection.TransitGatewayAttachment{first, second})
	if got := holders(ti); !slices.Equal(got, []string{"tgw-attach-b"}) {
		t.Fatalf("holders = %v, want the remaining attachment", got)
	}
	if _, ok := ti.TGWLinkTeardowns["tgw-attach-a"]; !ok {
		t.Error("the deleted holder is not torn down")
	}

	again := linkedAttachment("tgw-attach-a2", "vpc-a", 3, 3)
	ti = classify(vpcs, []ec2projection.TransitGatewayAttachment{again, second})
	if got := holders(ti); !slices.Equal(got, []string{"tgw-attach-b"}) {
		t.Fatalf("holders after reattachment = %v, want the older attachment", got)
	}
}

// Each record lands in the one stage that acts on it.
func TestClassifyTransitGateways_States(t *testing.T) {
	vpcs := vpcSpecs(map[string]string{"vpc-a": "10.0.0.0/16"})
	unlinked := linkedAttachment("tgw-attach-new", "vpc-a", 0, 0)
	unlinked.VPCAddr, unlinked.TGWAddr, unlinked.Available, unlinked.Pending = netip.Addr{}, netip.Addr{}, false, true
	orphanVPC := linkedAttachment("tgw-attach-gone", "vpc-gone", 2, 0)
	remote := linkedAttachment("tgw-attach-remote", "vpc-remote", 3, 0)
	gws := []ec2projection.TransitGateway{
		{ID: testTGW, AccountID: testAcct, Pending: true, Generation: 5},
		{ID: "tgw-old", AccountID: testAcct, CleanupPending: true},
	}
	vpcIDs := map[string]struct{}{"vpc-a": {}, "vpc-remote": {}}
	out := newTransitGatewayIntent(map[string]string{})
	classifyTransitGateways(&out, gws, []ec2projection.TransitGatewayAttachment{unlinked, orphanVPC, remote}, vpcs, vpcIDs)

	if r := out.TGWRouters[testTGW]; !r.Pending || r.Generation != 5 {
		t.Errorf("router intent = %+v, want pending at generation 5", r)
	}
	if _, ok := out.TGWTeardowns["tgw-old"]; !ok {
		t.Error("deleting Transit Gateway is not torn down")
	}
	if _, ok := out.TGWPrepare["tgw-attach-new"]; !ok {
		t.Error("unlinked pending attachment is not prepared")
	}
	if _, ok := out.TGWLinkTeardowns["tgw-attach-gone"]; !ok {
		t.Error("attachment of a VPC with no record is not torn down")
	}
	if len(out.TGWLinks) != 0 {
		t.Errorf("links = %v, want none: only a local linked attachment is realised", out.TGWLinks)
	}
	if _, ok := out.TGWLive["tgw-attach-remote"]; !ok {
		t.Error("a remote attachment is not live, so the prune would take its rows")
	}
}

// Routes fan out over the governed subnets with the VPC range and more
// specific gateway routes excluded; other targets are ignored.
func TestLoadTGWRoutes_FanOutAndExclusions(t *testing.T) {
	vpcs := vpcSpecs(map[string]string{"vpc-a": "10.0.0.0/16"})
	subnets := map[string]topology.SubnetSpec{
		"subnet-1": {SubnetID: "subnet-1", VPCID: "vpc-a"},
		"subnet-2": {SubnetID: "subnet-2", VPCID: "vpc-a"},
	}
	tables := []ec2routetable.RouteTableRecord{{
		RouteTableId: "rtb-main", VpcId: "vpc-a", IsMain: true,
		Routes: []ec2routetable.RouteRecord{
			{DestinationCidrBlock: "10.0.0.0/8", TransitGatewayId: testTGW},
			{DestinationCidrBlock: "10.9.0.0/16", GatewayId: "igw-1"},
			{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1"},
		},
	}}
	got := loadTGWRoutes(vpcs, subnets, tables)
	if len(got) != 2 || got[0].SubnetID != "subnet-1" || got[1].SubnetID != "subnet-2" {
		t.Fatalf("routes = %+v, want one per main-table subnet", got)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.9.0.0/16")}
	if !slices.Equal(got[0].Exclude, want) || got[0].TransitGatewayID != testTGW {
		t.Errorf("route = %+v, want excludes %v", got[0], want)
	}
}

// Records read from KV become a link with its subnet zones and the route that
// targets it; another AZ's VPC is not realised here.
func TestLoadIntentFromKV_TransitGateway(t *testing.T) {
	js := startKV(t)
	testutil.SeedKV(t, js, ec2vpc.KVBucketVPCs, map[string][]byte{
		testAcct + ".vpc-a": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a"}),
		testAcct + ".vpc-c": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-c", CidrBlock: "10.2.0.0/16", AZ: "us-east-1b"}),
	})
	testutil.SeedKV(t, js, ec2vpc.KVBucketSubnets, map[string][]byte{
		testAcct + ".subnet-a": mustJSON(t, ec2vpc.SubnetRecord{SubnetId: "subnet-a", VpcId: "vpc-a", CidrBlock: "10.0.1.0/24", AvailabilityZone: "us-east-1a"}),
	})
	testutil.SeedKV(t, js, ec2routetable.KVBucketRouteTables, map[string][]byte{
		testAcct + ".rtb-a": mustJSON(t, ec2routetable.RouteTableRecord{
			RouteTableId: "rtb-a", VpcId: "vpc-a", IsMain: true,
			Routes: []ec2routetable.RouteRecord{{DestinationCidrBlock: "10.2.0.0/16", TransitGatewayId: testTGW}},
		}),
	})
	testutil.SeedKV(t, js, ec2projection.TransitGatewayBucket, map[string][]byte{
		testAcct + "." + testTGW: mustJSON(t, ec2tgw.TransitGatewayRecord{ID: testTGW, AccountID: testAcct, State: ec2tgw.StateAvailable}),
	})
	att := func(id, vpcID, subnetID, prefix string) []byte {
		return mustJSON(t, ec2tgw.AttachmentRecord{
			ID: id, AccountID: testAcct, TransitGatewayID: testTGW, VpcID: vpcID, SubnetIDs: []string{subnetID},
			State: ec2tgw.StateAvailable, Generation: 2, LinkPrefix: prefix,
		})
	}
	testutil.SeedKV(t, js, ec2projection.TransitGatewayAttachmentBucket, map[string][]byte{
		testAcct + "." + testAttA: att(testAttA, "vpc-a", "subnet-a", "169.254.64.4/30"),
		testAcct + "." + testAttB: att(testAttB, "vpc-c", "subnet-c", "169.254.64.8/30"),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil || intent.TGWIncomplete != nil {
		t.Fatalf("LoadIntentFromKV = %v, incomplete %v", err, intent.TGWIncomplete)
	}
	if got := slices.Sorted(maps.Keys(intent.TGWLinks)); !slices.Equal(got, []string{testAttA}) {
		t.Fatalf("links = %v, want only the local VPC's", got)
	}
	link := intent.TGWLinks[testAttA]
	if _, ok := link.AZs["us-east-1a"]; !ok || !link.Holds || link.Spec.TGWAddr != netip.MustParseAddr("169.254.64.6") {
		t.Errorf("link = %+v", link)
	}
	if len(intent.TGWRoutes) != 1 || intent.TGWRoutes[0].SubnetID != "subnet-a" {
		t.Errorf("routes = %+v, want one on subnet-a", intent.TGWRoutes)
	}
	if len(intent.TGWLive) != 3 {
		t.Errorf("live = %v, want both attachments and the Transit Gateway", intent.TGWLive)
	}
}
