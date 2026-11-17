package projection

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ec2vpcpeering "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcpeering"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// With no peering bucket there is nothing to realise, and no error.
func TestLoadPeering_NoBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	snap, err := LoadPeering(context.Background(), js)
	if err != nil || len(snap.Connections) != 0 {
		t.Fatalf("LoadPeering = %+v, %v; want empty, nil", snap, err)
	}
}

// A connection maps its state to Linked and Active and its link /30 to the
// two end addresses; VPC presence comes from keys, readable or not.
func TestLoadPeering_MapsRecords(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	testutil.SeedKV(t, js, ec2vpc.KVBucketVPCs, map[string][]byte{
		"000000000001.vpc-a": []byte("{not json"),
	})
	testutil.SeedKV(t, js, PeeringBucket, map[string][]byte{
		"pcx.pcx-1": mustJSON(t, ec2vpcpeering.PeeringRecord{
			ID: "pcx-1", Status: ec2vpcpeering.StateProvisioning, Generation: 4,
			RequesterVpcID: "vpc-a", RequesterCIDR: "10.0.0.0/16",
			AccepterVpcID: "vpc-b", AccepterCIDR: "10.1.0.0/16", LinkPrefix: "169.254.192.8/30",
		}),
		"pcx.pcx-2": mustJSON(t, ec2vpcpeering.PeeringRecord{
			ID: "pcx-2", Status: ec2vpcpeering.StateDeleted, CleanupPending: true,
			RequesterVpcID: "vpc-a", AccepterVpcID: "vpc-b", LinkPrefix: "10.0.0.0/30",
		}),
	})
	snap, err := LoadPeering(context.Background(), js)
	if err != nil {
		t.Fatalf("LoadPeering: %v", err)
	}
	if _, ok := snap.VPCIDs["vpc-a"]; !ok || len(snap.VPCIDs) != 1 {
		t.Errorf("VPCIDs = %v, want [vpc-a]", snap.VPCIDs)
	}
	got := map[string]PeeringConnection{}
	for _, c := range snap.Connections {
		got[c.ID] = c
	}
	want := PeeringConnection{
		ID: "pcx-1", RequesterVPC: "vpc-a", AccepterVPC: "vpc-b",
		RequesterCIDR: netip.MustParsePrefix("10.0.0.0/16"), AccepterCIDR: netip.MustParsePrefix("10.1.0.0/16"),
		RequesterAddr: netip.MustParseAddr("169.254.192.9"), AccepterAddr: netip.MustParseAddr("169.254.192.10"),
		Generation: 4, Linked: true,
	}
	if got["pcx-1"] != want {
		t.Errorf("pcx-1 = %+v, want %+v", got["pcx-1"], want)
	}
	if c := got["pcx-2"]; c.Linked || !c.CleanupPending || c.RequesterAddr.IsValid() {
		t.Errorf("pcx-2 = %+v, want cleanup pending, unlinked, no link address outside the link range", c)
	}
}
