// Package workerident is the SQS domain's authority on which VM is the
// Region's shared SQS worker. A regional record moves launching -> bound ->
// stopped, or launching -> failed, under compare-and-swap; a per-instance
// index only finds the record.
package workerident

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket is the SQS domain's KV bucket for worker identity.
const Bucket = "spinifex-sqs-worker-identity"

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
	// A launch with this generation and claim ID is intended; no instance is
	// the worker yet.
	StateLaunching State = "launching"
	// The record names the one instance that is the worker.
	StateBound State = "bound"
	// The caller confirmed the launch created no instance holding the volume.
	StateFailed State = "failed"
	// The caller confirmed the bound instance stopped and released the volume.
	StateStopped State = "stopped"
)

// Record is the Region's current worker. Generation only grows; the first cut
// pins the worker to NodeID and to one encrypted data volume.
type Record struct {
	Region       string `json:"region"`
	State        State  `json:"state"`
	Generation   uint64 `json:"generation"`
	ClaimID      string `json:"claimID"`
	NodeID       string `json:"nodeID"`
	DataVolumeID string `json:"dataVolumeID"`
	InstanceType string `json:"instanceType"`
	InstanceID   string `json:"instanceID,omitempty"`
	// Dispatched is set once, before the launch is sent. A dispatched launch
	// is never sent again: its outcome is reconciled, not retried.
	Dispatched bool `json:"dispatched,omitempty"`
	// NetworkAttempted is set once, before the claim's ENI create is sent. A
	// create whose outcome was not seen is never sent again for that claim.
	NetworkAttempted bool `json:"networkAttempted,omitempty"`
}

// IndexEntry finds an instance's regional record. It is a lookup, never proof
// that the instance is current.
type IndexEntry struct {
	Region     string `json:"region"`
	Generation uint64 `json:"generation"`
	ClaimID    string `json:"claimID"`
}

// LaunchRequest is one launch attempt. ClaimID is freshly minted per attempt
// and reused only to retry it; only the current claim is recorded, so a VM
// launch must be idempotent on generation and claim together. Supersedes is the generation the caller
// read and is replacing, zero for the Region's first worker, so a replayed
// request cannot take a generation it never observed.
type LaunchRequest struct {
	ClaimID             string
	Supersedes          uint64
	NodeID              string
	DataVolumeID        string
	DataVolumeEncrypted bool
	// InstanceType is fixed for a generation; only a replacement may change it.
	InstanceType string
}

// Intent is a recorded launch the caller may realize.
type Intent struct {
	Region     string
	Generation uint64
	ClaimID    string
}

// Identity is an instance's authorized worker identity, taken from the
// current regional record rather than from anything the instance supplies.
type Identity struct {
	Region       string
	InstanceID   string
	Generation   uint64
	ClaimID      string
	NodeID       string
	DataVolumeID string
}

var (
	ErrUnknownInstance   = errors.New("instance is not an SQS worker")
	ErrNotCurrent        = errors.New("instance is not the Region's current SQS worker")
	ErrSuperseded        = errors.New("launch intent has been superseded")
	ErrInFlight          = errors.New("another SQS worker launch is in flight or bound")
	ErrConflict          = errors.New("worker identity conflicts with the recorded state")
	ErrWrongNode         = errors.New("SQS worker is pinned to another node")
	ErrVolumeMismatch    = errors.New("SQS worker is pinned to another data volume")
	ErrTypeChanged       = errors.New("SQS worker launch holds a different instance type")
	ErrUnencryptedVolume = errors.New("SQS worker data volume must be encrypted")
	ErrUnreadable        = errors.New("worker identity record is unreadable")
	ErrContended         = errors.New("worker identity changed concurrently; retry")
	ErrNotFenced         = errors.New("first-store fence does not yet exclude the replaced generation")
	ErrDispatched        = errors.New("SQS worker launch was already dispatched; its outcome must be reconciled")
	ErrNetworkAttempted  = errors.New("SQS worker network create was already sent; its outcome must be reconciled")
)

const maxAttempts = 8

var (
	regionPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	instancePattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	claimPattern    = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	volumePattern   = regexp.MustCompile(`^vol-[0-9a-f]{17}$`)
	nodePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	typePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
)

// Store is the worker identity authority. Reads go to the stream leader and
// every decision that grants something is a compare-and-swap at the revision
// read, so a stale or ambiguous result grants nothing.
type Store struct {
	JS jetstream.JetStream
	KV jetstream.KeyValue
	// Fences reads the first-store fence a replacement launch requires. A nil
	// reader refuses every replacement.
	Fences FenceReader
}

// FenceReader reports the lowest worker generation the first-store decision
// still accepts in a Region.
type FenceReader interface {
	FencedAt(ctx context.Context, region string) (uint64, error)
}

var _ FenceReader = firststore.Store{}

// Open opens or creates the bucket and refuses one that could lose records.
func Open(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.OpenDurableKV(ctx, js, Bucket, "SQS worker identity: the current worker per Region and its instance index")
	if err != nil {
		return Store{}, err
	}
	fences, err := firststore.Open(ctx, js)
	if err != nil {
		return Store{}, err
	}
	return Store{JS: js, KV: kv, Fences: fences}, nil
}

// Bind binds the existing bucket without creating it, refusing one that could
// lose records; a reader that must not provision state uses it. It reads no
// fence, so it refuses every replacement launch.
func Bind(ctx context.Context, js jetstream.JetStream) (Store, error) {
	kv, err := sqsjs.BindDurableKV(ctx, js, Bucket)
	if err != nil {
		return Store{}, err
	}
	return Store{JS: js, KV: kv}, nil
}

func regionKey(region string) string { return "region." + region }

func indexKey(instanceID string) string { return "instance." + instanceID }

// BeginLaunch records a launch intent. A retry of the same attempt returns the
// same intent. A new attempt is refused while the previous one is launching or
// bound, including one whose outcome is unknown; it takes the next generation
// only after FailLaunch or RecordStopped confirmed nothing holds the volume,
// and only once the first-store fence admits no older generation.
func (s Store) BeginLaunch(ctx context.Context, region string, req LaunchRequest) (Intent, error) {
	switch {
	case !regionPattern.MatchString(region):
		return Intent{}, errors.New("invalid region")
	case !claimPattern.MatchString(req.ClaimID):
		return Intent{}, errors.New("invalid claim ID")
	case !nodePattern.MatchString(req.NodeID):
		return Intent{}, errors.New("invalid node ID")
	case !volumePattern.MatchString(req.DataVolumeID):
		return Intent{}, errors.New("invalid data volume ID")
	case !req.DataVolumeEncrypted:
		return Intent{}, ErrUnencryptedVolume
	case !typePattern.MatchString(req.InstanceType):
		return Intent{}, errors.New("invalid instance type")
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			if req.Supersedes != 0 {
				return Intent{}, ErrConflict
			}
			next := Record{Region: region, State: StateLaunching, Generation: 1,
				ClaimID: req.ClaimID, NodeID: req.NodeID, DataVolumeID: req.DataVolumeID, InstanceType: req.InstanceType}
			// A failed create may still have applied; the re-read decides.
			if s.put(ctx, regionKey(region), next, 0) == nil {
				return intentOf(next), nil
			}
			continue
		}
		if err != nil {
			return Intent{}, err
		}
		switch {
		case rec.NodeID != req.NodeID:
			return Intent{}, ErrWrongNode
		case rec.DataVolumeID != req.DataVolumeID:
			return Intent{}, ErrVolumeMismatch
		}
		next := *rec
		switch {
		case rec.ClaimID == req.ClaimID:
			if rec.State != StateLaunching || rec.Generation != req.Supersedes+1 {
				return Intent{}, ErrSuperseded
			}
			if rec.InstanceType != req.InstanceType {
				return Intent{}, ErrTypeChanged
			}
		case req.Supersedes != rec.Generation:
			return Intent{}, ErrSuperseded
		case rec.State == StateLaunching || rec.State == StateBound:
			return Intent{}, ErrInFlight
		default:
			if err := s.requireFence(ctx, region, rec.Generation+1); err != nil {
				return Intent{}, err
			}
			next.State, next.Generation, next.ClaimID, next.InstanceID, next.Dispatched, next.NetworkAttempted =
				StateLaunching, rec.Generation+1, req.ClaimID, "", false, false
			next.InstanceType = req.InstanceType
		}
		// Unchanged for a retry: the rewrite proves the read was current.
		if s.put(ctx, regionKey(region), next, rev) == nil {
			return intentOf(next), nil
		}
	}
	return Intent{}, ErrContended
}

// requireFence refuses a replacement until the first-store fence is at least
// gen. The fence never falls, so one leader read that satisfies it stays true.
func (s Store) requireFence(ctx context.Context, region string, gen uint64) error {
	if s.Fences == nil {
		return ErrNotFenced
	}
	fence, err := s.Fences.FencedAt(ctx, region)
	if err != nil {
		return err
	}
	if fence < gen {
		return ErrNotFenced
	}
	return nil
}

// BindInstance makes instanceID the worker for intent. The index entry is
// written first, so the record never names an instance the index cannot
// find; a lost reply is retried with the same arguments.
func (s Store) BindInstance(ctx context.Context, intent Intent, instanceID string) error {
	if err := validIntent(intent); err != nil {
		return err
	}
	if !instancePattern.MatchString(instanceID) {
		return errors.New("invalid instance ID")
	}
	entry := IndexEntry(intent)
	if err := s.ensureIndex(ctx, instanceID, entry); err != nil {
		return err
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, intent.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrSuperseded
		}
		if err != nil {
			return err
		}
		if rec.Generation != intent.Generation || rec.ClaimID != intent.ClaimID {
			return ErrSuperseded
		}
		next := *rec
		switch rec.State {
		case StateLaunching:
			next.State, next.InstanceID = StateBound, instanceID
		case StateBound:
			if rec.InstanceID != instanceID {
				return ErrConflict
			}
		default:
			return ErrSuperseded
		}
		if s.put(ctx, regionKey(intent.Region), next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// FailLaunch records that intent's launch created no instance holding the data
// volume. Call it only once that is confirmed; a launch with an unknown outcome
// must stay launching. It refuses once bound or superseded, and is idempotent.
func (s Store) FailLaunch(ctx context.Context, intent Intent) error {
	if err := validIntent(intent); err != nil {
		return err
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, intent.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrSuperseded
		}
		if err != nil {
			return err
		}
		if rec.Generation != intent.Generation || rec.ClaimID != intent.ClaimID {
			return ErrSuperseded
		}
		next := *rec
		switch rec.State {
		case StateLaunching:
			next.State = StateFailed
		case StateFailed:
		default:
			return ErrConflict
		}
		if s.put(ctx, regionKey(intent.Region), next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// MarkDispatched records that intent's launch is about to be sent. Exactly one
// call wins; a second call, or a retry after a lost reply, gets ErrDispatched,
// so an ambiguous dispatch is never repeated.
func (s Store) MarkDispatched(ctx context.Context, intent Intent) error {
	if err := validIntent(intent); err != nil {
		return err
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, intent.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrSuperseded
		}
		if err != nil {
			return err
		}
		switch {
		case rec.Generation != intent.Generation || rec.ClaimID != intent.ClaimID:
			return ErrSuperseded
		case rec.State != StateLaunching:
			return ErrConflict
		case rec.Dispatched:
			return ErrDispatched
		}
		next := *rec
		next.Dispatched = true
		if s.put(ctx, regionKey(intent.Region), next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// MarkNetworkAttempted records that intent's ENI create is about to be sent.
// Exactly one call wins; a second call, or a retry after a lost reply, gets
// ErrNetworkAttempted, so an uncertain create is never repeated.
func (s Store) MarkNetworkAttempted(ctx context.Context, intent Intent) error {
	if err := validIntent(intent); err != nil {
		return err
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, intent.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrSuperseded
		}
		if err != nil {
			return err
		}
		switch {
		case rec.Generation != intent.Generation || rec.ClaimID != intent.ClaimID:
			return ErrSuperseded
		case rec.State != StateLaunching:
			return ErrConflict
		case rec.NetworkAttempted:
			return ErrNetworkAttempted
		}
		next := *rec
		next.NetworkAttempted = true
		if s.put(ctx, regionKey(intent.Region), next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// Current is the Region's record, read from the stream leader. It decides
// nothing by itself; every transition re-reads it under compare-and-swap.
func (s Store) Current(ctx context.Context, region string) (Record, error) {
	if !regionPattern.MatchString(region) {
		return Record{}, errors.New("invalid region")
	}
	rec, _, err := s.readRecord(ctx, region)
	if err != nil {
		return Record{}, err
	}
	return *rec, nil
}

// RecordStopped records that intent's bound instance is confirmed stopped and
// its data volume detached and no longer exported, which is the only way a
// bound worker can be replaced. It is idempotent.
func (s Store) RecordStopped(ctx context.Context, intent Intent, instanceID string) error {
	if err := validIntent(intent); err != nil {
		return err
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, intent.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrSuperseded
		}
		if err != nil {
			return err
		}
		if rec.Generation != intent.Generation || rec.ClaimID != intent.ClaimID {
			return ErrSuperseded
		}
		if rec.InstanceID != instanceID {
			return ErrConflict
		}
		next := *rec
		switch rec.State {
		case StateBound:
			next.State = StateStopped
		case StateStopped:
		default:
			return ErrConflict
		}
		if s.put(ctx, regionKey(intent.Region), next, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// Authorize returns instanceID's worker identity only if the Region's current
// record is bound to exactly that instance, generation and claim. A missing,
// stale or partially written index entry authorizes nothing.
func (s Store) Authorize(ctx context.Context, instanceID string) (Identity, error) {
	if !instancePattern.MatchString(instanceID) {
		return Identity{}, ErrUnknownInstance
	}
	var entry IndexEntry
	if _, err := s.read(ctx, indexKey(instanceID), &entry); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Identity{}, ErrUnknownInstance
		}
		return Identity{}, err
	}
	if !regionPattern.MatchString(entry.Region) || entry.Generation == 0 || !claimPattern.MatchString(entry.ClaimID) {
		return Identity{}, ErrUnreadable
	}
	for range maxAttempts {
		rec, rev, err := s.readRecord(ctx, entry.Region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Identity{}, ErrNotCurrent
		}
		if err != nil {
			return Identity{}, err
		}
		if rec.State != StateBound || rec.InstanceID != instanceID ||
			rec.Generation != entry.Generation || rec.ClaimID != entry.ClaimID {
			return Identity{}, ErrNotCurrent
		}
		// The unchanged rewrite proves the read was current, so a lagging
		// replica cannot keep a replaced worker authorized.
		if s.put(ctx, regionKey(entry.Region), *rec, rev) == nil {
			return Identity{Region: rec.Region, InstanceID: instanceID, Generation: rec.Generation,
				ClaimID: rec.ClaimID, NodeID: rec.NodeID, DataVolumeID: rec.DataVolumeID}, nil
		}
	}
	return Identity{}, ErrContended
}

// ensureIndex creates the entry once; instance IDs are never reused, so an
// existing different entry is a conflict rather than something to overwrite.
func (s Store) ensureIndex(ctx context.Context, instanceID string, entry IndexEntry) error {
	for range maxAttempts {
		var have IndexEntry
		_, err := s.read(ctx, indexKey(instanceID), &have)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// A failed create may still have applied; the re-read decides.
			if s.put(ctx, indexKey(instanceID), entry, 0) == nil {
				return nil
			}
			continue
		case err != nil:
			return err
		case have != entry:
			return ErrConflict
		}
		return nil
	}
	return ErrContended
}

func validIntent(in Intent) error {
	if !regionPattern.MatchString(in.Region) || in.Generation == 0 || !claimPattern.MatchString(in.ClaimID) {
		return errors.New("invalid launch intent")
	}
	return nil
}

func intentOf(r Record) Intent {
	return Intent{Region: r.Region, Generation: r.Generation, ClaimID: r.ClaimID}
}

func (s Store) readRecord(ctx context.Context, region string) (*Record, uint64, error) {
	var rec Record
	rev, err := s.read(ctx, regionKey(region), &rec)
	if err != nil {
		return nil, 0, err
	}
	if !wellFormed(&rec, region) {
		return nil, 0, ErrUnreadable
	}
	return &rec, rev, nil
}

func wellFormed(r *Record, region string) bool {
	if r.Region != region || r.Generation == 0 || !claimPattern.MatchString(r.ClaimID) ||
		!nodePattern.MatchString(r.NodeID) || !volumePattern.MatchString(r.DataVolumeID) ||
		!typePattern.MatchString(r.InstanceType) {
		return false
	}
	switch r.State {
	case StateBound, StateStopped:
		return instancePattern.MatchString(r.InstanceID)
	case StateLaunching, StateFailed:
		return r.InstanceID == ""
	default:
		return false
	}
}

func (s Store) read(ctx context.Context, key string, into any) (uint64, error) {
	value, rev, err := sqsjs.LeaderKVGet(ctx, s.JS, Bucket, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return 0, err
		}
		return 0, fmt.Errorf("read worker identity %s: %w", key, err)
	}
	if err := json.Unmarshal(value, into); err != nil {
		return 0, ErrUnreadable
	}
	return rev, nil
}

// put creates the key when rev is zero and is a revision compare-and-swap
// otherwise.
func (s Store) put(ctx context.Context, key string, v any, rev uint64) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if rev == 0 {
		_, err = s.KV.Create(ctx, key, b)
	} else {
		_, err = s.KV.Update(ctx, key, b, rev)
	}
	return err
}
