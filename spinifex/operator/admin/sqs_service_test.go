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
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go/jetstream"
)

const testRegion = "ap-southeast-2"

func enableInput() SQSEnableInput {
	return SQSEnableInput{Region: testRegion, NodeID: "node1", InstanceType: "t3.small", VolumeSizeGiB: 8,
		AccountID: "000000000000"}
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

// An enable whose launch fails stays recorded as desired, and status shows
// it as requested but not ready rather than as disabled or done.
func TestEnableSQS_InterruptedEnableStaysRequestedAndNotReady(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	before, err := DescribeSQS(ctx, js, testRegion, SQSObservers{})
	if err != nil || before.Desired != "disabled" || before.Instance != sqsInstanceAbsent || before.Ready {
		t.Fatalf("status before enable = %+v, %v", before, err)
	}
	if _, err := EnableSQS(ctx, nc, js, []string{"node1"}, 1, enableInput(), SQSObservers{}, time.Second); err == nil {
		t.Fatal("enable with no node daemon answering succeeded")
	}
	after, err := DescribeSQS(ctx, js, testRegion, SQSObservers{WorkerPolling: func() bool { return true }})
	if err != nil || after.Desired != "enabled" || after.NodeID != "node1" || after.Instance != sqsInstanceAbsent || after.Ready {
		t.Fatalf("status after interrupted enable = %+v, %v", after, err)
	}
	in := enableInput()
	in.NodeID = "node2"
	if _, err := EnableSQS(ctx, nc, js, []string{"node2"}, 1, in, SQSObservers{}, time.Second); err == nil {
		t.Fatal("enable on another node succeeded while node1 holds the service")
	}
}

// boundVM records the Region's SQS VM launched, bound and, if fingerprint is
// set, with its receipt key recorded by the guest.
func boundVM(t *testing.T, js jetstream.JetStream, fingerprint string) workerident.Store {
	t.Helper()
	ctx := context.Background()
	ws, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ws.BeginLaunch(ctx, testRegion, workerident.LaunchRequest{ClaimID: "worker-aaaa", NodeID: "node1",
		InstanceType: "t3.small", SizeGiB: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return ws.MarkVolumeAttempted(ctx, in) },
		func() error { return ws.RecordVolume(ctx, in, "vol-00000000000000001") },
		func() error { return ws.BindInstance(ctx, in, "i-00000000000000001") },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if fingerprint != "" {
		id, err := ws.Authorize(ctx, "i-00000000000000001")
		if err != nil {
			t.Fatal(err)
		}
		if err := ws.RecordReceiptKey(ctx, id, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// Ready needs the VM bound with its receipt key recorded, a polling worker
// whose local store is up, and the tenant API registered here.
func TestDescribeSQS_ReadyNeedsTheReceiptKeyTenantAPIAndAPollingWorker(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	st, err := servicestate.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(ctx, testRegion, servicestate.Enabled, "node1", time.Now()); err != nil {
		t.Fatal(err)
	}
	ws := boundVM(t, js, "")
	polling := true
	obs := SQSObservers{WorkerPolling: func() bool { return polling }, TenantAPIRegistered: func() bool { return true }}
	if got, err := DescribeSQS(ctx, js, testRegion, obs); err != nil || got.ReceiptKey != string(workerident.ReceiptPending) || got.Ready {
		t.Fatalf("pending receipt key = %+v, %v; want not Ready", got, err)
	}
	id, err := ws.Authorize(ctx, "i-00000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.RecordReceiptKey(ctx, id, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if got, err := DescribeSQS(ctx, js, testRegion, obs); err != nil || !got.Ready {
		t.Fatalf("recorded = %+v, %v; want Ready", got, err)
	}
	obs.TenantAPIRegistered = nil
	if got, err := DescribeSQS(ctx, js, testRegion, obs); err != nil || got.TenantAPI != sqsTenantAPINotRegistered || got.Ready {
		t.Fatalf("unregistered tenant API = %+v, %v; want not Ready", got, err)
	}
	obs.TenantAPIRegistered = func() bool { return true }
	polling = false
	if got, err := DescribeSQS(ctx, js, testRegion, obs); err != nil || got.WorkerPolling || got.Ready {
		t.Fatalf("worker gone = %+v, %v; want not polling, not Ready", got, err)
	}
}

// stripContract rewrites a record without its contract, as an older writer
// would leave it.
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

// A worker record with no launch contract, or a separate store record from
// the two-VM layout, blocks enable before it writes desired state or launches
// anything, is left untouched, and is named by status. nc is nil: any launch
// attempt would panic.
func TestEnableSQS_RefusesUnsupportedRecordsBeforeAnyWrite(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	for _, tc := range []struct {
		name, reason string
		seed         func(t *testing.T, js jetstream.JetStream) (jetstream.KeyValue, uint64)
	}{
		{"worker without contract", "missing or unsupported launch contract",
			func(t *testing.T, js jetstream.JetStream) (jetstream.KeyValue, uint64) {
				ws := boundVM(t, js, "")
				return ws.KV, stripContract(t, ws.KV, "region."+testRegion)
			}},
		{"separate store record", "separate SQS store VM record",
			func(t *testing.T, js jetstream.JetStream) (jetstream.KeyValue, uint64) {
				kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: legacyStoreBucket})
				if err != nil {
					t.Fatal(err)
				}
				rev, err := kv.Put(context.Background(), "region."+testRegion, []byte(`{"state":"bound"}`))
				if err != nil {
					t.Fatal(err)
				}
				return kv, rev
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			ctx := context.Background()
			kv, rev := tc.seed(t, js)

			_, err := EnableSQS(ctx, nil, js, []string{"node1"}, 1, enableInput(), SQSObservers{}, time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("enable = %v; want a %q refusal", err, tc.reason)
			}
			if _, err := js.KeyValue(ctx, servicestate.Bucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
				t.Fatalf("service state bucket = %v; want none created", err)
			}
			if e, err := kv.Get(ctx, "region."+testRegion); err != nil || e.Revision() != rev {
				t.Fatalf("refused record changed: %v", err)
			}
			got, err := DescribeSQS(ctx, js, testRegion, SQSObservers{WorkerPolling: func() bool { return true },
				TenantAPIRegistered: func() bool { return true }})
			if err != nil || !strings.Contains(got.Unsupported, tc.reason) || got.Ready {
				t.Fatalf("status = %+v, %v; want it named and not Ready", got, err)
			}
		})
	}
}
