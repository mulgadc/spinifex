// Package firststore is the SQS domain's durable decision on whether the shared
// SQS worker may create the store's client key. One record per Region moves
// claimed -> key-recorded -> launch-intent under compare-and-swap, behind a
// generation fence the launch path raises before it replaces a worker.
package firststore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// Bucket is the SQS domain's KV bucket for first-store records.
const Bucket = "spinifex-sqs-first-store"

// BucketConfig is the bucket's configuration for test fixtures; production
// opens it through Open, which takes the replica count from the cluster.
func BucketConfig(replicas int) jetstream.KeyValueConfig {
	return jetstream.KeyValueConfig{
		Bucket:   Bucket,
		History:  1,
		Storage:  jetstream.FileStorage,
		Replicas: replicas,
	}
}

type State string

const (
	// Only a fence is recorded: no worker has claimed since it was raised.
	StateOpen State = "open"
	// A worker identity holds the claim; no key is recorded, so no store can
	// have been launched with any key.
	StateClaimed State = "claimed"
	// The worker's public key is recorded; from here a missing or different
	// worker key fails closed.
	StateKeyRecorded State = "key-recorded"
	// A store launch may use the recorded key.
	StateLaunchIntent State = "launch-intent"
)

// Identity is one worker VM incarnation. Generation increases each time the
// worker is relaunched. A caller must derive it from authoritative instance
// state, never from the worker's request: a newer generation may take over.
type Identity struct {
	InstanceID string `json:"instanceID"`
	ClaimID    string `json:"claimID"`
	Generation uint64 `json:"generation"`
}

// Record is the stored decision. It never holds private key material.
type Record struct {
	Region    string   `json:"region"`
	State     State    `json:"state"`
	Worker    Identity `json:"worker"`
	PublicKey string   `json:"publicKey,omitempty"`
	// Fence is the lowest generation still accepted. It only rises, so a
	// request from a replaced worker that was already in flight is refused.
	Fence uint64 `json:"fence,omitempty"`
}

// Decision is what a claim authorizes: create a key, or use exactly the
// recorded one.
type Decision struct {
	CreateKey bool
	PublicKey string
}

var (
	ErrClaimHeld       = errors.New("first-store claim is held by another worker identity")
	ErrStaleGeneration = errors.New("worker generation is older than the recorded claim")
	ErrNotClaimed      = errors.New("no first-store claim is recorded")
	ErrKeyNotRecorded  = errors.New("worker public key is not recorded")
	ErrKeyMismatch     = errors.New("worker public key does not match the recorded key")
	ErrUnreadable      = errors.New("first-store record is unreadable")
	ErrContended       = errors.New("first-store record changed concurrently; retry")
)

const maxAttempts = 8

var (
	regionPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	instancePattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	claimPattern    = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
)

// Store is the first-store state machine over the domain's KV bucket. Reads
// go to the stream leader, and every authorizing answer is also confirmed by a
// compare-and-swap at the revision read, so a stale read authorizes nothing.
type Store struct {
	JS jetstream.JetStream
	KV jetstream.KeyValue
}

// Open opens or creates the bucket and refuses one that could lose records.
func Open(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.OpenDurableKV(ctx, js, Bucket, "SQS first-store decisions: who may create the store key per Region")
	if err != nil {
		return Store{}, err
	}
	return Store{JS: js, KV: kv}, nil
}

// Bind binds the existing bucket without creating it, refusing one that could
// lose records; a reader that must not provision state uses it.
func Bind(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.BindDurableKV(ctx, js, Bucket)
	if err != nil {
		return Store{}, err
	}
	return Store{JS: js, KV: kv}, nil
}

func key(region string) string { return "region." + region }

func validate(region string, id Identity) error {
	if !regionPattern.MatchString(region) {
		return errors.New("invalid region")
	}
	if !instancePattern.MatchString(id.InstanceID) {
		return errors.New("invalid worker instance ID")
	}
	if !claimPattern.MatchString(id.ClaimID) {
		return errors.New("invalid claim ID")
	}
	if id.Generation == 0 {
		return errors.New("worker generation must be positive")
	}
	return nil
}

// Claim decides for this worker identity. Absent a record it creates a claim
// and authorizes key creation. A claim with no recorded key may pass to a newer
// generation; once a key is recorded only that key is ever authorized.
func (s Store) Claim(ctx context.Context, region string, id Identity) (Decision, error) {
	if err := validate(region, id); err != nil {
		return Decision{}, err
	}
	for range maxAttempts {
		rec, rev, err := s.read(ctx, region)
		if errors.Is(err, ErrNotClaimed) {
			// A failed create may still have applied; the re-read decides.
			if s.create(ctx, region, Record{Region: region, State: StateClaimed, Worker: id}) == nil {
				return Decision{CreateKey: true}, nil
			}
			continue
		}
		if err != nil {
			return Decision{}, err
		}
		if id.Generation < rec.Fence {
			return Decision{}, ErrStaleGeneration
		}
		if rec.State == StateOpen {
			next := *rec
			next.State, next.Worker = StateClaimed, id
			if s.update(ctx, region, next, rev) == nil {
				return Decision{CreateKey: true}, nil
			}
			continue
		}
		if err := checkGeneration(rec.Worker, id); err != nil {
			return Decision{}, err
		}
		// Rebinds a strictly newer generation, keeping state and key; for the
		// same identity it rewrites the record to prove the read was current.
		next := *rec
		next.Worker = id
		if s.update(ctx, region, next, rev) != nil {
			continue
		}
		if rec.State == StateClaimed {
			return Decision{CreateKey: true}, nil
		}
		return Decision{PublicKey: rec.PublicKey}, nil
	}
	return Decision{}, ErrContended
}

// RecordPublicKey binds the worker's public key to its current claim. It is
// idempotent for the same key, so a lost reply can be retried.
func (s Store) RecordPublicKey(ctx context.Context, region string, id Identity, pub string) error {
	return s.advance(ctx, region, id, pub, func(rec *Record) (*Record, error) {
		switch rec.State {
		case StateClaimed:
			next := *rec
			next.State, next.PublicKey = StateKeyRecorded, pub
			return &next, nil
		default:
			return nil, nil
		}
	})
}

// RecordLaunchIntent permits a store launch with the recorded key. It refuses
// until the worker's public key is recorded.
func (s Store) RecordLaunchIntent(ctx context.Context, region string, id Identity, pub string) error {
	return s.advance(ctx, region, id, pub, func(rec *Record) (*Record, error) {
		switch rec.State {
		case StateClaimed:
			return nil, ErrKeyNotRecorded
		case StateKeyRecorded:
			next := *rec
			next.State = StateLaunchIntent
			return &next, nil
		default:
			return nil, nil
		}
	})
}

// advance applies step to the current record of the exact claiming identity.
// A nil next record means the transition has already happened.
func (s Store) advance(ctx context.Context, region string, id Identity, pub string, step func(*Record) (*Record, error)) error {
	if err := validate(region, id); err != nil {
		return err
	}
	if !nkeys.IsValidPublicUserKey(pub) {
		return errors.New("worker key is not a user public NKey")
	}
	for range maxAttempts {
		rec, rev, err := s.read(ctx, region)
		if err != nil {
			return err
		}
		if id.Generation < rec.Fence {
			return ErrStaleGeneration
		}
		if rec.State == StateOpen {
			return ErrNotClaimed
		}
		if rec.Worker != id {
			if err := checkGeneration(rec.Worker, id); err != nil {
				return err
			}
			// A newer generation must claim before it records.
			return ErrClaimHeld
		}
		if rec.State != StateClaimed && rec.PublicKey != pub {
			return ErrKeyMismatch
		}
		next, err := step(rec)
		if err != nil {
			return err
		}
		if next == nil {
			// Already applied: rewrite unchanged to prove the read was current.
			next = rec
		}
		if s.update(ctx, region, *next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// Fence raises the lowest accepted generation to gen and never lowers it. The
// launch path calls it before it replaces a worker, so a request from the old
// generation that is still in flight decides nothing once the fence is up.
func (s Store) Fence(ctx context.Context, region string, gen uint64) error {
	if !regionPattern.MatchString(region) || gen == 0 {
		return errors.New("invalid fence")
	}
	for range maxAttempts {
		rec, rev, err := s.read(ctx, region)
		if errors.Is(err, ErrNotClaimed) {
			if s.create(ctx, region, Record{Region: region, State: StateOpen, Fence: gen}) == nil {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		next := *rec
		next.Fence = max(rec.Fence, gen)
		// Unchanged when already raised: the rewrite proves the read was current.
		if s.update(ctx, region, next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// FencedAt is the Region's current fence, zero when none is recorded. The
// fence only rises, so a value read from the leader stays a lower bound.
func (s Store) FencedAt(ctx context.Context, region string) (uint64, error) {
	rec, _, err := s.read(ctx, region)
	if errors.Is(err, ErrNotClaimed) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return rec.Fence, nil
}

// checkGeneration refuses an older generation, and a different identity at
// the same generation; only a strictly newer generation may supersede.
func checkGeneration(held, id Identity) error {
	switch {
	case held == id:
		return nil
	case id.Generation < held.Generation:
		return ErrStaleGeneration
	case id.Generation == held.Generation:
		return ErrClaimHeld
	default:
		return nil
	}
}

func (s Store) read(ctx context.Context, region string) (*Record, uint64, error) {
	value, rev, err := sqsjs.LeaderKVGet(ctx, s.JS, Bucket, key(region))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, 0, ErrNotClaimed
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read first-store record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(value, &rec); err != nil {
		return nil, 0, ErrUnreadable
	}
	if !wellFormed(&rec, region) {
		return nil, 0, ErrUnreadable
	}
	return &rec, rev, nil
}

func wellFormed(rec *Record, region string) bool {
	if rec.Region != region {
		return false
	}
	if rec.State == StateOpen {
		return rec.Worker == Identity{} && rec.PublicKey == "" && rec.Fence > 0
	}
	if validate(region, rec.Worker) != nil {
		return false
	}
	switch rec.State {
	case StateClaimed:
		return rec.PublicKey == ""
	case StateKeyRecorded, StateLaunchIntent:
		return nkeys.IsValidPublicUserKey(rec.PublicKey)
	default:
		return false
	}
}

func (s Store) create(ctx context.Context, region string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.KV.Create(ctx, key(region), b)
	return err
}

func (s Store) update(ctx context.Context, region string, rec Record, rev uint64) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.KV.Update(ctx, key(region), b, rev)
	return err
}
