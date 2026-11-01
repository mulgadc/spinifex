package workerident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	region   = "ap-southeast-2"
	node     = "node-a"
	volume   = "vol-0123456789abcdef0"
	testType = "t3.small"
)

func newStore(t *testing.T) (Store, jetstream.KeyValue) {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	s, err := Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.KV
}

func launch(claim string, supersedes uint64) LaunchRequest {
	return LaunchRequest{ClaimID: claim, Supersedes: supersedes, NodeID: node, DataVolumeID: volume, DataVolumeEncrypted: true, InstanceType: testType}
}

func inst(n int) string { return fmt.Sprintf("i-%017x", n) }

func claim(n int) string { return fmt.Sprintf("claim-%04d", n) }

func raw(t *testing.T, kv jetstream.KeyValue, key string) []byte {
	t.Helper()
	e, err := kv.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return e.Value()
}

func record(t *testing.T, kv jetstream.KeyValue) Record {
	t.Helper()
	var r Record
	if err := json.Unmarshal(raw(t, kv, regionKey(region)), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func mustBound(t *testing.T, s Store, claimID, instanceID string, supersedes uint64) Intent {
	t.Helper()
	ctx := context.Background()
	in, err := s.BeginLaunch(ctx, region, launch(claimID, supersedes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindInstance(ctx, in, instanceID); err != nil {
		t.Fatal(err)
	}
	return in
}

func mustStop(t *testing.T, s Store, in Intent, instanceID string) {
	t.Helper()
	if err := s.RecordStopped(context.Background(), in, instanceID); err != nil {
		t.Fatalf("record stopped: %v", err)
	}
	fence(t, s, in.Generation+1)
}

// fence raises the first-store fence, as the launch path does once a stop or
// failure is confirmed and before it begins the replacement.
func fence(t *testing.T, s Store, gen uint64) {
	t.Helper()
	if err := s.Fences.(firststore.Store).Fence(context.Background(), region, gen); err != nil {
		t.Fatalf("fence: %v", err)
	}
}

func TestLifecycle_BindAuthorizesAndRestartKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	in, err := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	if err != nil || in.Generation != 1 {
		t.Fatalf("intent = %+v, %v; want generation 1", in, err)
	}
	if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("authorize before bind = %v, want ErrUnknownInstance", err)
	}
	if err := s.BindInstance(ctx, in, inst(1)); err != nil {
		t.Fatal(err)
	}
	want := Identity{Region: region, InstanceID: inst(1), Generation: 1, ClaimID: claim(1), NodeID: node, DataVolumeID: volume}
	// A same-instance reboot keeps the instance ID, so nothing is relaunched or
	// rebound and the identity is unchanged; a repeated bind is idempotent.
	for range 3 {
		got, err := s.Authorize(ctx, inst(1))
		if err != nil || got != want {
			t.Fatalf("authorize = %+v, %v; want %+v", got, err, want)
		}
		if err := s.BindInstance(ctx, in, inst(1)); err != nil {
			t.Fatalf("repeated bind = %v", err)
		}
	}
	if err := s.BindInstance(ctx, in, inst(2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second instance for a bound intent = %v, want ErrConflict", err)
	}
	if _, err := s.Authorize(ctx, inst(2)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("refused instance authorize = %v, want ErrNotCurrent", err)
	}
}

func TestReplacement_NeedsConfirmedStop(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	old := mustBound(t, s, claim(1), inst(1), 0)
	if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 1)); !errors.Is(err, ErrInFlight) {
		t.Fatalf("replacement of a bound worker = %v, want ErrInFlight", err)
	}
	if got, err := s.Authorize(ctx, inst(1)); err != nil || got.Generation != 1 {
		t.Fatalf("refused replacement disturbed the worker: %+v, %v", got, err)
	}
	if err := s.RecordStopped(ctx, old, inst(2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stop naming another instance = %v, want ErrConflict", err)
	}
	for range 2 {
		mustStop(t, s, old, inst(1))
	}
	// A stopped worker keeps its instance in the record but is not current.
	if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("stopped worker = %v, want ErrNotCurrent", err)
	}
	if err := s.BindInstance(ctx, old, inst(1)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("rebinding a stopped worker = %v, want ErrSuperseded", err)
	}
	in, err := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	if err != nil || in.Generation != 2 {
		t.Fatalf("replacement intent = %+v, %v; want generation 2", in, err)
	}
	if err := s.BindInstance(ctx, in, inst(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("old worker after replacement = %v, want ErrNotCurrent", err)
	}
	if got, err := s.Authorize(ctx, inst(2)); err != nil || got.Generation != 2 {
		t.Fatalf("new worker = %+v, %v", got, err)
	}
	if err := s.RecordStopped(ctx, old, inst(1)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stopping a superseded worker = %v, want ErrSuperseded", err)
	}
	if r := record(t, kv); r.State != StateBound || r.InstanceID != inst(2) {
		t.Fatalf("record = %+v", r)
	}
	if err := s.RecordStopped(ctx, Intent{Region: region, Generation: 3, ClaimID: claim(3)}, inst(2)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stop with an unrecorded intent = %v, want ErrSuperseded", err)
	}
}

// launcher stands in for a caller that starts a VM on the pinned volume as
// soon as BeginLaunch returns an intent.
type launcher struct {
	mu       sync.Mutex
	launched map[string]bool
}

func (l *launcher) start(t *testing.T, s Store, req LaunchRequest) (Intent, bool) {
	in, err := s.BeginLaunch(context.Background(), region, req)
	switch {
	case err == nil:
	case errors.Is(err, ErrInFlight), errors.Is(err, ErrSuperseded), errors.Is(err, ErrContended):
		return Intent{}, false
	default:
		t.Errorf("begin %s: %v", req.ClaimID, err)
		return Intent{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.launched == nil {
		l.launched = map[string]bool{}
	}
	l.launched[in.ClaimID] = true
	return in, true
}

func (l *launcher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.launched)
}

func TestCompetingLaunches_StartOneVM(t *testing.T) {
	for name, prior := range map[string]func(t *testing.T, s Store) uint64{
		"first worker": func(*testing.T, Store) uint64 { return 0 },
		"after a confirmed stop": func(t *testing.T, s Store) uint64 {
			mustStop(t, s, mustBound(t, s, claim(100), inst(100), 0), inst(100))
			return 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newStore(t)
			supersedes := prior(t, s)
			var l launcher
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Go(func() { l.start(t, s, launch(claim(i+1), supersedes)) })
			}
			wg.Wait()
			if l.count() != 1 {
				t.Fatalf("%d callers started a VM on the pinned volume, want 1", l.count())
			}
		})
	}
}

func TestAmbiguousPriorLaunch_BlocksOthers(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	var l launcher
	// A's VM launch returned no answer: it may or may not be running.
	a, ok := l.start(t, s, launch(claim(1), 0))
	if !ok {
		t.Fatal("first launch refused")
	}
	for range 3 {
		if _, ok := l.start(t, s, launch(claim(2), 1)); ok {
			t.Fatal("a second VM started while the first launch's outcome is unknown")
		}
		if _, ok := l.start(t, s, launch(claim(2), 0)); ok {
			t.Fatal("a second VM started from a stale generation")
		}
	}
	again, err := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	if err != nil || again != a {
		t.Fatalf("retry of the ambiguous launch = %+v, %v; want the same intent", again, err)
	}
	if err := s.FailLaunch(ctx, a); err != nil {
		t.Fatal(err)
	}
	fence(t, s, 2)
	if _, ok := l.start(t, s, launch(claim(2), 1)); !ok || l.count() != 2 {
		t.Fatalf("launch after confirmed failure refused; launched %d", l.count())
	}
}

func TestReplay_ABA(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	reqA := launch(claim(1), 0)
	a, _ := s.BeginLaunch(ctx, region, reqA)
	if err := s.FailLaunch(ctx, a); err != nil {
		t.Fatal(err)
	}
	fence(t, s, 2)
	b, err := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	if err != nil || b.Generation != 2 {
		t.Fatalf("B = %+v, %v", b, err)
	}
	if _, err := s.BeginLaunch(ctx, region, reqA); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("replayed A while B launches = %v, want ErrSuperseded", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 0)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("B retried with a generation it did not supersede = %v, want ErrSuperseded", err)
	}
	if err := s.BindInstance(ctx, b, inst(2)); err != nil {
		t.Fatal(err)
	}
	mustStop(t, s, b, inst(2))
	// Even once B is stopped, A's request names a generation it never observed.
	if _, err := s.BeginLaunch(ctx, region, reqA); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("replayed A after B stopped = %v, want ErrSuperseded", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 2)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("B's claim ID reused for generation 3 = %v, want ErrSuperseded", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(3), 7)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("a generation from the future = %v, want ErrSuperseded", err)
	}
	if c, err := s.BeginLaunch(ctx, region, launch(claim(3), 2)); err != nil || c.Generation != 3 {
		t.Fatalf("C = %+v, %v", c, err)
	}
	if _, err := s.BeginLaunch(ctx, "us-east-1", launch(claim(4), 3)); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseding in a Region with no record = %v, want ErrConflict", err)
	}
}

func TestFailedLaunch(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	in, _ := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	for range 2 {
		if err := s.FailLaunch(ctx, in); err != nil {
			t.Fatalf("fail launch = %v", err)
		}
	}
	if err := s.BindInstance(ctx, in, inst(1)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("bind after failure = %v, want ErrSuperseded", err)
	}
	// The bind wrote an index entry before it was refused; it proves nothing.
	if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("failed launch's instance = %v, want ErrNotCurrent", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("restarting a failed attempt = %v, want ErrSuperseded", err)
	}
	fence(t, s, 2)
	next, err := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	if err != nil || next.Generation != 2 {
		t.Fatalf("next launch = %+v, %v; want generation 2", next, err)
	}
	if err := s.FailLaunch(ctx, in); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("failing a superseded intent = %v, want ErrSuperseded", err)
	}
	if err := s.BindInstance(ctx, next, inst(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.FailLaunch(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("failing a bound intent = %v, want ErrConflict", err)
	}
}

func TestBeginLaunch_PinnedPlacementAndEncryption(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	unencrypted := launch(claim(1), 0)
	unencrypted.DataVolumeEncrypted = false
	if _, err := s.BeginLaunch(ctx, region, unencrypted); !errors.Is(err, ErrUnencryptedVolume) {
		t.Fatalf("unencrypted volume = %v", err)
	}
	mustBound(t, s, claim(1), inst(1), 0)
	other := launch(claim(2), 1)
	other.NodeID = "node-b"
	if _, err := s.BeginLaunch(ctx, region, other); !errors.Is(err, ErrWrongNode) {
		t.Fatalf("other node = %v, want ErrWrongNode", err)
	}
	other = launch(claim(2), 1)
	other.DataVolumeID = "vol-0fedcba9876543210"
	if _, err := s.BeginLaunch(ctx, region, other); !errors.Is(err, ErrVolumeMismatch) {
		t.Fatalf("other volume = %v, want ErrVolumeMismatch", err)
	}
	if _, err := s.Authorize(ctx, inst(1)); err != nil {
		t.Fatalf("a refused launch disturbed the worker: %v", err)
	}
}

func TestAuthorize_IndexIsNotProof(t *testing.T) {
	ctx := context.Background()
	put := func(t *testing.T, kv jetstream.KeyValue, key string, v any) {
		b, _ := json.Marshal(v)
		if _, err := kv.Put(ctx, key, b); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("entry whose generation is not the record's", func(t *testing.T) {
		s, kv := newStore(t)
		mustBound(t, s, claim(1), inst(1), 0)
		put(t, kv, indexKey(inst(9)), IndexEntry{Region: region, Generation: 1, ClaimID: claim(1)})
		if _, err := s.Authorize(ctx, inst(9)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("forged entry = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("bound instance whose entry was rewritten", func(t *testing.T) {
		s, kv := newStore(t)
		mustBound(t, s, claim(1), inst(1), 0)
		put(t, kv, indexKey(inst(1)), IndexEntry{Region: region, Generation: 7, ClaimID: claim(7)})
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("rewritten entry = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("entry for a launching record", func(t *testing.T) {
		s, kv := newStore(t)
		in, _ := s.BeginLaunch(ctx, region, launch(claim(1), 0))
		put(t, kv, indexKey(inst(1)), IndexEntry(in))
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("index without bind = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("entry without a regional record", func(t *testing.T) {
		s, kv := newStore(t)
		put(t, kv, indexKey(inst(1)), IndexEntry{Region: region, Generation: 1, ClaimID: claim(1)})
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("orphan entry = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("unreadable entry or record", func(t *testing.T) {
		s, kv := newStore(t)
		mustBound(t, s, claim(1), inst(1), 0)
		kv.Put(ctx, indexKey(inst(2)), []byte("{"))
		if _, err := s.Authorize(ctx, inst(2)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("corrupt entry = %v, want ErrUnreadable", err)
		}
		kv.Put(ctx, regionKey(region), []byte(`{"region":"ap-southeast-2","state":"ready"}`))
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("corrupt record = %v, want ErrUnreadable", err)
		}
		if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 0)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("launch over a corrupt record = %v, want ErrUnreadable", err)
		}
	})
	t.Run("conflicting entry is not overwritten", func(t *testing.T) {
		s, kv := newStore(t)
		in, _ := s.BeginLaunch(ctx, region, launch(claim(1), 0))
		put(t, kv, indexKey(inst(1)), IndexEntry{Region: region, Generation: 7, ClaimID: claim(7)})
		if err := s.BindInstance(ctx, in, inst(1)); !errors.Is(err, ErrConflict) {
			t.Fatalf("bind over a conflicting entry = %v, want ErrConflict", err)
		}
		if r := record(t, kv); r.State != StateLaunching {
			t.Fatalf("record state = %s, want launching", r.State)
		}
	})
}

// lossyKV applies or drops a write and then reports failure, as a write whose
// reply was lost or that never reached the server.
type lossyKV struct {
	jetstream.KeyValue

	failCreate, failUpdate int
	apply                  bool
}

func (k *lossyKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.failCreate > 0 {
		k.failCreate--
		if k.apply {
			k.KeyValue.Create(ctx, key, value, opts...)
		}
		return 0, errors.New("nats: timeout")
	}
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *lossyKV) Update(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	if k.failUpdate > 0 {
		k.failUpdate--
		if k.apply {
			k.KeyValue.Update(ctx, key, value, rev)
		}
		return 0, errors.New("nats: timeout")
	}
	return k.KeyValue.Update(ctx, key, value, rev)
}

func TestLostReplies(t *testing.T) {
	ctx := context.Background()
	t.Run("intent applied, reply lost, retried", func(t *testing.T) {
		base, kv := newStore(t)
		s := Store{JS: base.JS, KV: &lossyKV{KeyValue: kv, failCreate: 1, apply: true}}
		in, err := s.BeginLaunch(ctx, region, launch(claim(1), 0))
		if err != nil || in.Generation != 1 {
			t.Fatalf("intent = %+v, %v", in, err)
		}
		again, err := base.BeginLaunch(ctx, region, launch(claim(1), 0))
		if err != nil || again != in {
			t.Fatalf("retried intent = %+v, %v; want the same generation", again, err)
		}
	})
	t.Run("index and bind applied, replies lost, retried", func(t *testing.T) {
		base, kv := newStore(t)
		in, _ := base.BeginLaunch(ctx, region, launch(claim(1), 0))
		s := Store{JS: base.JS, KV: &lossyKV{KeyValue: kv, failCreate: 1, failUpdate: 1, apply: true}}
		if err := s.BindInstance(ctx, in, inst(1)); err != nil {
			t.Fatalf("bind = %v, want recovered success", err)
		}
		if _, err := base.Authorize(ctx, inst(1)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("bind reply lost and the caller gives up", func(t *testing.T) {
		base, kv := newStore(t)
		in, _ := base.BeginLaunch(ctx, region, launch(claim(1), 0))
		s := Store{JS: base.JS, KV: &lossyKV{KeyValue: kv, failUpdate: maxAttempts}}
		if err := s.BindInstance(ctx, in, inst(1)); !errors.Is(err, ErrContended) {
			t.Fatalf("bind = %v, want ErrContended", err)
		}
		if _, err := base.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("unbound instance = %v, want ErrNotCurrent", err)
		}
		if err := base.BindInstance(ctx, in, inst(1)); err != nil {
			t.Fatalf("retried bind = %v", err)
		}
	})
	t.Run("persistently dropped intent grants nothing", func(t *testing.T) {
		base, kv := newStore(t)
		s := Store{JS: base.JS, KV: &lossyKV{KeyValue: kv, failCreate: maxAttempts}}
		if _, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); !errors.Is(err, ErrContended) {
			t.Fatalf("intent = %v, want ErrContended", err)
		}
	})
}

// laggingReplica serves reads from a second server holding an older history
// of the record at the leader's past revisions; writes go to the real leader.
func laggingReplica(t *testing.T, leader Store, history ...[]byte) Store {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(context.Background(), BucketConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range history {
		kv.Put(context.Background(), regionKey(region), v)
	}
	return Store{JS: js, KV: leader.KV}
}

func TestStaleReads_GrantNothing(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	old, _ := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	launching := raw(t, kv, regionKey(region))
	if err := s.BindInstance(ctx, old, inst(1)); err != nil {
		t.Fatal(err)
	}
	mustStop(t, s, old, inst(1))
	next, _ := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	// The replica still shows generation 1 launching.
	stale := laggingReplica(t, s, launching)
	if err := stale.BindInstance(ctx, old, inst(3)); err == nil {
		t.Fatal("a stale read bound a superseded intent")
	}
	if _, err := stale.BeginLaunch(ctx, region, launch(claim(1), 0)); err == nil {
		t.Fatal("a stale read revived a superseded intent")
	}
	if r := record(t, kv); r.Generation != next.Generation || r.ClaimID != next.ClaimID || r.State != StateLaunching {
		t.Fatalf("leader record = %+v, want the newer intent untouched", r)
	}
}

func TestStaleReads_KeepNoReplacedWorkerAuthorized(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	in, _ := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	launching := raw(t, kv, regionKey(region))
	if err := s.BindInstance(ctx, in, inst(1)); err != nil {
		t.Fatal(err)
	}
	entry, bound := raw(t, kv, indexKey(inst(1))), raw(t, kv, regionKey(region))
	mustStop(t, s, in, inst(1))
	mustBound(t, s, claim(2), inst(2), 1)
	// The replica replays the leader's first three revisions, so it still shows
	// generation 1 bound to the old instance at the revisions it had then.
	_, _, js := testutil.StartTestJetStream(t)
	rkv, err := js.CreateKeyValue(ctx, BucketConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	rkv.Put(ctx, regionKey(region), launching)
	rkv.Put(ctx, indexKey(inst(1)), entry)
	rkv.Put(ctx, regionKey(region), bound)
	stale := Store{JS: js, KV: s.KV}
	if got, err := stale.Authorize(ctx, inst(1)); err == nil {
		t.Fatalf("a stale read kept the replaced worker authorized: %+v", got)
	}
	if got, err := s.Authorize(ctx, inst(2)); err != nil || got.Generation != 2 {
		t.Fatalf("current worker = %+v, %v", got, err)
	}
}

// Authorization reads must be leader-answered message gets, never direct gets.
func TestReads_UseLeaderMessageGet(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, nc, js := testutil.StartTestJetStream(t)
	s, err := Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	var direct, leader atomic.Int32
	for subj, n := range map[string]*atomic.Int32{
		"$JS.API.DIRECT.GET.KV_" + Bucket:        &direct,
		"$JS.API.DIRECT.GET.KV_" + Bucket + ".>": &direct,
		"$JS.API.STREAM.MSG.GET.KV_" + Bucket:    &leader,
	} {
		sub, _ := nc.Subscribe(subj, func(*nats.Msg) { n.Add(1) })
		defer sub.Unsubscribe()
	}
	nc.Flush()
	mustBound(t, s, claim(1), inst(1), 0)
	if _, err := s.Authorize(context.Background(), inst(1)); err != nil {
		t.Fatal(err)
	}
	nc.Flush()
	if direct.Load() != 0 || leader.Load() < 2 {
		t.Fatalf("direct gets=%d leader gets=%d; want only leader reads", direct.Load(), leader.Load())
	}
}

func TestOpen_RefusesNonDurableBuckets(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	ctx := context.Background()
	for name, cfg := range map[string]jetstream.KeyValueConfig{
		"expiring": {Bucket: Bucket, History: 1, Storage: jetstream.FileStorage, TTL: time.Hour},
		"memory":   {Bucket: Bucket, History: 1, Storage: jetstream.MemoryStorage},
		"capped":   {Bucket: Bucket, History: 1, Storage: jetstream.FileStorage, MaxBytes: 1 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			if _, err := js.CreateKeyValue(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(ctx, js); err == nil {
				t.Fatal("opened a bucket that can lose records")
			}
		})
	}
	// Settings a bucket can only get from a stream edited or created outside
	// the KV API; each could expire, lose, lag or refuse an authority record.
	stream := "KV_" + Bucket
	subjects := []string{"$KV." + Bucket + ".>"}
	kvStream := func(t *testing.T, js jetstream.JetStream) jetstream.StreamConfig {
		t.Helper()
		if _, err := js.CreateKeyValue(ctx, BucketConfig(1)); err != nil {
			t.Fatal(err)
		}
		info, err := js.Stream(ctx, stream)
		if err != nil {
			t.Fatal(err)
		}
		return info.CachedInfo().Config
	}
	for name, edit := range map[string]func(*jetstream.StreamConfig){
		"per-message TTL":         func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true },
		"sealed":                  func(c *jetstream.StreamConfig) { c.Sealed = true },
		"discard new per subject": func(c *jetstream.StreamConfig) { c.DiscardNewPerSubject = true; c.MaxMsgsPerSubject = 1 },
		"unacknowledged writes":   func(c *jetstream.StreamConfig) { c.NoAck = true },
		"message size cap":        func(c *jetstream.StreamConfig) { c.MaxMsgSize = 4096 },
		"discard old":             func(c *jetstream.StreamConfig) { c.Discard = jetstream.DiscardOld },
		"expiring delete markers": func(c *jetstream.StreamConfig) { c.AllowMsgTTL = true; c.SubjectDeleteMarkerTTL = time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			cfg := kvStream(t, js)
			edit(&cfg)
			if _, err := js.UpdateStream(ctx, cfg); err != nil {
				t.Fatalf("prepare bucket: %v", err)
			}
			if _, err := Open(ctx, js); err == nil {
				t.Fatal("opened a bucket that can lose or refuse records")
			}
		})
	}
	for name, cfg := range map[string]jetstream.StreamConfig{
		"work-queue retention": {Name: stream, Subjects: subjects, Retention: jetstream.WorkQueuePolicy,
			MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage, AllowDirect: true, AllowRollup: true},
		"interest retention": {Name: stream, Subjects: subjects, Retention: jetstream.InterestPolicy,
			MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage, AllowDirect: true, AllowRollup: true},
		"mirror": {Name: stream, Mirror: &jetstream.StreamSource{Name: "SRC"},
			MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage},
		"sources": {Name: stream, Subjects: subjects, Sources: []*jetstream.StreamSource{{Name: "SRC"}},
			MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage, AllowDirect: true, AllowRollup: true},
		"asynchronous flush": {Name: stream, Subjects: subjects, PersistMode: jetstream.AsyncPersistMode,
			MaxMsgsPerSubject: 1, Discard: jetstream.DiscardNew, Storage: jetstream.FileStorage, AllowDirect: true, AllowRollup: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "SRC", Subjects: []string{"src.>"}, Storage: jetstream.FileStorage}); err != nil {
				t.Fatal(err)
			}
			if _, err := js.CreateStream(ctx, cfg); err != nil {
				t.Fatalf("prepare bucket: %v", err)
			}
			if _, err := Open(ctx, js); err == nil {
				t.Fatal("opened a bucket that can lose, lag or refuse records")
			}
		})
	}
	t.Run("first-store bucket", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		cfg := firststore.BucketConfig(1)
		cfg.TTL = time.Hour
		if _, err := js.CreateKeyValue(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := firststore.Open(ctx, js); err == nil {
			t.Fatal("opened an expiring first-store bucket")
		}
	})
	t.Run("absent bucket is created durable and reopens", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		for range 2 {
			if _, err := Open(ctx, js); err != nil {
				t.Fatal(err)
			}
		}
		clustersize.RedeclareForTest(t, 3)
		if _, err := Open(ctx, js); err == nil {
			t.Fatal("opened a single-replica bucket when three were required")
		}
	})
}

// A reader that must not provision state binds the existing bucket only: an
// absent one stays absent, and a non-durable one is refused as Open refuses it.
func TestBind_NeverCreatesAndRefusesNonDurable(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	ctx := context.Background()
	t.Run("absent bucket", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		if _, err := Bind(ctx, js); err == nil {
			t.Fatal("bound a bucket that does not exist")
		}
		if _, err := firststore.Bind(ctx, js); err == nil {
			t.Fatal("bound a first-store bucket that does not exist")
		}
		for _, name := range []string{Bucket, firststore.Bucket} {
			if _, err := js.KeyValue(ctx, name); !errors.Is(err, jetstream.ErrBucketNotFound) {
				t.Fatalf("bind provisioned %s: %v", name, err)
			}
		}
	})
	t.Run("existing durable bucket", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		if _, err := Open(ctx, js); err != nil {
			t.Fatal(err)
		}
		if _, err := Bind(ctx, js); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("existing expiring bucket", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		cfg := BucketConfig(1)
		cfg.TTL = time.Hour
		if _, err := js.CreateKeyValue(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := Bind(ctx, js); err == nil {
			t.Fatal("bound an expiring bucket")
		}
	})
}

// A confirmed stop is not enough to replace a worker: the first-store fence
// must already exclude its generation, and a store with no fence reader
// refuses every replacement.
func TestReplacement_NeedsTheFirstStoreFence(t *testing.T) {
	s, kv := newStore(t)
	ctx := context.Background()
	first := mustBound(t, s, claim(1), inst(1), 0)
	if err := s.RecordStopped(ctx, first, inst(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 1)); !errors.Is(err, ErrNotFenced) {
		t.Fatalf("replacement before the fence = %v, want ErrNotFenced", err)
	}
	if r := record(t, kv); r.Generation != 1 || r.State != StateStopped {
		t.Fatalf("refused replacement changed the record: %+v", r)
	}
	unfenced := Store{JS: s.JS, KV: s.KV}
	fence(t, s, 2)
	if _, err := unfenced.BeginLaunch(ctx, region, launch(claim(2), 1)); !errors.Is(err, ErrNotFenced) {
		t.Fatalf("replacement with no fence reader = %v, want ErrNotFenced", err)
	}
	if in, err := s.BeginLaunch(ctx, region, launch(claim(2), 1)); err != nil || in.Generation != 2 {
		t.Fatalf("replacement after the fence = %+v, %v", in, err)
	}
}

// A launch is marked dispatched exactly once, and only for the current
// launching intent; a replay after a lost reply is refused, never resent.
func TestMarkDispatched_OnceForTheCurrentIntent(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	in, err := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, in); err != nil {
		t.Fatal(err)
	}
	if !record(t, kv).Dispatched {
		t.Fatal("record not marked dispatched")
	}
	if err := s.MarkDispatched(ctx, in); !errors.Is(err, ErrDispatched) {
		t.Fatalf("second dispatch = %v, want ErrDispatched", err)
	}
	if again, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); err != nil || again != in {
		t.Fatalf("same-claim retry = %+v, %v", again, err)
	}
	if !record(t, kv).Dispatched {
		t.Fatal("a same-claim retry cleared the dispatch mark")
	}
	stale := in
	stale.ClaimID = claim(9)
	if err := s.MarkDispatched(ctx, stale); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("other claim = %v, want ErrSuperseded", err)
	}
	if err := s.BindInstance(ctx, in, inst(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDispatched(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("dispatch after bind = %v, want ErrConflict", err)
	}
	// A replacement starts undispatched.
	mustStop(t, s, in, inst(1))
	next, err := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	if err != nil {
		t.Fatal(err)
	}
	if record(t, kv).Dispatched {
		t.Fatal("the replacement inherited the dispatch mark")
	}
	if err := s.MarkDispatched(ctx, next); err != nil {
		t.Fatal(err)
	}
}

func TestMarkNetworkAttempted_OnceForTheCurrentIntent(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	in, err := s.BeginLaunch(ctx, region, launch(claim(1), 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNetworkAttempted(ctx, in); err != nil {
		t.Fatal(err)
	}
	if !record(t, kv).NetworkAttempted {
		t.Fatal("record not marked")
	}
	if err := s.MarkNetworkAttempted(ctx, in); !errors.Is(err, ErrNetworkAttempted) {
		t.Fatalf("second mark = %v, want ErrNetworkAttempted", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); err != nil || !record(t, kv).NetworkAttempted {
		t.Fatalf("a same-claim retry cleared the mark: %v", err)
	}
	stale := in
	stale.ClaimID = claim(9)
	if err := s.MarkNetworkAttempted(ctx, stale); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("other claim = %v, want ErrSuperseded", err)
	}
	if err := s.BindInstance(ctx, in, inst(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNetworkAttempted(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("mark after bind = %v, want ErrConflict", err)
	}
	// A replacement's new claim starts unmarked.
	mustStop(t, s, in, inst(1))
	next, err := s.BeginLaunch(ctx, region, launch(claim(2), 1))
	if err != nil {
		t.Fatal(err)
	}
	if record(t, kv).NetworkAttempted {
		t.Fatal("a replacement inherited the network mark")
	}
	if err := s.MarkNetworkAttempted(ctx, next); err != nil {
		t.Fatal(err)
	}
}

// The instance type is part of a generation's intent: a retry of the same
// claim must name the same type, and a malformed type records nothing.
func TestBeginLaunch_InstanceTypeIsFixedForAGeneration(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	bad := launch(claim(1), 0)
	bad.InstanceType = ""
	if _, err := s.BeginLaunch(ctx, region, bad); err == nil {
		t.Fatal("an empty instance type was accepted")
	}
	if _, err := s.Current(ctx, region); err == nil {
		t.Fatalf("a refused type recorded state: %v", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); err != nil {
		t.Fatal(err)
	}
	other := launch(claim(1), 0)
	other.InstanceType = "t3.large"
	if _, err := s.BeginLaunch(ctx, region, other); !errors.Is(err, ErrTypeChanged) {
		t.Fatalf("retry with another type = %v, want ErrTypeChanged", err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(1), 0)); err != nil {
		t.Fatalf("retry with the same type = %v", err)
	}
	if rec, err := s.Current(ctx, region); err != nil || rec.InstanceType != testType {
		t.Fatalf("record = %+v, %v; want type %s", rec, err, testType)
	}
}

// A first launch is recorded before its volume exists. Its one create is
// marked once, a proven unsent create may withdraw the mark, the returned
// volume is recorded once, and nothing without a volume can be dispatched.
func TestVolumeIntent_MarkedOnceBeforeTheCreate(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	req := launch(claim(1), 0)
	req.DataVolumeID, req.DataVolumeEncrypted = "", false
	in, err := s.BeginLaunch(ctx, region, req)
	if err != nil {
		t.Fatal(err)
	}
	if rec := record(t, kv); rec.State != StateLaunching || rec.DataVolumeID != "" || rec.Allocated() {
		t.Fatalf("intent = %+v", rec)
	}
	if err := s.MarkDispatched(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("dispatch without a volume = %v, want ErrConflict", err)
	}
	if err := s.RecordVolume(ctx, in, volume); !errors.Is(err, ErrConflict) {
		t.Fatalf("record before the mark = %v, want ErrConflict", err)
	}
	if err := s.MarkVolumeAttempted(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVolumeAttempted(ctx, in); !errors.Is(err, ErrVolumeAttempted) {
		t.Fatalf("second mark = %v, want ErrVolumeAttempted", err)
	}
	if again, err := s.BeginLaunch(ctx, region, req); err != nil || again != in || !record(t, kv).VolumeAttempted {
		t.Fatalf("same-claim retry = %+v, %v; record %+v", again, err, record(t, kv))
	}
	if err := s.ClearVolumeAttempt(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVolumeAttempted(ctx, in); err != nil {
		t.Fatalf("mark after a withdrawn attempt = %v", err)
	}
	if err := s.RecordVolume(ctx, in, volume); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVolume(ctx, in, volume); err != nil {
		t.Fatalf("lost-reply retry = %v", err)
	}
	if err := s.RecordVolume(ctx, in, "vol-0fedcba9876543210"); !errors.Is(err, ErrVolumeMismatch) {
		t.Fatalf("other volume = %v, want ErrVolumeMismatch", err)
	}
	if err := s.ClearVolumeAttempt(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("clear after record = %v, want ErrConflict", err)
	}
	stale := in
	stale.ClaimID = claim(9)
	if err := s.MarkVolumeAttempted(ctx, stale); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("other claim = %v, want ErrSuperseded", err)
	}
	if err := s.MarkDispatched(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVolume(ctx, in, volume); !errors.Is(err, ErrConflict) {
		t.Fatalf("record after dispatch = %v, want ErrConflict", err)
	}
}

// A replacement after a launch that failed with its create unsettled keeps
// the mark, so the create is still never sent again.
func TestVolumeIntent_ReplacementKeepsAnUnsettledMark(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	req := launch(claim(1), 0)
	req.DataVolumeID, req.DataVolumeEncrypted = "", false
	in, err := s.BeginLaunch(ctx, region, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVolumeAttempted(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.FailLaunch(ctx, in); err != nil {
		t.Fatal(err)
	}
	fence(t, s, 2)
	next := launch(claim(2), 1)
	next.DataVolumeID, next.DataVolumeEncrypted = "", false
	in2, err := s.BeginLaunch(ctx, region, next)
	if err != nil {
		t.Fatal(err)
	}
	if rec := record(t, kv); !rec.VolumeAttempted || rec.DataVolumeID != "" {
		t.Fatalf("replacement = %+v, want the unsettled mark kept", rec)
	}
	if err := s.MarkVolumeAttempted(ctx, in2); !errors.Is(err, ErrVolumeAttempted) {
		t.Fatalf("mark on replacement = %v, want ErrVolumeAttempted", err)
	}
}

// Only an undispatched or failed launch may lack a volume, and a recorded
// volume must have been marked first.
func TestReadRecord_RefusesAVolumeOutsideItsIntent(t *testing.T) {
	ctx := context.Background()
	for name, rec := range map[string]Record{
		"dispatched without volume": {Region: region, State: StateLaunching, Generation: 1, ClaimID: claim(1),
			NodeID: node, InstanceType: testType, VolumeAttempted: true, Dispatched: true},
		"bound without volume": {Region: region, State: StateBound, Generation: 1, ClaimID: claim(1),
			NodeID: node, InstanceType: testType, InstanceID: inst(1)},
		"volume without mark": {Region: region, State: StateLaunching, Generation: 1, ClaimID: claim(1),
			NodeID: node, InstanceType: testType, DataVolumeID: volume},
	} {
		s, kv := newStore(t)
		b, _ := json.Marshal(rec)
		if _, err := kv.Put(ctx, regionKey(region), b); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Current(ctx, region); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: %v, want ErrUnreadable", name, err)
		}
	}
}

// A replacement reuses its predecessor's data volume, so it keeps that
// predecessor's contract: replacement never makes a contract supported.
func TestReplacement_KeepsAnUnsupportedContract(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	old := mustBound(t, s, claim(1), inst(1), 0)
	mustStop(t, s, old, inst(1))
	r := record(t, kv)
	if r.UnsupportedContract() {
		t.Fatalf("first launch by this release = %+v; want the authenticated contract", r)
	}
	r.Contract = 0
	b, _ := json.Marshal(r)
	if _, err := kv.Put(ctx, regionKey(region), b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginLaunch(ctx, region, launch(claim(2), 1)); err != nil {
		t.Fatal(err)
	}
	if got := record(t, kv); got.Generation != 2 || !got.UnsupportedContract() {
		t.Fatalf("replacement = %+v; want generation 2, still unsupported", got)
	}
}
