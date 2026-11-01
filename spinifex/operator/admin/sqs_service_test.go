package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/servicestate"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
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
	obs.TenantAPIRegistered = true
	if got, err = DescribeSQS(ctx, js, region, obs); err != nil || !got.Ready {
		t.Fatalf("registered = %+v, %v; want Ready", got, err)
	}
	polling = false
	if got, err = DescribeSQS(ctx, js, region, obs); err != nil || got.WorkerPolling || got.Ready {
		t.Fatalf("worker gone = %+v, %v; want not polling, not Ready", got, err)
	}
}
