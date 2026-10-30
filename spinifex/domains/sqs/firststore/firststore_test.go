package firststore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

const region = "ap-southeast-2"

func newStore(t *testing.T) (Store, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(context.Background(), BucketConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	return Store{KV: kv}, kv
}

func ident(n int, gen uint64) Identity {
	return Identity{InstanceID: fmt.Sprintf("i-%017x", n), ClaimID: fmt.Sprintf("claim-%04d-%d", n, gen), Generation: gen}
}

func userPub(t *testing.T) (pub, seed string) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := kp.PublicKey()
	s, _ := kp.Seed()
	return p, string(s)
}

func stored(t *testing.T, kv jetstream.KeyValue) string {
	t.Helper()
	e, err := kv.Get(context.Background(), key(region))
	if err != nil {
		t.Fatal(err)
	}
	return string(e.Value())
}

func TestClaim_FirstClaimCreatesKeyThenOnlyTheRecordedKey(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	w := ident(1, 1)
	d, err := s.Claim(ctx, region, w)
	if err != nil || !d.CreateKey {
		t.Fatalf("first claim = %+v, %v; want CreateKey", d, err)
	}
	pub, _ := userPub(t)
	if err := s.RecordPublicKey(ctx, region, w, pub); err != nil {
		t.Fatal(err)
	}
	d, err = s.Claim(ctx, region, w)
	if err != nil || d.CreateKey || d.PublicKey != pub {
		t.Fatalf("after record = %+v, %v; want only the recorded key", d, err)
	}
}

func TestClaim_ConcurrentClaimsHaveOneWinner(t *testing.T) {
	s, _ := newStore(t)
	const n = 16
	var created, held atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			d, err := s.Claim(context.Background(), region, ident(i+1, 1))
			switch {
			case err == nil && d.CreateKey:
				created.Add(1)
			case errors.Is(err, ErrClaimHeld):
				held.Add(1)
			default:
				t.Errorf("claim %d = %+v, %v", i, d, err)
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 || held.Load() != n-1 {
		t.Fatalf("created=%d held=%d, want exactly one winner", created.Load(), held.Load())
	}
}

func TestClaim_GenerationRules(t *testing.T) {
	ctx := context.Background()
	t.Run("newer generation takes over a claim with no key", func(t *testing.T) {
		s, _ := newStore(t)
		old, next := ident(1, 1), ident(2, 2)
		if _, err := s.Claim(ctx, region, old); err != nil {
			t.Fatal(err)
		}
		d, err := s.Claim(ctx, region, next)
		if err != nil || !d.CreateKey {
			t.Fatalf("takeover = %+v, %v", d, err)
		}
		pub, _ := userPub(t)
		if err := s.RecordPublicKey(ctx, region, old, pub); !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("superseded record = %v, want ErrStaleGeneration", err)
		}
		if _, err := s.Claim(ctx, region, old); !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("superseded claim = %v, want ErrStaleGeneration", err)
		}
	})
	t.Run("newer generation after recording gets only the recorded key", func(t *testing.T) {
		s, _ := newStore(t)
		old, next := ident(1, 1), ident(2, 2)
		s.Claim(ctx, region, old)
		pub, _ := userPub(t)
		if err := s.RecordPublicKey(ctx, region, old, pub); err != nil {
			t.Fatal(err)
		}
		d, err := s.Claim(ctx, region, next)
		if err != nil || d.CreateKey || d.PublicKey != pub {
			t.Fatalf("relaunched worker = %+v, %v; want the recorded key", d, err)
		}
		other, _ := userPub(t)
		if err := s.RecordPublicKey(ctx, region, next, other); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("new key after recording = %v, want ErrKeyMismatch", err)
		}
		if _, err := s.Claim(ctx, region, old); !errors.Is(err, ErrStaleGeneration) {
			t.Fatalf("replayed old boot = %v, want ErrStaleGeneration", err)
		}
	})
	t.Run("different identity at the same generation is refused", func(t *testing.T) {
		s, _ := newStore(t)
		s.Claim(ctx, region, ident(1, 3))
		rival := ident(2, 3)
		if _, err := s.Claim(ctx, region, rival); !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("rival claim = %v, want ErrClaimHeld", err)
		}
		pub, _ := userPub(t)
		if err := s.RecordPublicKey(ctx, region, rival, pub); !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("rival record = %v, want ErrClaimHeld", err)
		}
		sameInstance := ident(1, 3)
		sameInstance.ClaimID = "claim-replayed-x"
		if _, err := s.Claim(ctx, region, sameInstance); !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("replayed claim ID = %v, want ErrClaimHeld", err)
		}
	})
	t.Run("newer generation must claim before recording", func(t *testing.T) {
		s, _ := newStore(t)
		s.Claim(ctx, region, ident(1, 1))
		pub, _ := userPub(t)
		if err := s.RecordPublicKey(ctx, region, ident(2, 2), pub); !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("unclaimed newer record = %v, want ErrClaimHeld", err)
		}
	})
}

func TestRecordPublicKey_IdempotentAndFailClosed(t *testing.T) {
	ctx := context.Background()
	s, kv := newStore(t)
	w := ident(1, 1)
	pub, seed := userPub(t)
	if err := s.RecordPublicKey(ctx, region, w, pub); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("record without claim = %v, want ErrNotClaimed", err)
	}
	s.Claim(ctx, region, w)
	err := s.RecordPublicKey(ctx, region, w, seed)
	if err == nil || strings.Contains(err.Error(), seed) {
		t.Fatalf("seed as public key = %v; want a refusal without the seed", err)
	}
	if err := s.RecordPublicKey(ctx, region, w, pub); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPublicKey(ctx, region, w, pub); err != nil {
		t.Fatalf("retried record = %v, want idempotent success", err)
	}
	other, _ := userPub(t)
	if err := s.RecordPublicKey(ctx, region, w, other); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("second key = %v, want ErrKeyMismatch", err)
	}
	if strings.Contains(stored(t, kv), seed) {
		t.Fatal("the record holds a seed")
	}
}

func TestRecordLaunchIntent_NeedsRecordedKey(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	w := ident(1, 1)
	pub, _ := userPub(t)
	if err := s.RecordLaunchIntent(ctx, region, w, pub); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("launch without claim = %v", err)
	}
	s.Claim(ctx, region, w)
	if err := s.RecordLaunchIntent(ctx, region, w, pub); !errors.Is(err, ErrKeyNotRecorded) {
		t.Fatalf("launch before key = %v, want ErrKeyNotRecorded", err)
	}
	s.RecordPublicKey(ctx, region, w, pub)
	other, _ := userPub(t)
	if err := s.RecordLaunchIntent(ctx, region, w, other); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("launch with another key = %v, want ErrKeyMismatch", err)
	}
	for range 2 {
		if err := s.RecordLaunchIntent(ctx, region, w, pub); err != nil {
			t.Fatalf("launch intent = %v", err)
		}
	}
	d, err := s.Claim(ctx, region, w)
	if err != nil || d.CreateKey || d.PublicKey != pub {
		t.Fatalf("claim after launch intent = %+v, %v", d, err)
	}
	if err := s.RecordPublicKey(ctx, region, w, other); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("new key after launch intent = %v, want ErrKeyMismatch", err)
	}
}

func TestClaim_UnreadableRecordAuthorizesNothing(t *testing.T) {
	ctx := context.Background()
	pub, _ := userPub(t)
	for name, raw := range map[string]string{
		"not json":           "{",
		"unknown state":      `{"region":"ap-southeast-2","state":"ready","worker":{"instanceID":"i-00000000000000001","claimID":"claim-0001-1","generation":1}}`,
		"keyed without key":  `{"region":"ap-southeast-2","state":"key-recorded","worker":{"instanceID":"i-00000000000000001","claimID":"claim-0001-1","generation":1}}`,
		"claimed with a key": `{"region":"ap-southeast-2","state":"claimed","worker":{"instanceID":"i-00000000000000001","claimID":"claim-0001-1","generation":1},"publicKey":"` + pub + `"}`,
		"other region":       `{"region":"us-east-1","state":"claimed","worker":{"instanceID":"i-00000000000000001","claimID":"claim-0001-1","generation":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, kv := newStore(t)
			if _, err := kv.Put(ctx, key(region), []byte(raw)); err != nil {
				t.Fatal(err)
			}
			d, err := s.Claim(ctx, region, ident(1, 1))
			if !errors.Is(err, ErrUnreadable) || d.CreateKey {
				t.Fatalf("claim = %+v, %v; want ErrUnreadable", d, err)
			}
			if err := s.RecordPublicKey(ctx, region, ident(1, 1), pub); !errors.Is(err, ErrUnreadable) {
				t.Fatalf("record = %v, want ErrUnreadable", err)
			}
			if stored(t, kv) != raw {
				t.Fatal("an unreadable record was overwritten")
			}
		})
	}
}

// lossyKV applies or drops a write and then reports failure, as a write whose
// reply was lost or that never reached the server.
type lossyKV struct {
	jetstream.KeyValue

	failCreate, failUpdate int
	apply                  bool
	beforeCreate           func()
}

func (k *lossyKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if k.beforeCreate != nil {
		k.beforeCreate()
		k.beforeCreate = nil
	}
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

func TestAmbiguousWrites(t *testing.T) {
	ctx := context.Background()
	t.Run("applied claim with a lost reply is recovered", func(t *testing.T) {
		_, kv := newStore(t)
		s := Store{KV: &lossyKV{KeyValue: kv, failCreate: 1, apply: true}}
		d, err := s.Claim(ctx, region, ident(1, 1))
		if err != nil || !d.CreateKey {
			t.Fatalf("claim = %+v, %v", d, err)
		}
	})
	t.Run("dropped claim that a rival won is refused", func(t *testing.T) {
		_, kv := newStore(t)
		lossy := &lossyKV{KeyValue: kv, failCreate: 1}
		lossy.beforeCreate = func() { Store{KV: kv}.Claim(ctx, region, ident(2, 1)) }
		if _, err := (Store{KV: lossy}).Claim(ctx, region, ident(1, 1)); !errors.Is(err, ErrClaimHeld) {
			t.Fatalf("claim = %v, want ErrClaimHeld", err)
		}
	})
	t.Run("applied key record with a lost reply is confirmed on retry", func(t *testing.T) {
		_, kv := newStore(t)
		w := ident(1, 1)
		Store{KV: kv}.Claim(ctx, region, w)
		pub, _ := userPub(t)
		lossy := &lossyKV{KeyValue: kv, failUpdate: maxAttempts, apply: true}
		s := Store{KV: lossy}
		// The first update applies; the re-read then sees it already recorded.
		if err := s.RecordPublicKey(ctx, region, w, pub); err != nil {
			t.Fatalf("record = %v, want recovered success", err)
		}
		d, err := s.Claim(ctx, region, w)
		if err != nil || d.CreateKey || d.PublicKey != pub {
			t.Fatalf("claim after lost reply = %+v, %v", d, err)
		}
	})
	t.Run("persistently dropped writes authorize nothing", func(t *testing.T) {
		_, kv := newStore(t)
		s := Store{KV: &lossyKV{KeyValue: kv, failCreate: maxAttempts}}
		d, err := s.Claim(ctx, region, ident(1, 1))
		if !errors.Is(err, ErrContended) || d.CreateKey {
			t.Fatalf("claim = %+v, %v; want ErrContended", d, err)
		}
	})
}

func TestValidation(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	bad := []Identity{
		{InstanceID: "", ClaimID: "claim-0001", Generation: 1},
		{InstanceID: "i-00000000000000001", ClaimID: "", Generation: 1},
		{InstanceID: "i-00000000000000001", ClaimID: "claim-0001", Generation: 0},
		{InstanceID: "i-00000000000000001.x", ClaimID: "claim-0001", Generation: 1},
	}
	for _, id := range bad {
		if _, err := s.Claim(ctx, region, id); err == nil {
			t.Errorf("claim accepted %+v", id)
		}
	}
	if _, err := s.Claim(ctx, "Bad.Region", ident(1, 1)); err == nil {
		t.Error("claim accepted an invalid region")
	}
}
