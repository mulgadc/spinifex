//test:in-package — builds the reconciler with its unexported hooks, so the
// pass reads intent from a real JetStream the way vpcd does.

package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// benchJetStream starts an embedded JetStream server for a benchmark.
func benchJetStream(b *testing.B) jetstream.JetStream {
	b.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: b.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		b.Fatalf("NewServer: %v", err)
	}
	ns.Start()
	b.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		b.Fatal("embedded NATS server not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		b.Fatalf("Connect: %v", err)
	}
	b.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		b.Fatalf("jetstream.New: %v", err)
	}
	return js
}

func benchPut(b *testing.B, kv jetstream.KeyValue, key string, rec any) {
	b.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		b.Fatalf("marshal %s: %v", key, err)
	}
	if _, err := kv.Put(context.Background(), key, raw); err != nil {
		b.Fatalf("put %s: %v", key, err)
	}
}

// seedDriftFixture writes vpcs VPCs, each with one subnet and one SG, and enis
// ENIs spread across them.
func seedDriftFixture(b *testing.B, js jetstream.JetStream, vpcs, enis int) {
	b.Helper()
	ctx := context.Background()
	bucket := func(name string) jetstream.KeyValue {
		kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: name, History: 1})
		if err != nil {
			b.Fatalf("bucket %s: %v", name, err)
		}
		return kv
	}
	vpcKV, subnetKV := bucket(ec2vpc.KVBucketVPCs), bucket(ec2vpc.KVBucketSubnets)
	sgKV, eniKV := bucket(ec2vpc.KVBucketSecurityGroups), bucket(ec2vpc.KVBucketENIs)
	// The EC2 services create every topology bucket at start; a missing one
	// reads as lost state, and the pass would change nothing.
	for _, name := range []string{ec2igw.KVBucketIGW, ec2eip.KVBucketEIPs, ec2routetable.KVBucketRouteTables, ec2natgw.KVBucketNatGateways} {
		bucket(name)
	}
	for v := range vpcs {
		vpc, subnet, sg := fmt.Sprintf("vpc-%04d", v), fmt.Sprintf("subnet-%04d", v), fmt.Sprintf("sg-%04d", v)
		benchPut(b, vpcKV, "acct."+vpc, ec2vpc.VPCRecord{VpcId: vpc, CidrBlock: fmt.Sprintf("10.%d.0.0/16", v), VNI: int64(100 + v)})
		benchPut(b, subnetKV, "acct."+subnet, ec2vpc.SubnetRecord{SubnetId: subnet, VpcId: vpc, CidrBlock: fmt.Sprintf("10.%d.0.0/20", v)})
		benchPut(b, sgKV, "acct."+sg, ec2vpc.SecurityGroupRecord{GroupId: sg, VpcId: vpc})
	}
	for e := range enis {
		v := e % vpcs
		id := fmt.Sprintf("eni-%06d", e)
		host := e / vpcs
		benchPut(b, eniKV, "acct."+id, ec2vpc.ENIRecord{
			NetworkInterfaceId: id, SubnetId: fmt.Sprintf("subnet-%04d", v), VpcId: fmt.Sprintf("vpc-%04d", v),
			PrivateIpAddress: fmt.Sprintf("10.%d.%d.%d", v, 1+host/250, 1+host%250),
			MacAddress:       fmt.Sprintf("02:00:00:%02x:%02x:%02x", v, (e>>8)&0xff, e&0xff),
			SecurityGroupIds: []string{fmt.Sprintf("sg-%04d", v)}, Status: "in-use",
		})
	}
}

// BenchmarkDriftPass measures one converged drift pass: intent loaded from KV,
// then a reconcile that finds nothing to change and runs every sweep.
func BenchmarkDriftPass(b *testing.B) {
	for _, enis := range []int{1000} {
		b.Run(fmt.Sprintf("enis=%d", enis), func(b *testing.B) {
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.DiscardHandler))
			b.Cleanup(func() { slog.SetDefault(prev) })

			js := benchJetStream(b)
			seedDriftFixture(b, js, 10, enis)
			load := func(ctx context.Context) (IntentState, error) { return LoadIntentFromKV(ctx, js, "us-east-1a") }
			m := mock.New()
			nat, err := policy.NewNATManager(m, policy.NATModeDistributed)
			if err != nil {
				b.Fatalf("NewNATManager: %v", err)
			}
			routes := policy.NewRouteManager(m)
			igw, err := external.NewIGWManager(external.IGWManagerConfig{
				OVN: m, Routes: routes, NAT: nat, Allocator: external.LinkLocalAllocator{}, NATMode: policy.NATModeDistributed,
			})
			if err != nil {
				b.Fatalf("NewIGWManager: %v", err)
			}
			rec, err := New(Config{
				OVN: m, SG: policy.NewSecurityGroupManager(m, policy.EgressPolicy{}), NAT: nat, Routes: routes, IGW: igw,
				Topology: topology.NewLiveManager(m), LocalAZ: "us-east-1a", NodeHostname: "bench-host",
				FreshIntent: load,
				LocalPorts:  func(context.Context) (map[string]struct{}, error) { return map[string]struct{}{}, nil },
			})
			if err != nil {
				b.Fatalf("New: %v", err)
			}
			ctx := context.Background()
			intent, err := load(ctx)
			if err != nil {
				b.Fatalf("load: %v", err)
			}
			// A partial pass changes nothing, so timing it would measure nothing.
			if err := rec.Reconcile(ctx, intent); err != nil {
				b.Fatalf("converging Reconcile (ErrPassIncomplete=%t): %v", errors.Is(err, ErrPassIncomplete), err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				intent, err := load(ctx)
				if err != nil {
					b.Fatalf("load: %v", err)
				}
				if err := rec.Reconcile(ctx, intent); err != nil {
					b.Fatalf("Reconcile: %v", err)
				}
			}
		})
	}
}
