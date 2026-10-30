// Package workerlaunch realizes the Region's shared SQS worker from its
// identity record: one intent, one dispatched launch, one bound instance. A
// launch whose outcome is unknown is never sent again; it waits to be
// reconciled, so an ambiguous reply can never start a second VM.
package workerlaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/nats-io/nats.go/jetstream"
)

// Volumes prepares the worker's data volume and reads its own metadata.
type Volumes interface {
	// Prepare creates a new, empty, formatted volume and returns its ID. An
	// error wrapping storevolume.ErrNotCreated proves no create was sent.
	Prepare(ctx context.Context) (string, error)
	// Encrypted reports the volume record's own encryption flag.
	Encrypted(ctx context.Context, volumeID string) (bool, error)
}

// Roles ensures the worker's instance role and profile and returns the
// profile ARN.
type Roles interface {
	EnsureWorkerProfile(ctx context.Context) (string, error)
}

// Network is the worker's primary interface in the SQS system VPC.
type Network struct {
	SubnetID string
	ENIID    string
	ENIMac   string
	ENIIP    string
	// CIDR is the ENI address with the subnet's prefix length.
	CIDR    string
	Gateway string
}

// NetworkAllocator manages the one ENI for a launch claim. Find only looks
// and returns nil when none is visible; Create sends a create, after reusing
// one that is already visible.
type NetworkAllocator interface {
	FindWorkerNetwork(ctx context.Context, region, claimID string) (*Network, error)
	CreateWorkerNetwork(ctx context.Context, region, claimID string) (*Network, error)
}

// Spec is one worker VM launch. It carries no key: the guest takes its
// first-store decision from the gateway at boot.
type Spec struct {
	InstanceType string
	ProfileARN   string
	Network      Network
	DataVolumeID string
	Region       string
}

// Host reports whether this node can boot a worker now: its image is
// complete and its boot inputs exist. It allocates and records nothing.
type Host interface {
	Ready(ctx context.Context) error
}

// VMLauncher starts the worker VM on this node. Any error leaves the outcome
// unknown to the caller.
type VMLauncher interface {
	LaunchWorker(ctx context.Context, spec Spec) (string, error)
}

var (
	// ErrOutcomeUnknown is returned when a dispatched launch may or may not
	// have started a VM. The intent stays recorded until it is reconciled.
	ErrOutcomeUnknown = errors.New("SQS worker launch outcome is unknown; reconcile before relaunching")
	ErrUnencrypted    = errors.New("SQS worker data volume is not encrypted")
	ErrIncomplete     = errors.New("SQS worker launcher is missing a capability")
	ErrHostNotReady   = errors.New("this node cannot boot an SQS worker yet")
	// ErrNetworkOutcomeUnknown is returned when the claim's ENI create was
	// sent but no ENI is visible; the create is not repeated until reconciled.
	ErrNetworkOutcomeUnknown = errors.New("SQS worker network create outcome is unknown; reconcile before relaunching")
	// ErrVolumeNotCreated is a volume Prepare that sent no create; a retry may
	// prepare one.
	ErrVolumeNotCreated = errors.New("SQS worker volume was not created")
	// ErrVolumeOutcomeUnknown is returned for a volume create that was sent
	// but not recorded; the create is not repeated until reconciled.
	ErrVolumeOutcomeUnknown = errors.New("SQS worker volume create outcome is unknown; reconcile before relaunching")
)

// Launcher realizes the worker on NodeID, the node it runs on and the only
// node the worker is pinned to.
type Launcher struct {
	Identities workerident.Store
	FirstStore firststore.Store
	Volumes    Volumes
	Roles      Roles
	Network    NetworkAllocator
	Host       Host
	VMs        VMLauncher
	NodeID     string
	// RetainedVolume extracts a volume ID from a Prepare error, if any.
	RetainedVolume func(error) string

	newClaimID func() (string, error)
}

// Result is the Region's realized worker.
type Result struct {
	InstanceID   string
	Generation   uint64
	InstanceType string
}

// Ensure returns the Region's bound worker, or launches it. A record that is
// bound is returned as is; one launching and already dispatched is refused
// with ErrOutcomeUnknown; a stopped or failed one is replaced only after the
// first-store fence excludes its generation.
func (l *Launcher) Ensure(ctx context.Context, region, instanceType string) (Result, error) {
	if l.Identities.KV == nil || l.FirstStore.KV == nil || l.Volumes == nil || l.Roles == nil ||
		l.Network == nil || l.Host == nil || l.VMs == nil || l.NodeID == "" || instanceType == "" {
		return Result{}, ErrIncomplete
	}
	cur, err := l.Identities.Current(ctx, region)
	var req workerident.LaunchRequest
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		// The first launch records its intent before any volume exists.
		if err := l.hostReady(ctx); err != nil {
			return Result{}, err
		}
		claim, err := l.claimID()
		if err != nil {
			return Result{}, err
		}
		req = workerident.LaunchRequest{ClaimID: claim, NodeID: l.NodeID}
	case err != nil:
		return Result{}, err
	case cur.NodeID != l.NodeID:
		return Result{}, workerident.ErrWrongNode
	case cur.InstanceType != instanceType &&
		(cur.State == workerident.StateBound || (cur.State == workerident.StateLaunching && cur.Allocated())):
		// Changing the type needs a replacement, never a silent answer for the old VM.
		return Result{}, fmt.Errorf("%w: %s", workerident.ErrTypeChanged, cur.InstanceType)
	case cur.State == workerident.StateBound:
		return Result{InstanceID: cur.InstanceID, Generation: cur.Generation, InstanceType: cur.InstanceType}, nil
	case cur.State == workerident.StateLaunching && cur.Dispatched:
		return Result{}, ErrOutcomeUnknown
	case cur.State == workerident.StateLaunching:
		// An undispatched intent started nothing; resume it with its claim.
		if err := l.hostReady(ctx); err != nil {
			return Result{}, err
		}
		req = workerident.LaunchRequest{ClaimID: cur.ClaimID, Supersedes: cur.Generation - 1,
			NodeID: l.NodeID, DataVolumeID: cur.DataVolumeID}
	default:
		// Stopped or failed: confirmed to hold nothing. Fence the replaced
		// generation out of the first-store decision before the next begins.
		if err := l.hostReady(ctx); err != nil {
			return Result{}, err
		}
		if err := l.FirstStore.Fence(ctx, region, cur.Generation+1); err != nil {
			return Result{}, fmt.Errorf("fence replaced worker generation: %w", err)
		}
		claim, err := l.claimID()
		if err != nil {
			return Result{}, err
		}
		req = workerident.LaunchRequest{ClaimID: claim, Supersedes: cur.Generation,
			NodeID: l.NodeID, DataVolumeID: cur.DataVolumeID}
	}

	req.InstanceType = instanceType
	intent, err := l.begin(ctx, region, req)
	if err != nil {
		return Result{}, err
	}

	// Nothing below is dispatched yet: a failure leaves an undispatched
	// intent that the next call resumes with the same claim.
	volumeID, err := l.volume(ctx, intent, req.DataVolumeID)
	if err != nil {
		return Result{}, err
	}
	profile, err := l.Roles.EnsureWorkerProfile(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("ensure worker instance profile: %w", err)
	}
	network, err := l.network(ctx, region, intent)
	if err != nil {
		return Result{}, err
	}
	spec := Spec{InstanceType: instanceType, ProfileARN: profile, Network: *network,
		DataVolumeID: volumeID, Region: region}

	if err := l.Identities.MarkDispatched(ctx, intent); err != nil {
		return Result{}, err
	}
	instanceID, err := l.VMs.LaunchWorker(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	if err := l.bind(ctx, intent, instanceID); err != nil {
		return Result{}, fmt.Errorf("worker %s launched but not bound: %w", instanceID, err)
	}
	return Result{InstanceID: instanceID, Generation: intent.Generation, InstanceType: instanceType}, nil
}

// network reuses the claim's visible ENI, or marks the intent and sends one
// create. Once marked, an ENI that is not visible fails closed: the create
// may still land, so a second one could give the claim two interfaces.
func (l *Launcher) network(ctx context.Context, region string, intent workerident.Intent) (*Network, error) {
	found, err := l.Network.FindWorkerNetwork(ctx, region, intent.ClaimID)
	if err != nil {
		return nil, fmt.Errorf("worker network: %w", err)
	}
	if found != nil {
		return found, nil
	}
	if err := l.Identities.MarkNetworkAttempted(ctx, intent); err != nil {
		if errors.Is(err, workerident.ErrNetworkAttempted) {
			return nil, ErrNetworkOutcomeUnknown
		}
		return nil, fmt.Errorf("worker network: %w", err)
	}
	created, err := l.Network.CreateWorkerNetwork(ctx, region, intent.ClaimID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNetworkOutcomeUnknown, err)
	}
	return created, nil
}

// hostReady runs before any volume, intent, network or dispatch, so a node
// that cannot boot the worker refuses with nothing allocated or recorded.
func (l *Launcher) hostReady(ctx context.Context) error {
	if err := l.Host.Ready(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrHostNotReady, err)
	}
	return nil
}

// volume returns the intent's recorded volume, or marks the intent and
// prepares one. A volume it knows of but cannot use is reported in a
// VolumeError and left in place, never deleted.
func (l *Launcher) volume(ctx context.Context, intent workerident.Intent, recorded string) (string, error) {
	if recorded != "" {
		return recorded, nil
	}
	if err := l.Identities.MarkVolumeAttempted(ctx, intent); err != nil {
		if errors.Is(err, workerident.ErrVolumeAttempted) {
			return "", ErrVolumeOutcomeUnknown
		}
		return "", fmt.Errorf("worker data volume: %w", err)
	}
	id, err := l.Volumes.Prepare(ctx)
	if errors.Is(err, storevolume.ErrNotCreated) {
		if cerr := l.Identities.ClearVolumeAttempt(ctx, intent); cerr != nil {
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
	if err := l.Identities.RecordVolume(ctx, intent, id); err != nil {
		return "", &VolumeError{VolumeID: id, Err: fmt.Errorf("%w: %w", ErrVolumeOutcomeUnknown, err)}
	}
	encrypted, err := l.Volumes.Encrypted(ctx, id)
	if err != nil {
		return "", &VolumeError{VolumeID: id, Err: fmt.Errorf("read its record: %w", err)}
	}
	if !encrypted {
		return "", &VolumeError{VolumeID: id, Err: ErrUnencrypted}
	}
	return id, nil
}

// begin checks a recorded volume's own record and records the launch intent.
func (l *Launcher) begin(ctx context.Context, region string, req workerident.LaunchRequest) (workerident.Intent, error) {
	if req.DataVolumeID == "" {
		return l.Identities.BeginLaunch(ctx, region, req)
	}
	encrypted, err := l.Volumes.Encrypted(ctx, req.DataVolumeID)
	if err != nil {
		return workerident.Intent{}, fmt.Errorf("read data volume %s: %w", req.DataVolumeID, err)
	}
	if !encrypted {
		return workerident.Intent{}, fmt.Errorf("%w: %s", ErrUnencrypted, req.DataVolumeID)
	}
	req.DataVolumeEncrypted = true
	return l.Identities.BeginLaunch(ctx, region, req)
}

// VolumeError reports a worker volume this call knows of but could not use:
// it is kept, never deleted, and its ID stays visible to the operator.
type VolumeError struct {
	VolumeID string
	Err      error
}

func (e *VolumeError) Error() string {
	return fmt.Sprintf("SQS worker data volume %s is kept: %v", e.VolumeID, e.Err)
}

func (e *VolumeError) Unwrap() error { return e.Err }

// bind retries a lost reply with the same arguments, which BindInstance
// accepts as idempotent.
func (l *Launcher) bind(ctx context.Context, intent workerident.Intent, instanceID string) error {
	var err error
	for range 3 {
		if err = l.Identities.BindInstance(ctx, intent, instanceID); err == nil {
			return nil
		}
		if errors.Is(err, workerident.ErrSuperseded) || errors.Is(err, workerident.ErrConflict) {
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
	return "claim-" + hex.EncodeToString(b), nil
}
