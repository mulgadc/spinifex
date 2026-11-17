package reconcile

import (
	"context"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

func peeringRec(id, status, req, reqCIDR, acc, accCIDR string) ec2vpcpeering.PeeringRecord {
	return ec2vpcpeering.PeeringRecord{
		ID: id, Status: status, Generation: 2,
		RequesterAccountID: "000000000001", RequesterVpcID: req, RequesterCIDR: reqCIDR,
		AccepterAccountID: "000000000001", AccepterVpcID: acc, AccepterCIDR: accCIDR,
		LinkPrefix: "169.254.192.4/30",
	}
}

// Each connection lands in exactly one of link, teardown or nothing, decided
// by its state and by where its two VPCs live.
func TestLoadIntentFromKV_PeeringClassification(t *testing.T) {
	js := startKV(t)
	testutil.SeedKV(t, js, ec2vpc.KVBucketVPCs, map[string][]byte{
		"000000000001.vpc-a": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a"}),
		"000000000001.vpc-b": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-b", CidrBlock: "10.1.0.0/16", AZ: "us-east-1a"}),
		"000000000001.vpc-c": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-c", CidrBlock: "10.2.0.0/16", AZ: "us-east-1b"}),
	})
	deleted := peeringRec("pcx-3", ec2vpcpeering.StateDeleted, "vpc-a", "10.0.0.0/16", "vpc-b", "10.1.0.0/16")
	deleted.CleanupPending = true
	recs := []ec2vpcpeering.PeeringRecord{
		peeringRec("pcx-1", ec2vpcpeering.StateActive, "vpc-a", "10.0.0.0/16", "vpc-b", "10.1.0.0/16"),
		peeringRec("pcx-2", ec2vpcpeering.StateProvisioning, "vpc-a", "10.0.0.0/16", "vpc-c", "10.2.0.0/16"),
		deleted,
		peeringRec("pcx-4", ec2vpcpeering.StateActive, "vpc-a", "10.0.0.0/16", "vpc-gone", "10.9.0.0/16"),
		peeringRec("pcx-5", ec2vpcpeering.StateActive, "vpc-c", "10.2.0.0/16", "vpc-gone", "10.9.0.0/16"),
		peeringRec("pcx-6", ec2vpcpeering.StatePendingAcceptance, "vpc-a", "10.0.0.0/16", "vpc-b", "10.1.0.0/16"),
		peeringRec("pcx-7", ec2vpcpeering.StateActive, "vpc-gone", "10.9.0.0/16", "vpc-gone2", "10.8.0.0/16"),
	}
	seed := map[string][]byte{}
	for _, r := range recs {
		seed["pcx."+r.ID] = mustJSON(t, r)
	}
	testutil.SeedKV(t, js, ec2vpcpeering.KVBucketPeeringConnections, seed)

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	if intent.PeeringIncomplete != nil {
		t.Fatalf("PeeringIncomplete = %v", intent.PeeringIncomplete)
	}
	if got := slices.Sorted(maps.Keys(intent.PeeringLinks)); !slices.Equal(got, []string{"pcx-1"}) {
		t.Errorf("links = %v, want [pcx-1]", got)
	}
	if got := slices.Sorted(maps.Keys(intent.PeeringTeardowns)); !slices.Equal(got, []string{"pcx-3", "pcx-4", "pcx-7"}) {
		t.Errorf("teardowns = %v, want [pcx-3 pcx-4 pcx-7]", got)
	}
	if len(intent.PeeringLive) != len(recs) {
		t.Errorf("live = %d, want %d", len(intent.PeeringLive), len(recs))
	}
	link := intent.PeeringLinks["pcx-1"]
	want := topology.PeeringLinkSpec{
		PcxID: "pcx-1", RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: netip.MustParsePrefix("10.0.0.0/16"), AccepterCIDR: netip.MustParsePrefix("10.1.0.0/16"),
		RequesterAddr: netip.MustParseAddr("169.254.192.5"), AccepterAddr: netip.MustParseAddr("169.254.192.6"),
	}
	if link.Spec != want || link.Generation != 2 || !link.Active {
		t.Errorf("link = %+v, want spec %+v generation 2 active", link, want)
	}
}

// A VPC whose record will not unmarshal is still present, so its links are
// not torn down for want of a readable record.
func TestLoadIntentFromKV_PeeringUnreadableVPCIsPresent(t *testing.T) {
	js := startKV(t)
	testutil.SeedKV(t, js, ec2vpc.KVBucketVPCs, map[string][]byte{
		"000000000001.vpc-a": mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a"}),
		"000000000001.vpc-b": []byte("{not json"),
	})
	testutil.SeedKV(t, js, ec2vpcpeering.KVBucketPeeringConnections, map[string][]byte{
		"pcx.pcx-1": mustJSON(t, peeringRec("pcx-1", ec2vpcpeering.StateActive, "vpc-a", "10.0.0.0/16", "vpc-b", "10.1.0.0/16")),
	})
	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	if len(intent.PeeringTeardowns) != 0 {
		t.Errorf("teardowns = %v, want none", intent.PeeringTeardowns)
	}
}

// A peering route fans out over every subnet its table governs and excludes
// the VPC range and each more specific gateway route inside its destination.
func TestLoadPeeringRoutes_FanOutAndExclusions(t *testing.T) {
	vpcs := map[string]topology.VPCSpec{"vpc-a": {VPCID: "vpc-a", CIDR: netip.MustParsePrefix("10.0.0.0/16")}}
	subnets := map[string]topology.SubnetSpec{
		"subnet-1": {SubnetID: "subnet-1", VPCID: "vpc-a"},
		"subnet-2": {SubnetID: "subnet-2", VPCID: "vpc-a"},
		"subnet-3": {SubnetID: "subnet-3", VPCID: "vpc-a"},
	}
	tables := []ec2routetable.RouteTableRecord{
		{
			RouteTableId: "rtb-main", VpcId: "vpc-a", IsMain: true,
			Routes: []ec2routetable.RouteRecord{
				{DestinationCidrBlock: "10.0.0.0/8", VpcPeeringConnectionId: "pcx-1"},
				{DestinationCidrBlock: "10.5.0.0/16", GatewayId: "igw-1"},
				{DestinationCidrBlock: "10.6.0.0/16", NatGatewayId: "nat-1"},
				{DestinationCidrBlock: "192.168.0.0/16", GatewayId: "igw-1"},
				{DestinationCidrBlock: "10.0.0.0/7", GatewayId: "igw-1"},
			},
		},
		{
			RouteTableId: "rtb-x", VpcId: "vpc-a",
			Associations: []ec2routetable.AssociationRecord{{AssociationId: "rtbassoc-x", SubnetId: "subnet-3"}},
		},
	}
	got := loadPeeringRoutes(vpcs, subnets, tables)
	slices.SortFunc(got, func(a, b PeeringRouteIntent) int {
		if a.SubnetID < b.SubnetID {
			return -1
		}
		return 1
	})
	if len(got) != 2 || got[0].SubnetID != "subnet-1" || got[1].SubnetID != "subnet-2" {
		t.Fatalf("routes = %+v, want subnet-1 and subnet-2 only", got)
	}
	wantExclude := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.5.0.0/16"), netip.MustParsePrefix("10.6.0.0/16"),
	}
	if !reflect.DeepEqual(got[0].Exclude, wantExclude) {
		t.Errorf("exclude = %v, want %v", got[0].Exclude, wantExclude)
	}
	if got[0].PcxID != "pcx-1" || got[0].Dest != netip.MustParsePrefix("10.0.0.0/8") {
		t.Errorf("route = %+v", got[0])
	}
}
