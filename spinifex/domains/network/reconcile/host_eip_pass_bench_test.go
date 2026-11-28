//test:in-package — builds the reconciler with its unexported hooks, so the
// host pass reads intent from a real JetStream the way vpcd does.

package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// BenchmarkHostEIPPass measures one per-node host EIP pass over 1,000 ENIs
// with public addresses, on complete intent and on partial intent.
func BenchmarkHostEIPPass(b *testing.B) {
	for _, partial := range []bool{false, true} {
		b.Run(fmt.Sprintf("partial=%t", partial), func(b *testing.B) {
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.DiscardHandler))
			b.Cleanup(func() { slog.SetDefault(prev) })

			js := benchJetStream(b)
			seedDriftFixture(b, js, 10, 1000)
			ctx := context.Background()
			eniKV, err := js.KeyValue(ctx, ec2vpc.KVBucketENIs)
			if err != nil {
				b.Fatalf("ENI bucket: %v", err)
			}
			for e := range 1000 {
				id := fmt.Sprintf("eni-%06d", e)
				v, host := e%10, e/10
				benchPut(b, eniKV, "acct."+id, ec2vpc.ENIRecord{
					NetworkInterfaceId: id, SubnetId: fmt.Sprintf("subnet-%04d", v), VpcId: fmt.Sprintf("vpc-%04d", v),
					PrivateIpAddress: fmt.Sprintf("10.%d.%d.%d", v, 1+host/250, 1+host%250),
					MacAddress:       fmt.Sprintf("02:00:00:%02x:%02x:%02x", v, (e>>8)&0xff, e&0xff),
					SecurityGroupIds: []string{fmt.Sprintf("sg-%04d", v)}, Status: "in-use",
					PublicIpAddress: fmt.Sprintf("203.0.%d.%d", 1+e/250, 1+e%250),
				})
			}
			if partial {
				sgKV, err := js.KeyValue(ctx, ec2vpc.KVBucketSecurityGroups)
				if err != nil {
					b.Fatalf("SG bucket: %v", err)
				}
				if _, err := sgKV.Put(ctx, "acct.sg-0000", poison); err != nil {
					b.Fatalf("poison SG: %v", err)
				}
			}

			m := mock.New()
			binder := policy.HostEIPBinder{
				Bind:   func(policy.EIPSpec, string) error { return nil },
				Unbind: func(string) error { return nil },
				List:   func() ([]string, error) { return nil, nil },
			}
			nat, err := policy.NewNATManager(m, policy.NATModeRouted, policy.WithHostEIPBinder(binder))
			if err != nil {
				b.Fatalf("NewNATManager: %v", err)
			}
			routes := policy.NewRouteManager(m)
			igw, err := external.NewIGWManager(external.IGWManagerConfig{
				OVN: m, Routes: routes, NAT: nat, Allocator: external.LinkLocalAllocator{}, NATMode: policy.NATModeRouted,
			})
			if err != nil {
				b.Fatalf("NewIGWManager: %v", err)
			}
			rec, err := New(Config{
				OVN: m, SG: policy.NewSecurityGroupManager(m, policy.EgressPolicy{}), NAT: nat, Routes: routes, IGW: igw,
				Topology: topology.NewLiveManager(m), LocalAZ: "us-east-1a", NodeHostname: "bench-host",
				FreshIntent: func(ctx context.Context) (IntentState, error) { return LoadIntentFromKV(ctx, js, "us-east-1a") },
			})
			if err != nil {
				b.Fatalf("New: %v", err)
			}

			// The complete case must really be complete, or it would time a pass
			// that changes nothing.
			if err := rec.ReconcileHostEIPs(ctx); (err != nil) != partial {
				b.Fatalf("first ReconcileHostEIPs = %v with partial=%t", err, partial)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				err := rec.ReconcileHostEIPs(ctx)
				if (err != nil) != partial {
					b.Fatalf("ReconcileHostEIPs = %v with partial=%t", err, partial)
				}
			}
		})
	}
}
