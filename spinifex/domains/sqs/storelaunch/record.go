// Package storelaunch realizes the Region's SQS store guest on the shared
// worker's node, with the worker's recorded public key. Its record carries a
// one-shot marker before every create or dispatch whose outcome can be lost,
// so an ambiguous step is never repeated; it waits to be reconciled.
package storelaunch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
)

// Bucket is the SQS domain's KV bucket for store launches.
const Bucket = "spinifex-sqs-store-launch"

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
	StateLaunching State = "launching"
	StateBound     State = "bound"
)

// Record is the Region's store launch. It holds the worker's public key only;
// no private key material is ever written here.
type Record struct {
	Region    string `json:"region"`
	State     State  `json:"state"`
	ClaimID   string `json:"claimID"`
	NodeID    string `json:"nodeID"`
	PublicKey string `json:"publicKey"`
	SizeGiB   int64  `json:"sizeGiB"`
	// The operator's instance type, fixed at Begin.
	InstanceType string `json:"instanceType"`
	// VolumeAttempted is set before the volume create is sent; DataVolumeID
	// records its result. A set marker without a volume is never retried.
	VolumeAttempted  bool   `json:"volumeAttempted,omitempty"`
	DataVolumeID     string `json:"dataVolumeID,omitempty"`
	NetworkAttempted bool   `json:"networkAttempted,omitempty"`
	Dispatched       bool   `json:"dispatched,omitempty"`
	InstanceID       string `json:"instanceID,omitempty"`
	Address          string `json:"address,omitempty"`
	// ServerKeyPin names the bound store's TLS server key. It is set once,
	// by the bound instance itself, and never changed.
	ServerKeyPin string `json:"serverKeyPin,omitempty"`
	// Contract is set only when this release creates the record. A missing
	// or unknown value is refused; it never proves when or how it was written.
	Contract int `json:"contract,omitempty"`
}

// ContractAuthenticated marks a launch made under authenticated bootstrap.
const ContractAuthenticated = 1

// UnsupportedContract reports a missing or unknown launch contract. Such a
// record is refused; its volume is kept and never treated as empty.
func (r Record) UnsupportedContract() bool { return r.Contract != ContractAuthenticated }

var (
	ErrUnreadable       = errors.New("SQS store launch record is unreadable")
	ErrContended        = errors.New("SQS store launch record changed concurrently; retry")
	ErrSuperseded       = errors.New("SQS store launch record names another claim")
	ErrConflict         = errors.New("SQS store launch record is not launching")
	ErrMarked           = errors.New("SQS store launch step was already sent; its outcome must be reconciled")
	ErrKeyChanged       = errors.New("SQS store launch record holds a different public key")
	ErrSizeChanged      = errors.New("SQS store launch record holds a different volume size")
	ErrTypeChanged      = errors.New("SQS store launch record holds a different instance type")
	ErrInvalidRecording = errors.New("invalid SQS store launch value")
	ErrNotBound         = errors.New("SQS store launch record is not yet bound to an instance")
	ErrNotStore         = errors.New("caller is not the Region's bound SQS store instance")
	ErrPinChanged       = errors.New("SQS store launch record holds a different server key pin")
)

const maxAttempts = 8

// MaxSizeGiB is the EBS gp3 volume size limit.
const MaxSizeGiB = 16384

var (
	regionPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	claimPattern    = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	nodePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	volumePattern   = regexp.MustCompile(`^vol-[0-9a-f]{17}$`)
	instancePattern = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)
	typePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
)

// Records is the store launch state machine. Reads go to the stream leader
// and every change is a compare-and-swap at the revision read.
type Records struct {
	JS jetstream.JetStream
	KV jetstream.KeyValue
}

// Open opens or creates the bucket and refuses one that could lose records.
func Open(ctx context.Context, js jetstream.JetStream) (Records, error) {
	kv, err := sqsjs.OpenDurableKV(ctx, js, Bucket, "SQS store launches: the store guest per Region")
	if err != nil {
		return Records{}, err
	}
	return Records{JS: js, KV: kv}, nil
}

// Bind binds the existing bucket without creating it, refusing one that could
// lose records; a reader that must not provision state uses it.
func Bind(ctx context.Context, js jetstream.JetStream) (Records, error) {
	kv, err := sqsjs.BindDurableKV(ctx, js, Bucket)
	if err != nil {
		return Records{}, err
	}
	return Records{JS: js, KV: kv}, nil
}

func key(region string) string { return "region." + region }

// Current is the Region's record, read from the stream leader.
func (r Records) Current(ctx context.Context, region string) (Record, error) {
	rec, _, err := r.read(ctx, region)
	if err != nil {
		return Record{}, err
	}
	return *rec, nil
}

// Begin creates the Region's launching record. A retry with the same claim,
// key and size returns nil; any other existing record is refused.
func (r Records) Begin(ctx context.Context, region, claimID, nodeID, publicKey, instanceType string, sizeGiB int64) error {
	next := Record{Region: region, State: StateLaunching, ClaimID: claimID, NodeID: nodeID, PublicKey: publicKey,
		SizeGiB: sizeGiB, InstanceType: instanceType, Contract: ContractAuthenticated}
	if !wellFormed(&next, region) {
		return ErrInvalidRecording
	}
	for range maxAttempts {
		rec, rev, err := r.read(ctx, region)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			if r.put(ctx, region, next, 0) == nil {
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		if rec.ClaimID != claimID || rec.NodeID != nodeID {
			return ErrSuperseded
		}
		if rec.PublicKey != publicKey {
			return ErrKeyChanged
		}
		if rec.SizeGiB != sizeGiB {
			return ErrSizeChanged
		}
		if rec.InstanceType != instanceType {
			return ErrTypeChanged
		}
		// Unchanged for a retry: the rewrite proves the read was current.
		if r.put(ctx, region, *rec, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// MarkVolumeAttempted records that the claim's one volume create is about to
// be sent. A second call gets ErrMarked.
func (r Records) MarkVolumeAttempted(ctx context.Context, region, claimID string) error {
	return r.advance(ctx, region, claimID, func(rec *Record) error {
		if rec.VolumeAttempted {
			return ErrMarked
		}
		rec.VolumeAttempted = true
		return nil
	})
}

// ClearVolumeAttempt withdraws the volume mark after a Prepare proven to have
// sent no create, so a retry may try again. It refuses once a volume is recorded.
func (r Records) ClearVolumeAttempt(ctx context.Context, region, claimID string) error {
	return r.advance(ctx, region, claimID, func(rec *Record) error {
		if !rec.VolumeAttempted || rec.DataVolumeID != "" {
			return ErrConflict
		}
		rec.VolumeAttempted = false
		return nil
	})
}

// RecordVolume records the volume the attempted create returned. It is
// idempotent for the same volume.
func (r Records) RecordVolume(ctx context.Context, region, claimID, volumeID string) error {
	if !volumePattern.MatchString(volumeID) {
		return ErrInvalidRecording
	}
	return r.advance(ctx, region, claimID, func(rec *Record) error {
		switch {
		case !rec.VolumeAttempted:
			return ErrConflict
		case rec.DataVolumeID == volumeID:
			return nil
		case rec.DataVolumeID != "":
			return ErrConflict
		}
		rec.DataVolumeID = volumeID
		return nil
	})
}

// MarkNetworkAttempted records that the claim's one ENI create is about to be
// sent. A second call gets ErrMarked.
func (r Records) MarkNetworkAttempted(ctx context.Context, region, claimID string) error {
	return r.advance(ctx, region, claimID, func(rec *Record) error {
		if rec.NetworkAttempted {
			return ErrMarked
		}
		rec.NetworkAttempted = true
		return nil
	})
}

// MarkDispatched records that the launch is about to be sent. It requires a
// recorded volume; a second call gets ErrMarked.
func (r Records) MarkDispatched(ctx context.Context, region, claimID string) error {
	return r.advance(ctx, region, claimID, func(rec *Record) error {
		switch {
		case rec.DataVolumeID == "":
			return ErrConflict
		case rec.Dispatched:
			return ErrMarked
		}
		rec.Dispatched = true
		return nil
	})
}

// Bind records the launched store instance and its address. It is idempotent
// for the same instance and address, so a lost reply can be retried.
func (r Records) Bind(ctx context.Context, region, claimID, instanceID, address string) error {
	if !instancePattern.MatchString(instanceID) || address == "" {
		return ErrInvalidRecording
	}
	for range maxAttempts {
		rec, rev, err := r.read(ctx, region)
		if err != nil {
			return err
		}
		if rec.ClaimID != claimID {
			return ErrSuperseded
		}
		switch rec.State {
		case StateBound:
			if rec.InstanceID != instanceID || rec.Address != address {
				return ErrConflict
			}
		case StateLaunching:
			if !rec.Dispatched {
				return ErrConflict
			}
			rec.State, rec.InstanceID, rec.Address = StateBound, instanceID, address
		}
		if r.put(ctx, region, *rec, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// RecordServerKey records the bound store's server key pin. Only the bound
// instance may record it, and only for the claim and volume it was told of,
// so a replaced store cannot pin its key on its successor's record. The same
// pin again succeeds, so a lost reply can be retried; a different pin is
// refused and the record is unchanged.
func (r Records) RecordServerKey(ctx context.Context, region, instanceID, claimID, volumeID, pin string) error {
	if !instancePattern.MatchString(instanceID) || !claimPattern.MatchString(claimID) ||
		!volumePattern.MatchString(volumeID) || !sqsv1.ValidPin(pin) {
		return ErrInvalidRecording
	}
	for range maxAttempts {
		rec, rev, err := r.boundTo(ctx, region, instanceID)
		if err != nil {
			return err
		}
		switch {
		case rec.ClaimID != claimID || rec.DataVolumeID != volumeID:
			return ErrNotStore
		case rec.ServerKeyPin == pin:
			return nil
		case rec.ServerKeyPin != "":
			return ErrPinChanged
		}
		rec.ServerKeyPin = pin
		if r.put(ctx, region, *rec, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

// ServerKeyOf is the Region's record as its bound instance may see it: the
// claim, volume and any recorded pin. It refuses every other caller.
func (r Records) ServerKeyOf(ctx context.Context, region, instanceID string) (Record, error) {
	rec, _, err := r.boundTo(ctx, region, instanceID)
	if err != nil {
		return Record{}, err
	}
	return *rec, nil
}

func (r Records) boundTo(ctx context.Context, region, instanceID string) (*Record, uint64, error) {
	rec, rev, err := r.read(ctx, region)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, ErrNotBound
	case err != nil:
		return nil, 0, err
	case rec.State != StateBound:
		return nil, 0, ErrNotBound
	case rec.InstanceID != instanceID:
		return nil, 0, ErrNotStore
	}
	return rec, rev, nil
}

// advance applies step to the claim's launching record under compare-and-swap.
func (r Records) advance(ctx context.Context, region, claimID string, step func(*Record) error) error {
	for range maxAttempts {
		rec, rev, err := r.read(ctx, region)
		if err != nil {
			return err
		}
		if rec.ClaimID != claimID {
			return ErrSuperseded
		}
		if rec.State != StateLaunching {
			return ErrConflict
		}
		if err := step(rec); err != nil {
			return err
		}
		if r.put(ctx, region, *rec, rev) == nil {
			return nil
		}
	}
	return ErrContended
}

func (r Records) read(ctx context.Context, region string) (*Record, uint64, error) {
	if !regionPattern.MatchString(region) {
		return nil, 0, ErrInvalidRecording
	}
	value, rev, err := sqsjs.LeaderKVGet(ctx, r.JS, Bucket, key(region))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("read SQS store launch record: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(value, &rec); err != nil || !wellFormed(&rec, region) {
		return nil, 0, ErrUnreadable
	}
	return &rec, rev, nil
}

func wellFormed(r *Record, region string) bool {
	if r.Region != region || !claimPattern.MatchString(r.ClaimID) || !nodePattern.MatchString(r.NodeID) ||
		!nkeys.IsValidPublicUserKey(r.PublicKey) || r.SizeGiB < 1 || r.SizeGiB > MaxSizeGiB ||
		!typePattern.MatchString(r.InstanceType) {
		return false
	}
	if r.DataVolumeID != "" && (!r.VolumeAttempted || !volumePattern.MatchString(r.DataVolumeID)) {
		return false
	}
	if r.Dispatched && r.DataVolumeID == "" {
		return false
	}
	switch r.State {
	case StateLaunching:
		return r.InstanceID == "" && r.Address == ""
	case StateBound:
		return r.Dispatched && instancePattern.MatchString(r.InstanceID) && r.Address != "" &&
			(r.ServerKeyPin == "" || sqsv1.ValidPin(r.ServerKeyPin))
	default:
		return false
	}
}

// put creates the key when rev is zero and is a revision compare-and-swap
// otherwise.
func (r Records) put(ctx context.Context, region string, rec Record, rev uint64) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if rev == 0 {
		_, err = r.KV.Create(ctx, key(region), b)
	} else {
		_, err = r.KV.Update(ctx, key(region), b, rev)
	}
	return err
}
