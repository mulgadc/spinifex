package projection

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
)

// With neither bucket there is nothing to realise, and no error.
func TestLoadTransitGateways_NoBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	gws, atts, err := LoadTransitGateways(context.Background(), js)
	if err != nil || len(gws) != 0 || len(atts) != 0 {
		t.Fatalf("LoadTransitGateways = %+v, %+v, %v; want empty, nil", gws, atts, err)
	}
}

// Records map their state to the flags network acts on, a link prefix to the
// two router addresses, and index keys are not records.
func TestLoadTransitGateways_MapsRecords(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	created := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	testutil.SeedKV(t, js, TransitGatewayBucket, map[string][]byte{
		"111.tgw-1":      mustJSON(t, ec2tgw.TransitGatewayRecord{ID: "tgw-1", AccountID: "111", State: ec2tgw.StatePending, Generation: 2}),
		"111.tgw-2":      mustJSON(t, ec2tgw.TransitGatewayRecord{ID: "tgw-2", AccountID: "111", State: ec2tgw.StateDeleting}),
		"acct.111.tgw-1": mustJSON(t, map[string]string{"account_id": "111"}),
	})
	testutil.SeedKV(t, js, TransitGatewayAttachmentBucket, map[string][]byte{
		"111.tgw-attach-1": mustJSON(t, ec2tgw.AttachmentRecord{
			ID: "tgw-attach-1", AccountID: "111", TransitGatewayID: "tgw-1", VpcID: "vpc-a",
			SubnetIDs: []string{"subnet-a"}, State: ec2tgw.StatePending, Generation: 3,
			LinkPrefix: "169.254.64.4/30", CreatedAt: created,
		}),
		"111.tgw-attach-2":       mustJSON(t, ec2tgw.AttachmentRecord{ID: "tgw-attach-2", AccountID: "111", State: ec2tgw.StateFailed}),
		"vpc.vpc-a.tgw-attach-1": mustJSON(t, map[string]string{"account_id": "111"}),
	})
	gws, atts, err := LoadTransitGateways(context.Background(), js)
	if err != nil {
		t.Fatalf("LoadTransitGateways: %v", err)
	}
	byGW := map[string]TransitGateway{}
	for _, gw := range gws {
		byGW[gw.ID] = gw
	}
	if len(byGW) != 2 || !byGW["tgw-1"].Pending || byGW["tgw-1"].Generation != 2 || !byGW["tgw-2"].CleanupPending {
		t.Fatalf("gateways = %+v", gws)
	}
	byAtt := map[string]TransitGatewayAttachment{}
	for _, a := range atts {
		byAtt[a.ID] = a
	}
	a := byAtt["tgw-attach-1"]
	if len(byAtt) != 2 || !a.Pending || a.Generation != 3 || !a.CreatedAt.Equal(created) || a.SubnetIDs[0] != "subnet-a" {
		t.Fatalf("attachments = %+v", atts)
	}
	if a.VPCAddr != netip.MustParseAddr("169.254.64.5") || a.TGWAddr != netip.MustParseAddr("169.254.64.6") {
		t.Errorf("link addresses = %s, %s; want .5 and .6", a.VPCAddr, a.TGWAddr)
	}
	if !byAtt["tgw-attach-2"].CleanupPending || byAtt["tgw-attach-2"].VPCAddr.IsValid() {
		t.Errorf("failed attachment = %+v, want cleanup pending and no link", byAtt["tgw-attach-2"])
	}
}
