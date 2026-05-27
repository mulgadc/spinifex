package reconcile

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
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

func TestLoadIntentFromKV_NoBucketsTolerated(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV on empty cluster: %v", err)
	}
	if len(intent.VPCs)+len(intent.Subnets)+len(intent.Ports)+len(intent.SGs)+len(intent.IGWs)+len(intent.EIPs)+len(intent.NATGWs) != 0 {
		t.Errorf("expected empty intent on fresh cluster, got %#v", intent)
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
