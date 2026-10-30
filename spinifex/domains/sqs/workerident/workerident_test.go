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
	region = "ap-southeast-2"
	node   = "node-a"
	volume = "vol-0123456789abcdef0"
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

func launch(claim string) LaunchRequest {
	return LaunchRequest{ClaimID: claim, NodeID: node, DataVolumeID: volume, DataVolumeEncrypted: true}
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

func mustBound(t *testing.T, s Store, claimID, instanceID string) Intent {
	t.Helper()
	ctx := context.Background()
	in, err := s.BeginLaunch(ctx, region, launch(claimID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindInstance(ctx, in, instanceID); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestLifecycle_BindAuthorizesAndRestartKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	in, err := s.BeginLaunch(ctx, region, launch(claim(1)))
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

func TestReplacement_OldIndexIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	mustBound(t, s, claim(1), inst(1))
	in, err := s.BeginLaunch(ctx, region, launch(claim(2)))
	if err != nil || in.Generation != 2 {
		t.Fatalf("replacement intent = %+v, %v; want generation 2", in, err)
	}
	// The old worker stops being current at the intent, before any rebind.
	if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
		t.Fatalf("old worker during replacement = %v, want ErrNotCurrent", err)
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
	if err := s.BindInstance(ctx, Intent{Region: region, Generation: 1, ClaimID: claim(1)}, inst(1)); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("replayed old bind = %v, want ErrSuperseded", err)
	}
}

func TestCompetingLaunches_AuthorizeOneWorker(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	const n = 8
	intents := make([]Intent, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			in, err := s.BeginLaunch(ctx, region, launch(claim(i+1)))
			if err != nil && !errors.Is(err, ErrContended) {
				t.Errorf("begin %d: %v", i, err)
			}
			intents[i] = in
		})
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for _, in := range intents {
		if in.Generation != 0 {
			if seen[in.Generation] {
				t.Fatalf("two intents share generation %d", in.Generation)
			}
			seen[in.Generation] = true
		}
	}
	var bound atomic.Int32
	for i, in := range intents {
		if in.Generation == 0 {
			continue
		}
		wg.Go(func() {
			if err := s.BindInstance(ctx, in, inst(i+1)); err == nil {
				bound.Add(1)
			} else if !errors.Is(err, ErrSuperseded) && !errors.Is(err, ErrContended) {
				t.Errorf("bind %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	authorized := 0
	for i := range n {
		if _, err := s.Authorize(ctx, inst(i+1)); err == nil {
			authorized++
		}
	}
	if bound.Load() != 1 || authorized != 1 {
		t.Fatalf("bound=%d authorized=%d, want exactly one worker", bound.Load(), authorized)
	}
}

func TestFailedLaunch(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	in, _ := s.BeginLaunch(ctx, region, launch(claim(1)))
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
	if _, err := s.BeginLaunch(ctx, region, launch(claim(1))); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("restarting a failed attempt = %v, want ErrSuperseded", err)
	}
	next, err := s.BeginLaunch(ctx, region, launch(claim(2)))
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
	unencrypted := launch(claim(1))
	unencrypted.DataVolumeEncrypted = false
	if _, err := s.BeginLaunch(ctx, region, unencrypted); !errors.Is(err, ErrUnencryptedVolume) {
		t.Fatalf("unencrypted volume = %v", err)
	}
	mustBound(t, s, claim(1), inst(1))
	other := launch(claim(2))
	other.NodeID = "node-b"
	if _, err := s.BeginLaunch(ctx, region, other); !errors.Is(err, ErrWrongNode) {
		t.Fatalf("other node = %v, want ErrWrongNode", err)
	}
	other = launch(claim(2))
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
		mustBound(t, s, claim(1), inst(1))
		put(t, kv, indexKey(inst(9)), IndexEntry{Region: region, Generation: 1, ClaimID: claim(1)})
		if _, err := s.Authorize(ctx, inst(9)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("forged entry = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("bound instance whose entry was rewritten", func(t *testing.T) {
		s, kv := newStore(t)
		mustBound(t, s, claim(1), inst(1))
		put(t, kv, indexKey(inst(1)), IndexEntry{Region: region, Generation: 7, ClaimID: claim(7)})
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrNotCurrent) {
			t.Fatalf("rewritten entry = %v, want ErrNotCurrent", err)
		}
	})
	t.Run("entry for a launching record", func(t *testing.T) {
		s, kv := newStore(t)
		in, _ := s.BeginLaunch(ctx, region, launch(claim(1)))
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
		mustBound(t, s, claim(1), inst(1))
		kv.Put(ctx, indexKey(inst(2)), []byte("{"))
		if _, err := s.Authorize(ctx, inst(2)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("corrupt entry = %v, want ErrUnreadable", err)
		}
		kv.Put(ctx, regionKey(region), []byte(`{"region":"ap-southeast-2","state":"ready"}`))
		if _, err := s.Authorize(ctx, inst(1)); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("corrupt record = %v, want ErrUnreadable", err)
		}
		if _, err := s.BeginLaunch(ctx, region, launch(claim(2))); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("launch over a corrupt record = %v, want ErrUnreadable", err)
		}
	})
	t.Run("conflicting entry is not overwritten", func(t *testing.T) {
		s, kv := newStore(t)
		in, _ := s.BeginLaunch(ctx, region, launch(claim(1)))
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
		in, err := s.BeginLaunch(ctx, region, launch(claim(1)))
		if err != nil || in.Generation != 1 {
			t.Fatalf("intent = %+v, %v", in, err)
		}
		again, err := base.BeginLaunch(ctx, region, launch(claim(1)))
		if err != nil || again != in {
			t.Fatalf("retried intent = %+v, %v; want the same generation", again, err)
		}
	})
	t.Run("index and bind applied, replies lost, retried", func(t *testing.T) {
		base, kv := newStore(t)
		in, _ := base.BeginLaunch(ctx, region, launch(claim(1)))
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
		in, _ := base.BeginLaunch(ctx, region, launch(claim(1)))
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
		if _, err := s.BeginLaunch(ctx, region, launch(claim(1))); !errors.Is(err, ErrContended) {
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
	old, _ := s.BeginLaunch(ctx, region, launch(claim(1)))
	launching := raw(t, kv, regionKey(region))
	if err := s.BindInstance(ctx, old, inst(1)); err != nil {
		t.Fatal(err)
	}
	next, _ := s.BeginLaunch(ctx, region, launch(claim(2)))
	// The replica still shows generation 1 launching.
	stale := laggingReplica(t, s, launching)
	if err := stale.BindInstance(ctx, old, inst(3)); err == nil {
		t.Fatal("a stale read bound a superseded intent")
	}
	if _, err := stale.BeginLaunch(ctx, region, launch(claim(1))); err == nil {
		t.Fatal("a stale read revived a superseded intent")
	}
	if r := record(t, kv); r.Generation != next.Generation || r.ClaimID != next.ClaimID || r.State != StateLaunching {
		t.Fatalf("leader record = %+v, want the newer intent untouched", r)
	}
}

func TestStaleReads_KeepNoReplacedWorkerAuthorized(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	in, _ := s.BeginLaunch(ctx, region, launch(claim(1)))
	launching := raw(t, kv, regionKey(region))
	if err := s.BindInstance(ctx, in, inst(1)); err != nil {
		t.Fatal(err)
	}
	entry, bound := raw(t, kv, indexKey(inst(1))), raw(t, kv, regionKey(region))
	mustBound(t, s, claim(2), inst(2))
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
	mustBound(t, s, claim(1), inst(1))
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
