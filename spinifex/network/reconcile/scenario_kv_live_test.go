package reconcile

// This scenario drives the full front-to-back path: seed the control-plane KV
// (the same JetStream buckets the EC2 VPC handlers write) -> LoadIntentFromKV
// -> reconcile -> real in-process OVN NB. It proves the KV load layer and the
// reconcile wiring agree, not just a hand-built IntentState.

import (
	"context"
	"encoding/json"
	"testing"

	vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"

	"github.com/nats-io/nats.go"
)

// seedKV marshals rec and stores it under key in the named JetStream KV bucket,
// creating the bucket if absent — mirroring how the VPC handlers persist state.
func seedKV(t *testing.T, js nats.JetStreamContext, bucket, key string, rec any) {
	t.Helper()
	kv, err := utils.GetOrCreateKVBucket(js, bucket, 10)
	if err != nil {
		t.Fatalf("GetOrCreateKVBucket %s: %v", bucket, err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %s/%s: %v", bucket, key, err)
	}
	if _, err := kv.Put(key, b); err != nil {
		t.Fatalf("kv.Put %s/%s: %v", bucket, key, err)
	}
}

// TestScenario_LoadIntentFromKV_Live seeds a VPC + subnet + SG + ENI into the
// control-plane KV, loads intent from it, reconciles into a real NB DB, and
// asserts the expected rows plus idempotency.
func TestScenario_LoadIntentFromKV_Live(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	seedKV(t, js, vpc.KVBucketVPCs, "vpc-a", vpc.VPCRecord{
		VpcId: "vpc-a", CidrBlock: "10.0.0.0/16", VNI: 100,
	})
	seedKV(t, js, vpc.KVBucketSubnets, "subnet-a", vpc.SubnetRecord{
		SubnetId: "subnet-a", VpcId: "vpc-a", CidrBlock: "10.0.1.0/24",
	})
	seedKV(t, js, vpc.KVBucketSecurityGroups, "sg-a", vpc.SecurityGroupRecord{
		GroupId: "sg-a", VpcId: "vpc-a",
	})
	seedKV(t, js, vpc.KVBucketENIs, "eni-a", vpc.ENIRecord{
		NetworkInterfaceId: "eni-a", SubnetId: "subnet-a", VpcId: "vpc-a",
		PrivateIpAddress: "10.0.1.10", MacAddress: "02:00:00:00:00:01",
		SecurityGroupIds: []string{"sg-a"},
	})

	ctx := context.Background()
	intent, err := LoadIntentFromKV(ctx, js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}

	// Spot-check the load layer produced the expected specs.
	if v, ok := intent.VPCs["vpc-a"]; !ok || v.VNI != 100 {
		t.Fatalf("intent.VPCs[vpc-a] = %+v, ok=%v", v, ok)
	}
	if _, ok := intent.Subnets["subnet-a"]; !ok {
		t.Fatalf("intent.Subnets missing subnet-a: %+v", intent.Subnets)
	}
	if _, ok := intent.SGs["sg-a"]; !ok {
		t.Fatalf("intent.SGs missing sg-a: %+v", intent.SGs)
	}
	p, ok := intent.Ports["eni-a"]
	if !ok {
		t.Fatalf("intent.Ports missing eni-a: %+v", intent.Ports)
	}
	if p.PrivateIP.String() != "10.0.1.10" || len(p.SGIDs) != 1 || p.SGIDs[0] != "sg-a" {
		t.Fatalf("intent.Ports[eni-a] = %+v, want IP 10.0.1.10 SG sg-a", p)
	}

	rec, cli := newLiveReconciler(t)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}

	if _, err := cli.GetLogicalRouter(ctx, "vpc-vpc-a"); err != nil {
		t.Errorf("VPC router vpc-vpc-a: %v", err)
	}
	if _, err := cli.GetLogicalSwitch(ctx, "subnet-subnet-a"); err != nil {
		t.Errorf("subnet switch subnet-subnet-a: %v", err)
	}
	if _, err := cli.GetLogicalSwitchPort(ctx, "port-eni-a"); err != nil {
		t.Errorf("ENI port port-eni-a: %v", err)
	}

	before := snapshotNB(t, ctx, cli)
	if err := rec.Reconcile(ctx, intent); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	after := snapshotNB(t, ctx, cli)
	if before != after {
		t.Fatalf("reconcile not idempotent; NB changed on second pass:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}
