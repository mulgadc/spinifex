package workerkey

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

const region = "ap-southeast-2"

// domainClaimer drives the real first-store state machine for one worker
// identity. A failing record either drops the write or applies it and loses
// the reply.
type domainClaimer struct {
	s          firststore.Store
	id         firststore.Identity
	failRecord bool
	applied    bool
}

func (c *domainClaimer) Claim(ctx context.Context) (bool, string, error) {
	d, err := c.s.Claim(ctx, region, c.id)
	return d.CreateKey, d.PublicKey, err
}

func (c *domainClaimer) RecordPublicKey(ctx context.Context, pub string) error {
	if c.failRecord {
		c.failRecord = false
		if c.applied {
			c.s.RecordPublicKey(ctx, region, c.id, pub)
		}
		return errors.New("nats: timeout")
	}
	return c.s.RecordPublicKey(ctx, region, c.id, pub)
}

func domain(t *testing.T) (firststore.Store, jetstream.KeyValue) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(context.Background(), firststore.BucketConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	return firststore.Store{JS: js, KV: kv}, kv
}

func worker(gen uint64, n byte) firststore.Identity {
	return firststore.Identity{InstanceID: "i-0000000000000000" + string('0'+n), ClaimID: "claim-" + string('a'+n) + "-0001", Generation: gen}
}

func workerDisk(t *testing.T) Store {
	return Store{Dir: filepath.Join(t.TempDir(), "sqs-worker-key")}
}

func keyPath(s Store) string { return filepath.Join(s.Dir, keyFile) }

func pubOf(t *testing.T, kp nkeys.KeyPair) string {
	t.Helper()
	p, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func recorded(t *testing.T, s firststore.Store, id firststore.Identity) string {
	t.Helper()
	d, err := s.Claim(context.Background(), region, id)
	if err != nil || d.CreateKey {
		t.Fatalf("domain decision = %+v, %v; want a recorded key", d, err)
	}
	return d.PublicKey
}

func TestAcquire_CrashBeforeRecordingReusesTheKeyOnDisk(t *testing.T) {
	ctx := context.Background()
	fs, _ := domain(t)
	disk := workerDisk(t)
	c := &domainClaimer{s: fs, id: worker(1, 1), failRecord: true}
	if _, err := disk.Acquire(ctx, c); err == nil {
		t.Fatal("a key was returned before its public key was recorded")
	}
	before, err := os.ReadFile(keyPath(disk))
	if err != nil {
		t.Fatalf("key not synced to disk before recording: %v", err)
	}
	kp, err := disk.Acquire(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Wipe()
	after, _ := os.ReadFile(keyPath(disk))
	if string(before) != string(after) {
		t.Fatal("the retry replaced the key")
	}
	if recorded(t, fs, c.id) != pubOf(t, kp) {
		t.Fatal("the recorded key is not the key on disk")
	}
}

func TestAcquire_LostRecordReplyIsRecoveredOnReboot(t *testing.T) {
	ctx := context.Background()
	fs, _ := domain(t)
	disk := workerDisk(t)
	c := &domainClaimer{s: fs, id: worker(1, 1), failRecord: true, applied: true}
	if _, err := disk.Acquire(ctx, c); err == nil {
		t.Fatal("expected the lost reply to surface")
	}
	kp, err := disk.Acquire(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Wipe()
	if recorded(t, fs, c.id) != pubOf(t, kp) {
		t.Fatal("reboot did not use the recorded key")
	}
}

func TestAcquire_AfterRecordingKeyLossOrMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, launch bool) (firststore.Store, Store, *domainClaimer, string) {
		fs, _ := domain(t)
		disk := workerDisk(t)
		c := &domainClaimer{s: fs, id: worker(1, 1)}
		kp, err := disk.Acquire(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		pub := pubOf(t, kp)
		kp.Wipe()
		if launch {
			if err := fs.RecordLaunchIntent(ctx, region, c.id, pub); err != nil {
				t.Fatal(err)
			}
		}
		return fs, disk, c, pub
	}
	for _, launch := range []bool{false, true} {
		name := map[bool]string{false: "key recorded", true: "launch intent"}[launch]
		t.Run(name+"/key lost then reboot", func(t *testing.T) {
			fs, disk, c, pub := setup(t, launch)
			os.Remove(keyPath(disk))
			// A relaunched worker generation gets the same answer.
			for _, id := range []firststore.Identity{c.id, worker(2, 2)} {
				c.id = id
				if _, err := disk.Acquire(ctx, c); !errors.Is(err, ErrKeyMissing) {
					t.Fatalf("acquire = %v, want ErrKeyMissing", err)
				}
				if _, err := os.Lstat(keyPath(disk)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("a replacement key was created")
				}
			}
			if recorded(t, fs, c.id) != pub {
				t.Fatal("the recorded key changed")
			}
		})
		t.Run(name+"/different key on disk", func(t *testing.T) {
			_, _, c, _ := setup(t, launch)
			other := workerDisk(t)
			kp, err := other.ForNewStore()
			if err != nil {
				t.Fatal(err)
			}
			kp.Wipe()
			before, _ := os.ReadFile(keyPath(other))
			if _, err := other.Acquire(ctx, c); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("acquire = %v, want ErrKeyMismatch", err)
			}
			after, _ := os.ReadFile(keyPath(other))
			if string(before) != string(after) {
				t.Fatal("the mismatched key was replaced")
			}
		})
	}
}

func TestAcquire_RefusedClaimCreatesNoKey(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(t *testing.T, fs firststore.Store, kv jetstream.KeyValue) firststore.Identity{
		"rival holds the claim": func(t *testing.T, fs firststore.Store, _ jetstream.KeyValue) firststore.Identity {
			fs.Claim(ctx, region, worker(1, 1))
			return worker(1, 2)
		},
		"stale replayed generation": func(t *testing.T, fs firststore.Store, _ jetstream.KeyValue) firststore.Identity {
			fs.Claim(ctx, region, worker(1, 1))
			fs.Claim(ctx, region, worker(2, 2))
			return worker(1, 1)
		},
		"unreadable record": func(t *testing.T, _ firststore.Store, kv jetstream.KeyValue) firststore.Identity {
			kv.Put(ctx, "region."+region, []byte("{"))
			return worker(1, 1)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fs, kv := domain(t)
			id := setup(t, fs, kv)
			disk := workerDisk(t)
			if _, err := disk.Acquire(ctx, &domainClaimer{s: fs, id: id}); err == nil {
				t.Fatal("a refused claim returned a key")
			}
			if _, err := os.Lstat(keyPath(disk)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused claim created a key")
			}
		})
	}
}

type contradictoryClaimer struct{ pub string }

func (c contradictoryClaimer) Claim(context.Context) (bool, string, error) { return true, c.pub, nil }
func (contradictoryClaimer) RecordPublicKey(context.Context, string) error {
	return errors.New("unexpected")
}

func TestAcquire_ContradictoryDecisionCreatesNoKey(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	disk := workerDisk(t)
	if _, err := disk.Acquire(context.Background(), contradictoryClaimer{pub: pubOf(t, kp)}); err == nil {
		t.Fatal("a contradictory decision returned a key")
	}
	if _, err := os.Lstat(keyPath(disk)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a contradictory decision created a key")
	}
}

// A replica that has applied the claim but not the key record must not lead a
// worker whose key is lost to create a replacement.
func TestAcquire_StaleClaimedReadAfterRecordingCreatesNoKey(t *testing.T) {
	ctx := context.Background()
	fs, kv := domain(t)
	disk := workerDisk(t)
	id := worker(1, 1)
	fs.Claim(ctx, region, id)
	claimed, err := kv.Get(ctx, "region."+region)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := disk.Acquire(ctx, &domainClaimer{s: fs, id: id})
	if err != nil {
		t.Fatal(err)
	}
	kp.Wipe()
	os.Remove(keyPath(disk))

	_, _, replicaJS := testutil.StartTestJetStream(t)
	replica, err := replicaJS.CreateKeyValue(ctx, firststore.BucketConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	replica.Put(ctx, "region."+region, claimed.Value())
	stale := firststore.Store{JS: replicaJS, KV: fs.KV}
	if _, err := disk.Acquire(ctx, &domainClaimer{s: stale, id: id}); err == nil {
		t.Fatal("a stale claimed read returned a key")
	}
	if _, err := os.Lstat(keyPath(disk)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stale claimed read created a replacement key")
	}
}
