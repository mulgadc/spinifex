package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/servicestate"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func enableInput() SQSEnableInput {
	return SQSEnableInput{Region: "ap-southeast-2", NodeID: "node1", WorkerInstanceType: "t3.small",
		StoreInstanceType: "t3.small", VolumeSizeGiB: "8", AccountID: "000000000000"}
}

// Outside a single-node cluster, enable is refused before any state exists.
func TestEnableSQS_RefusesAMultiNodeClusterWritingNothing(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	for _, tc := range []struct {
		nodes    []string
		expected int
	}{{[]string{"node1", "node2", "node3"}, 3}, {[]string{"node1"}, 3}, {[]string{"node1", "node2"}, 1}} {
		if _, err := EnableSQS(ctx, nc, js, tc.nodes, tc.expected, enableInput(), SQSObservers{}, time.Second); err == nil {
			t.Fatalf("enable on %v/%d succeeded", tc.nodes, tc.expected)
		}
	}
	if _, err := js.KeyValue(ctx, servicestate.Bucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
		t.Fatalf("service state bucket = %v; want none created", err)
	}
}

// An enable whose launches fail stays recorded as desired, and status shows
// it as requested but not ready rather than as disabled or done.
func TestEnableSQS_InterruptedEnableStaysRequestedAndNotReady(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	before, err := DescribeSQS(ctx, js, "ap-southeast-2", SQSObservers{})
	if err != nil || before.Desired != "disabled" || before.Store != sqsStoreAbsent || before.Ready {
		t.Fatalf("status before enable = %+v, %v", before, err)
	}
	if _, err := EnableSQS(ctx, nc, js, []string{"node1"}, 1, enableInput(), SQSObservers{}, time.Second); err == nil {
		t.Fatal("enable with no node daemon answering succeeded")
	}
	after, err := DescribeSQS(ctx, js, "ap-southeast-2", SQSObservers{WorkerPolling: func() bool { return true }})
	if err != nil || after.Desired != "enabled" || after.NodeID != "node1" || after.Store != sqsStoreAbsent || after.Ready {
		t.Fatalf("status after interrupted enable = %+v, %v", after, err)
	}
	in := enableInput()
	in.NodeID = "node2"
	if _, err := EnableSQS(ctx, nc, js, []string{"node2"}, 1, in, SQSObservers{}, time.Second); err == nil {
		t.Fatal("enable on another node succeeded while node1 holds the service")
	}
}

// With the store bound and pinned and a worker polling, status still is not
// Ready while no tenant API is registered here, and stops being Ready as soon
// as the worker drops out of the poller window.
func TestDescribeSQS_ReadyNeedsTheTenantAPIAndAPollingWorker(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	const region, claim, vol, inst = "ap-southeast-2", "store-aaaa", "vol-00000000000000001", "i-00000000000000001"
	st, err := servicestate.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(ctx, region, servicestate.Enabled, "node1", time.Now()); err != nil {
		t.Fatal(err)
	}
	recs, err := storelaunch.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	for _, step := range []func() error{
		func() error { return recs.Begin(ctx, region, claim, "node1", pub, "t3.small", 8) },
		func() error { return recs.MarkVolumeAttempted(ctx, region, claim) },
		func() error { return recs.RecordVolume(ctx, region, claim, vol) },
		func() error { return recs.MarkNetworkAttempted(ctx, region, claim) },
		func() error { return recs.MarkDispatched(ctx, region, claim) },
		func() error { return recs.Bind(ctx, region, claim, inst, "10.0.0.2:4222") },
		func() error { return recs.RecordServerKey(ctx, region, inst, claim, vol, strings.Repeat("ab", 32)) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	polling := true
	obs := SQSObservers{WorkerPolling: func() bool { return polling }}

	got, err := DescribeSQS(ctx, js, region, obs)
	if err != nil || got.Store != sqsStoreReady || !got.WorkerPolling || got.TenantAPI != sqsTenantAPINotRegistered || got.Ready {
		t.Fatalf("unregistered tenant API = %+v, %v; want bound, polling, not-registered, not Ready", got, err)
	}
	obs.TenantAPIRegistered = func() bool { return true }
	if got, err = DescribeSQS(ctx, js, region, obs); err != nil || !got.Ready {
		t.Fatalf("registered = %+v, %v; want Ready", got, err)
	}
	polling = false
	if got, err = DescribeSQS(ctx, js, region, obs); err != nil || got.WorkerPolling || got.Ready {
		t.Fatalf("worker gone = %+v, %v; want not polling, not Ready", got, err)
	}
}

// stripContract rewrites a record as the release before authenticated
// bootstrap wrote it: the same fields, with no contract.
func stripContract(t *testing.T, kv jetstream.KeyValue, key string) uint64 {
	t.Helper()
	ctx := context.Background()
	e, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(e.Value(), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["contract"]; !ok {
		t.Fatal("record written by this release has no contract")
	}
	delete(m, "contract")
	b, _ := json.Marshal(m)
	rev, err := kv.Update(ctx, key, b, e.Revision())
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

// A store or worker record this release did not create blocks enable before
// it writes desired state or launches anything, leaves the record untouched,
// and status names it. nc is nil: any launch attempt would panic.
func TestEnableSQS_RefusesAPreAPIRecordBeforeAnyWrite(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	const region = "ap-southeast-2"
	for _, kind := range []string{"store", "worker"} {
		t.Run(kind, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			ctx := context.Background()
			var kv jetstream.KeyValue
			switch kind {
			case "store":
				recs, err := storelaunch.Open(ctx, js)
				if err != nil {
					t.Fatal(err)
				}
				if err := recs.Begin(ctx, region, "store-aaaa", "node1", storePublicKey(t), "t3.small", 8); err != nil {
					t.Fatal(err)
				}
				kv = recs.KV
			case "worker":
				ws, err := workerident.Open(ctx, js)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ws.BeginLaunch(ctx, region, workerident.LaunchRequest{ClaimID: "worker-aaaa", NodeID: "node1", InstanceType: "t3.small"}); err != nil {
					t.Fatal(err)
				}
				kv = ws.KV
			}
			rev := stripContract(t, kv, "region."+region)

			_, err := EnableSQS(ctx, nil, js, []string{"node1"}, 1, enableInput(), SQSObservers{}, time.Second)
			if err == nil || !strings.Contains(err.Error(), "pre-API SQS "+kind) || !strings.Contains(err.Error(), "volume kept") {
				t.Fatalf("enable = %v; want a pre-API %s refusal", err, kind)
			}
			if _, err := js.KeyValue(ctx, servicestate.Bucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
				t.Fatalf("service state bucket = %v; want none created", err)
			}
			if e, err := kv.Get(ctx, "region."+region); err != nil || e.Revision() != rev {
				t.Fatalf("pre-API record changed: %v", err)
			}
			got, err := DescribeSQS(ctx, js, region, SQSObservers{WorkerPolling: func() bool { return true }, TenantAPIRegistered: func() bool { return true }})
			if err != nil || !strings.Contains(got.Unsupported, "pre-API SQS "+kind) || got.Ready {
				t.Fatalf("status = %+v, %v; want the pre-API %s named and not Ready", got, err, kind)
			}
		})
	}
}

func storePublicKey(t *testing.T) string {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	return pub
}
