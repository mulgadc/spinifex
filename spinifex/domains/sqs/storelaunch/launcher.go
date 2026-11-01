package storelaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/nats-io/nats.go/jetstream"
)

// Volumes prepares the store's data volume and reads its own metadata.
type Volumes interface {
	// Prepare creates a new, empty, formatted volume and returns its ID. On
	// failure it returns any volume it created that it could not discard.
	Prepare(ctx context.Context, sizeGiB int64) (string, error)
	// Encrypted reports the volume record's own encryption flag.
	Encrypted(ctx context.Context, volumeID string) (bool, error)
}

// NetworkAllocator manages the store's one ENI in the worker's subnet. Find
// only looks and returns nil when none is visible.
type NetworkAllocator interface {
	FindStoreNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error)
	CreateStoreNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error)
}

// Spec is one store VM launch. ClientNKey is the worker's recorded public
// key; nothing here is private.
type Spec struct {
	InstanceType string
	Region       string
	ProfileARN   string
	Network      workerlaunch.Network
	DataVolumeID string
	ClientNKey   string
}

// Roles ensures the store's instance role and profile, granted only the
// store's own internal actions, and returns the profile ARN.
type Roles interface {
	EnsureStoreProfile(ctx context.Context) (string, error)
}

// Host reports whether this node can boot the store now. It allocates and
// records nothing.
type Host interface {
	Ready(ctx context.Context) error
}

// VMLauncher starts the store VM on this node. Any error leaves the outcome
// unknown to the caller.
type VMLauncher interface {
	LaunchStore(ctx context.Context, spec Spec) (string, error)
}

var (
	ErrIncomplete     = errors.New("SQS store launcher is missing a capability")
	ErrHostNotReady   = errors.New("this node cannot boot the SQS store yet")
	ErrWorkerNotBound = errors.New("the Region's SQS worker is not bound on this node")
	ErrKeyNotRecorded = errors.New("the SQS worker has not recorded its public key")
	ErrWrongNode      = errors.New("the Region's SQS store is pinned to another node")
	ErrUnencrypted    = errors.New("SQS store data volume is not encrypted")
	// ErrVolumeNotCreated is a volume Prepare that sent no create; a retry may
	// try again once its cause is fixed.
	ErrVolumeNotCreated = errors.New("SQS store volume was not created")
	// ErrOutcomeUnknown is returned for a dispatched launch whose result was
	// not seen; it is never sent again until reconciled.
	ErrOutcomeUnknown = errors.New("SQS store launch outcome is unknown; reconcile before relaunching")
	// ErrVolumeOutcomeUnknown is returned for a volume create that was sent
	// and not recorded; it is never sent again until reconciled.
	ErrVolumeOutcomeUnknown = errors.New("SQS store volume create outcome is unknown; reconcile before relaunching")
	// ErrNetworkOutcomeUnknown is returned for an ENI create that was sent
	// with no ENI visible; it is never sent again until reconciled.
	ErrNetworkOutcomeUnknown = errors.New("SQS store network create outcome is unknown; reconcile before relaunching")
)

// VolumeError reports a store volume this call knows of but could not use:
// one a failed prepare left behind, or one that is not encrypted. It is kept.
type VolumeError struct {
	VolumeID string
	Err      error
}

func (e *VolumeError) Error() string {
	return fmt.Sprintf("SQS store data volume %s is kept: %v", e.VolumeID, e.Err)
}

func (e *VolumeError) Unwrap() error { return e.Err }

// Launcher realizes the store on NodeID, the node the Region's worker is
// bound to. The daemon never holds the worker's private key.
type Launcher struct {
	Records    Records
	Workers    workerident.Store
	FirstStore firststore.Store
	Volumes    Volumes
	Network    NetworkAllocator
	Roles      Roles
	Host       Host
	VMs        VMLauncher
	NodeID     string

	// RetainedVolume extracts a volume ID from a Prepare error, if any.
	RetainedVolume func(error) string
	newClaimID     func() (string, error)
}

// Result is the Region's bound store. Bound is not ready.
type Result struct {
	InstanceID   string
	Address      string
	InstanceType string
}

// Ensure returns the Region's bound store or launches it. Every step whose
// outcome can be lost is marked first, so a retry after a lost reply resumes
// or fails closed and never creates a second volume, ENI or VM.
func (l *Launcher) Ensure(ctx context.Context, region, instanceType string, sizeGiB int64) (Result, error) {
	if l.Records.KV == nil || l.Workers.KV == nil || l.FirstStore.KV == nil || l.Volumes == nil ||
		l.Network == nil || l.Roles == nil || l.Host == nil || l.VMs == nil || l.NodeID == "" || instanceType == "" ||
		sizeGiB < 1 || sizeGiB > MaxSizeGiB {
		return Result{}, ErrIncomplete
	}
	rec, err := l.Records.Current(ctx, region)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
	case err != nil:
		return Result{}, err
	case rec.NodeID != l.NodeID:
		return Result{}, ErrWrongNode
	case rec.InstanceType != instanceType:
		return Result{}, fmt.Errorf("%w: %s", ErrTypeChanged, rec.InstanceType)
	case rec.SizeGiB != sizeGiB:
		return Result{}, fmt.Errorf("%w: %d GiB", ErrSizeChanged, rec.SizeGiB)
	case rec.State == StateBound:
		return Result{InstanceID: rec.InstanceID, Address: rec.Address, InstanceType: rec.InstanceType}, nil
	case rec.Dispatched:
		return Result{}, ErrOutcomeUnknown
	}
	if err := l.Host.Ready(ctx); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrHostNotReady, err)
	}
	pub, err := l.recordedKey(ctx, region)
	if err != nil {
		return Result{}, err
	}
	claim := rec.ClaimID
	if claim == "" {
		if claim, err = l.claimID(); err != nil {
			return Result{}, err
		}
	}
	if err := l.Records.Begin(ctx, region, claim, l.NodeID, pub, instanceType, sizeGiB); err != nil {
		return Result{}, err
	}
	if rec, err = l.Records.Current(ctx, region); err != nil {
		return Result{}, err
	}

	volumeID, err := l.volume(ctx, region, rec)
	if err != nil {
		return Result{}, err
	}
	network, err := l.network(ctx, region, claim)
	if err != nil {
		return Result{}, err
	}
	profile, err := l.Roles.EnsureStoreProfile(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("sqs store: instance profile: %w", err)
	}
	if err := l.Records.MarkDispatched(ctx, region, claim); err != nil {
		if errors.Is(err, ErrMarked) {
			return Result{}, ErrOutcomeUnknown
		}
		return Result{}, err
	}
	instanceID, err := l.VMs.LaunchStore(ctx, Spec{InstanceType: instanceType, Region: region,
		ProfileARN: profile, Network: *network, DataVolumeID: volumeID, ClientNKey: pub})
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	if err := l.bind(ctx, region, claim, instanceID, network.ENIIP); err != nil {
		return Result{}, fmt.Errorf("store %s launched but not bound: %w", instanceID, err)
	}
	return Result{InstanceID: instanceID, Address: network.ENIIP, InstanceType: instanceType}, nil
}

// recordedKey requires the worker bound on this node and returns the public
// key it recorded, after recording the launch intent that confirms it.
func (l *Launcher) recordedKey(ctx context.Context, region string) (string, error) {
	w, err := l.Workers.Current(ctx, region)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", ErrWorkerNotBound
	}
	if err != nil {
		return "", err
	}
	if w.State != workerident.StateBound || w.NodeID != l.NodeID {
		return "", ErrWorkerNotBound
	}
	id := firststore.Identity{InstanceID: w.InstanceID, ClaimID: w.ClaimID, Generation: w.Generation}
	holder, pub, err := l.FirstStore.RecordedKey(ctx, region)
	if errors.Is(err, firststore.ErrNotClaimed) || errors.Is(err, firststore.ErrKeyNotRecorded) {
		return "", ErrKeyNotRecorded
	}
	if err != nil {
		return "", err
	}
	if holder != id {
		return "", fmt.Errorf("%w: the key was recorded by another worker identity", ErrKeyNotRecorded)
	}
	if err := l.FirstStore.RecordLaunchIntent(ctx, region, id, pub); err != nil {
		return "", fmt.Errorf("record store launch intent: %w", err)
	}
	return pub, nil
}

// volume returns the recorded volume, or marks the record and prepares one.
// Its encryption is read from the volume's own record before any use.
func (l *Launcher) volume(ctx context.Context, region string, rec Record) (string, error) {
	volumeID := rec.DataVolumeID
	if volumeID == "" {
		if err := l.Records.MarkVolumeAttempted(ctx, region, rec.ClaimID); err != nil {
			if errors.Is(err, ErrMarked) {
				return "", ErrVolumeOutcomeUnknown
			}
			return "", err
		}
		id, err := l.Volumes.Prepare(ctx, rec.SizeGiB)
		if errors.Is(err, storevolume.ErrNotCreated) {
			if cerr := l.Records.ClearVolumeAttempt(ctx, region, rec.ClaimID); cerr != nil {
				return "", fmt.Errorf("%w: no volume was created, but its mark was not cleared: %w", ErrVolumeOutcomeUnknown, cerr)
			}
			return "", fmt.Errorf("%w: %w", ErrVolumeNotCreated, err)
		}
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrVolumeOutcomeUnknown, err)
			if l.RetainedVolume != nil {
				if kept := l.RetainedVolume(err); kept != "" {
					return "", &VolumeError{VolumeID: kept, Err: err}
				}
			}
			return "", err
		}
		if err := l.Records.RecordVolume(ctx, region, rec.ClaimID, id); err != nil {
			return "", &VolumeError{VolumeID: id, Err: fmt.Errorf("%w: %w", ErrVolumeOutcomeUnknown, err)}
		}
		volumeID = id
	}
	encrypted, err := l.Volumes.Encrypted(ctx, volumeID)
	if err != nil {
		return "", fmt.Errorf("read store data volume %s: %w", volumeID, err)
	}
	if !encrypted {
		return "", &VolumeError{VolumeID: volumeID, Err: ErrUnencrypted}
	}
	return volumeID, nil
}

// network reuses the claim's visible ENI, or marks the record and sends one
// create. Once marked, an ENI that is not visible fails closed.
func (l *Launcher) network(ctx context.Context, region, claim string) (*workerlaunch.Network, error) {
	found, err := l.Network.FindStoreNetwork(ctx, region, claim)
	if err != nil {
		return nil, fmt.Errorf("store network: %w", err)
	}
	if found != nil {
		return found, nil
	}
	if err := l.Records.MarkNetworkAttempted(ctx, region, claim); err != nil {
		if errors.Is(err, ErrMarked) {
			return nil, ErrNetworkOutcomeUnknown
		}
		return nil, fmt.Errorf("store network: %w", err)
	}
	created, err := l.Network.CreateStoreNetwork(ctx, region, claim)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNetworkOutcomeUnknown, err)
	}
	return created, nil
}

// bind retries a lost reply with the same arguments, which Bind accepts as
// idempotent.
func (l *Launcher) bind(ctx context.Context, region, claim, instanceID, address string) error {
	var err error
	for range 3 {
		if err = l.Records.Bind(ctx, region, claim, instanceID, address); err == nil {
			return nil
		}
		if errors.Is(err, ErrSuperseded) || errors.Is(err, ErrConflict) {
			return err
		}
	}
	return err
}

func (l *Launcher) claimID() (string, error) {
	if l.newClaimID != nil {
		return l.newClaimID()
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint claim ID: %w", err)
	}
	return "store-" + hex.EncodeToString(b), nil
}
