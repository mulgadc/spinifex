package reconcile

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
)

func TestMatchesLocalAZ(t *testing.T) {
	cases := []struct {
		vpcAZ, localAZ string
		want           bool
	}{
		{"", "us-east-1a", true}, // legacy record matches every AZ
		{"us-east-1a", "us-east-1a", true},
		{"us-east-1b", "us-east-1a", false},
		{"us-east-1a", "", false}, // non-legacy vpc AZ does not match empty local AZ
	}
	for _, c := range cases {
		if got := matchesLocalAZ(c.vpcAZ, c.localAZ); got != c.want {
			t.Errorf("matchesLocalAZ(%q, %q) = %v, want %v", c.vpcAZ, c.localAZ, got, c.want)
		}
	}
}

func TestLoadIntentFromKV_AZFilter(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	localVPC := handlers_ec2_vpc.VPCRecord{
		VpcId: "vpc-local", CidrBlock: "10.0.0.0/16", State: "available", VNI: 100, AZ: "us-east-1a", CreatedAt: time.Now(),
	}
	foreignVPC := handlers_ec2_vpc.VPCRecord{
		VpcId: "vpc-foreign", CidrBlock: "10.1.0.0/16", State: "available", VNI: 101, AZ: "us-east-1b", CreatedAt: time.Now(),
	}
	legacyVPC := handlers_ec2_vpc.VPCRecord{
		VpcId: "vpc-legacy", CidrBlock: "10.2.0.0/16", State: "available", VNI: 102, CreatedAt: time.Now(),
	}
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/" + localVPC.VpcId:   mustJSON(t, localVPC),
		"acct/" + foreignVPC.VpcId: mustJSON(t, foreignVPC),
		"acct/" + legacyVPC.VpcId:  mustJSON(t, legacyVPC),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if _, ok := intent.VPCs["vpc-local"]; !ok {
		t.Errorf("local VPC missing from intent")
	}
	if _, ok := intent.VPCs["vpc-legacy"]; !ok {
		t.Errorf("legacy (empty-AZ) VPC missing from intent — backwards-compat rule broken")
	}
	if _, ok := intent.VPCs["vpc-foreign"]; ok {
		t.Errorf("foreign-AZ VPC leaked into intent")
	}
}

func TestLoadIntentFromKV_TransitiveSubnetFilter(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a", CreatedAt: time.Now(),
		}),
		"acct/vpc-foreign": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-foreign", CidrBlock: "10.1.0.0/16", AZ: "us-east-1b", CreatedAt: time.Now(),
		}),
	})

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-local": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-local", VpcId: "vpc-local", CidrBlock: "10.0.1.0/24",
		}),
		"acct/subnet-foreign": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-foreign", VpcId: "vpc-foreign", CidrBlock: "10.1.1.0/24",
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if _, ok := intent.Subnets["subnet-local"]; !ok {
		t.Errorf("local subnet missing from intent")
	}
	if _, ok := intent.Subnets["subnet-foreign"]; ok {
		t.Errorf("foreign-AZ subnet leaked into intent — transitive filter broken")
	}
}

func TestLoadIntentFromKV_EIPStateFilter(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_eip.KVBucketEIPs, map[string][]byte{
		"acct/eipassoc-a": mustJSON(t, handlers_ec2_eip.EIPRecord{
			AllocationId: "eipalloc-a", PublicIp: "203.0.113.10", PrivateIp: "10.0.1.5",
			VpcId: "vpc-local", State: "associated",
		}),
		"acct/eipassoc-b": mustJSON(t, handlers_ec2_eip.EIPRecord{
			AllocationId: "eipalloc-b", PublicIp: "203.0.113.11", PrivateIp: "10.0.1.6",
			VpcId: "vpc-local", State: "allocated", // not associated → excluded
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if _, ok := intent.EIPs["10.0.1.5"]; !ok {
		t.Errorf("associated EIP missing from intent")
	}
	if _, ok := intent.EIPs["10.0.1.6"]; ok {
		t.Errorf("non-associated EIP leaked into intent")
	}
}

func TestLoadIntentFromKV_IGWAttachedFilter(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "10.0.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_igw.KVBucketIGW, map[string][]byte{
		"acct/igw-attached": mustJSON(t, handlers_ec2_igw.IGWRecord{
			InternetGatewayId: "igw-attached", VpcId: "vpc-local", State: "available",
		}),
		"acct/igw-detached": mustJSON(t, handlers_ec2_igw.IGWRecord{
			InternetGatewayId: "igw-detached", VpcId: "", State: "available",
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if _, ok := intent.IGWs["vpc-local"]; !ok {
		t.Errorf("attached IGW missing from intent")
	}
	if len(intent.IGWs) != 1 {
		t.Errorf("got %d IGWs, want 1 (detached should be excluded)", len(intent.IGWs))
	}
}

// TestLoadIntentFromKV_NATGWUsesAssociatedSubnet enforces that loadNATGWs emits
// specs for associated *private* subnets, not the NATGW's own public home subnet.
// Wrong CIDR corrupts conntrack reverse-NAT and causes 100% packet loss.
func TestLoadIntentFromKV_NATGWUsesAssociatedSubnet(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "172.31.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-pub":  mustJSON(t, handlers_ec2_vpc.SubnetRecord{SubnetId: "subnet-pub", VpcId: "vpc-local", CidrBlock: "172.31.0.0/20"}),
		"acct/subnet-priv": mustJSON(t, handlers_ec2_vpc.SubnetRecord{SubnetId: "subnet-priv", VpcId: "vpc-local", CidrBlock: "172.31.16.0/20"}),
	})
	testutil.SeedKV(t, js, handlers_ec2_natgw.KVBucketNatGateways, map[string][]byte{
		"acct/nat-1": mustJSON(t, handlers_ec2_natgw.NatGatewayRecord{
			NatGatewayId: "nat-1", VpcId: "vpc-local", SubnetId: "subnet-pub",
			PublicIp: "203.0.113.50", State: "available",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_routetable.KVBucketRouteTables, map[string][]byte{
		"acct/rtb-priv": mustJSON(t, handlers_ec2_routetable.RouteTableRecord{
			RouteTableId: "rtb-priv", VpcId: "vpc-local",
			Routes: []handlers_ec2_routetable.RouteRecord{
				{DestinationCidrBlock: "0.0.0.0/0", NatGatewayId: "nat-1", State: "active"},
			},
			Associations: []handlers_ec2_routetable.AssociationRecord{
				{AssociationId: "rtbassoc-x", SubnetId: "subnet-priv"},
			},
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if len(intent.NATGWs) != 1 {
		t.Fatalf("got %d NATGW specs, want 1; intent=%#v", len(intent.NATGWs), intent.NATGWs)
	}
	for _, spec := range intent.NATGWs {
		if spec.SubnetCIDR != "172.31.16.0/20" {
			t.Errorf("SubnetCIDR=%q, want %q (private subnet routed via NATGW, not NATGW's home subnet)",
				spec.SubnetCIDR, "172.31.16.0/20")
		}
		if spec.NATGatewayID != "nat-1" || spec.PublicIP != "203.0.113.50" {
			t.Errorf("spec mismatch: %#v", spec)
		}
	}
}

// TestLoadIntentFromKV_NATGWNoAssociationSkips guards against SNAT install for
// a NATGW with no route-table association; emitting the home-subnet CIDR would corrupt the SNAT table.
func TestLoadIntentFromKV_NATGWNoAssociationSkips(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "172.31.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-pub": mustJSON(t, handlers_ec2_vpc.SubnetRecord{SubnetId: "subnet-pub", VpcId: "vpc-local", CidrBlock: "172.31.0.0/20"}),
	})
	testutil.SeedKV(t, js, handlers_ec2_natgw.KVBucketNatGateways, map[string][]byte{
		"acct/nat-orphan": mustJSON(t, handlers_ec2_natgw.NatGatewayRecord{
			NatGatewayId: "nat-orphan", VpcId: "vpc-local", SubnetId: "subnet-pub",
			PublicIp: "203.0.113.51", State: "available",
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	if len(intent.NATGWs) != 0 {
		t.Errorf("NATGW with no associated subnets must not produce specs, got %#v", intent.NATGWs)
	}
}

func TestLoadIntentFromKV_NoBucketsTolerated(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV on empty cluster: %v", err)
	}
	if len(intent.VPCs)+len(intent.Subnets)+len(intent.Ports)+len(intent.SGs)+len(intent.IGWs)+len(intent.EIPs)+len(intent.NATGWs)+len(intent.IGWRoutes)+len(intent.NATGWRoutes) != 0 {
		t.Errorf("expected empty intent on fresh cluster, got %#v", intent)
	}
}

// TestLoadIntentFromKV_IGWRoutesFanOutMainRT covers the bootstrap race: events
// dropped before subscribers attach; reconcile must re-derive per-subnet egress
// intent from the main RT, including implicit-main subnets.
func TestLoadIntentFromKV_IGWRoutesFanOutMainRT(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "172.31.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-implicit": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-implicit", VpcId: "vpc-local", CidrBlock: "172.31.0.0/20",
		}),
		"acct/subnet-explicit": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-explicit", VpcId: "vpc-local", CidrBlock: "172.31.16.0/20",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_routetable.KVBucketRouteTables, map[string][]byte{
		"acct/rtb-main": mustJSON(t, handlers_ec2_routetable.RouteTableRecord{
			RouteTableId: "rtb-main", VpcId: "vpc-local", IsMain: true,
			Routes: []handlers_ec2_routetable.RouteRecord{
				{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1", State: "active"},
			},
		}),
		"acct/rtb-explicit": mustJSON(t, handlers_ec2_routetable.RouteTableRecord{
			RouteTableId: "rtb-explicit", VpcId: "vpc-local",
			Associations: []handlers_ec2_routetable.AssociationRecord{
				{AssociationId: "rtbassoc-x", SubnetId: "subnet-explicit"},
			},
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	if len(intent.IGWRoutes) != 1 {
		t.Fatalf("got %d IGWRoutes, want 1; routes=%#v", len(intent.IGWRoutes), intent.IGWRoutes)
	}
	for _, spec := range intent.IGWRoutes {
		if spec.SubnetID != "subnet-implicit" {
			t.Errorf("SubnetID=%q, want %q (explicit subnet is on rtb-explicit which has no IGW route)",
				spec.SubnetID, "subnet-implicit")
		}
		if spec.DestCIDR.String() != "0.0.0.0/0" {
			t.Errorf("DestCIDR=%q, want 0.0.0.0/0", spec.DestCIDR.String())
		}
	}
}

// TestLoadIntentFromKV_DropGatesForUnroutedSubnetWithIGW: a subnet with no
// 0.0.0.0/0 route in an IGW-attached VPC must produce a DropGates intent.
func TestLoadIntentFromKV_DropGatesForUnroutedSubnetWithIGW(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-local": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-local", CidrBlock: "172.31.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-routed": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-routed", VpcId: "vpc-local", CidrBlock: "172.31.0.0/20",
		}),
		"acct/subnet-isolated": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-isolated", VpcId: "vpc-local", CidrBlock: "172.31.16.0/20",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_igw.KVBucketIGW, map[string][]byte{
		"acct/igw-1": mustJSON(t, handlers_ec2_igw.IGWRecord{
			InternetGatewayId: "igw-1", VpcId: "vpc-local", State: "available",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_routetable.KVBucketRouteTables, map[string][]byte{
		"acct/rtb-main": mustJSON(t, handlers_ec2_routetable.RouteTableRecord{
			RouteTableId: "rtb-main", VpcId: "vpc-local", IsMain: true,
			Associations: []handlers_ec2_routetable.AssociationRecord{
				{AssociationId: "rtbassoc-r", SubnetId: "subnet-routed"},
			},
			Routes: []handlers_ec2_routetable.RouteRecord{
				{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1", State: "active"},
			},
		}),
		"acct/rtb-isolated": mustJSON(t, handlers_ec2_routetable.RouteTableRecord{
			RouteTableId: "rtb-isolated", VpcId: "vpc-local",
			Associations: []handlers_ec2_routetable.AssociationRecord{
				{AssociationId: "rtbassoc-i", SubnetId: "subnet-isolated"},
			},
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	if _, ok := intent.IGWRoutes[subnetEgressKey("subnet-routed", netip.MustParsePrefix("0.0.0.0/0"))]; !ok {
		t.Errorf("subnet-routed missing IGW egress intent")
	}
	if _, ok := intent.DropGates[subnetEgressKey("subnet-routed", netip.MustParsePrefix("0.0.0.0/0"))]; ok {
		t.Errorf("subnet-routed must not have a drop gate (it has an IGW route)")
	}
	if _, ok := intent.DropGates[subnetEgressKey("subnet-isolated", netip.MustParsePrefix("0.0.0.0/0"))]; !ok {
		t.Errorf("subnet-isolated missing drop gate intent (no 0.0.0.0/0 in its RT, IGW is attached)")
	}
}

// TestLoadIntentFromKV_NoDropGateWithoutIGW: VPC with no IGW has no router-wide
// default static route; lr_in_ip_routing already drops, so no drop policy needed.
func TestLoadIntentFromKV_NoDropGateWithoutIGW(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketVPCs, map[string][]byte{
		"acct/vpc-air-gapped": mustJSON(t, handlers_ec2_vpc.VPCRecord{
			VpcId: "vpc-air-gapped", CidrBlock: "10.99.0.0/16", AZ: "us-east-1a",
		}),
	})
	testutil.SeedKV(t, js, handlers_ec2_vpc.KVBucketSubnets, map[string][]byte{
		"acct/subnet-local": mustJSON(t, handlers_ec2_vpc.SubnetRecord{
			SubnetId: "subnet-local", VpcId: "vpc-air-gapped", CidrBlock: "10.99.1.0/24",
		}),
	})

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	if len(intent.DropGates) != 0 {
		t.Errorf("VPC has no IGW: expected 0 drop gates, got %d (%v)", len(intent.DropGates), intent.DropGates)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
