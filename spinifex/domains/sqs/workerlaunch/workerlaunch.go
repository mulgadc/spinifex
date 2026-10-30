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
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/nats-io/nats.go/jetstream"
)

// Volumes prepares the worker's data volume and reads its own metadata.
type Volumes interface {
	// Prepare creates a new, empty, formatted volume and returns its ID.
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

// NetworkAllocator finds or creates the one ENI for a launch claim, so a
// retry of the same claim reuses it.
type NetworkAllocator interface {
	WorkerNetwork(ctx context.Context, region, claimID string) (*Network, error)
}

// Spec is one worker VM launch. StoreNKey is a public key; nothing in a spec
// is ever a seed.
type Spec struct {
	InstanceType string
	ProfileARN   string
	Network      Network
	DataVolumeID string
	StoreStatus  string
	StoreNKey    string
}

// VMLauncher starts the worker VM on this node. Any error leaves the outcome
// unknown to the caller.
type VMLauncher interface {
	LaunchWorker(ctx context.Context, spec Spec) (string, error)
}

// Store status values the guest reads; mirrored from the system-instance
// contract, which this domain does not import.
const (
	StoreNew      = "new"
	StoreExisting = "existing"
)

var (
	// ErrOutcomeUnknown is returned when a dispatched launch may or may not
	// have started a VM. The intent stays recorded until it is reconciled.
	ErrOutcomeUnknown = errors.New("SQS worker launch outcome is unknown; reconcile before relaunching")
	ErrUnencrypted    = errors.New("SQS worker data volume is not encrypted")
	ErrIncomplete     = errors.New("SQS worker launcher is missing a capability")
)

// Launcher realizes the worker on NodeID, the node it runs on and the only
// node the worker is pinned to.
type Launcher struct {
	Identities workerident.Store
	FirstStore firststore.Store
	Volumes    Volumes
	Roles      Roles
	Network    NetworkAllocator
	VMs        VMLauncher
	NodeID     string

	newClaimID func() (string, error)
}

// Result is the Region's realized worker.
type Result struct {
	InstanceID string
	Generation uint64
}

// Ensure returns the Region's bound worker, or launches it. A record that is
// bound is returned as is; one launching and already dispatched is refused
// with ErrOutcomeUnknown; a stopped or failed one is replaced only after the
// first-store fence excludes its generation.
func (l *Launcher) Ensure(ctx context.Context, region, instanceType string) (Result, error) {
	if l.Identities.KV == nil || l.FirstStore.KV == nil || l.Volumes == nil || l.Roles == nil ||
		l.Network == nil || l.VMs == nil || l.NodeID == "" || instanceType == "" {
		return Result{}, ErrIncomplete
	}
	cur, err := l.Identities.Current(ctx, region)
	var req workerident.LaunchRequest
	var fresh string
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		req, err = l.first(ctx)
		if err != nil {
			return Result{}, err
		}
		fresh = req.DataVolumeID
	case err != nil:
		return Result{}, err
	case cur.NodeID != l.NodeID:
		return Result{}, workerident.ErrWrongNode
	case cur.State == workerident.StateBound:
		return Result{InstanceID: cur.InstanceID, Generation: cur.Generation}, nil
	case cur.State == workerident.StateLaunching && cur.Dispatched:
		return Result{}, ErrOutcomeUnknown
	case cur.State == workerident.StateLaunching:
		// An undispatched intent started nothing; resume it with its claim.
		req = workerident.LaunchRequest{ClaimID: cur.ClaimID, Supersedes: cur.Generation - 1,
			NodeID: l.NodeID, DataVolumeID: cur.DataVolumeID}
	default:
		// Stopped or failed: confirmed to hold nothing. Fence the replaced
		// generation out of the first-store decision before the next begins.
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

	intent, err := l.begin(ctx, region, req)
	if err != nil {
		if fresh != "" {
			return Result{}, &UnclaimedVolumeError{VolumeID: fresh, Err: err}
		}
		return Result{}, err
	}

	// Nothing below is dispatched yet: a failure leaves an undispatched
	// intent that the next call resumes with the same claim.
	profile, err := l.Roles.EnsureWorkerProfile(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("ensure worker instance profile: %w", err)
	}
	network, err := l.Network.WorkerNetwork(ctx, region, intent.ClaimID)
	if err != nil {
		return Result{}, fmt.Errorf("worker network: %w", err)
	}
	key, err := l.FirstStore.RecordedKey(ctx, region)
	if err != nil {
		return Result{}, fmt.Errorf("read first-store decision: %w", err)
	}
	spec := Spec{InstanceType: instanceType, ProfileARN: profile, Network: *network,
		DataVolumeID: req.DataVolumeID, StoreStatus: StoreNew}
	if key != "" {
		spec.StoreStatus, spec.StoreNKey = StoreExisting, key
	}

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
	return Result{InstanceID: instanceID, Generation: intent.Generation}, nil
}

// first prepares the Region's first worker volume, after everything that can
// fail without allocating. A volume this call cannot record is reported in an
// UnclaimedVolumeError and left in place, never deleted.
func (l *Launcher) first(ctx context.Context) (workerident.LaunchRequest, error) {
	claim, err := l.claimID()
	if err != nil {
		return workerident.LaunchRequest{}, err
	}
	volumeID, err := l.Volumes.Prepare(ctx)
	if err != nil {
		return workerident.LaunchRequest{}, fmt.Errorf("prepare worker data volume: %w", err)
	}
	return workerident.LaunchRequest{ClaimID: claim, NodeID: l.NodeID, DataVolumeID: volumeID}, nil
}

// begin checks the volume's own record and records the launch intent.
func (l *Launcher) begin(ctx context.Context, region string, req workerident.LaunchRequest) (workerident.Intent, error) {
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

// UnclaimedVolumeError reports a worker volume this call created but did not
// confirm recording in a launch intent. Its ownership is unproven, so it is
// left for an operator; a BeginLaunch whose reply was lost may still hold it.
type UnclaimedVolumeError struct {
	VolumeID string
	Err      error
}

func (e *UnclaimedVolumeError) Error() string {
	return fmt.Sprintf("worker data volume %s was created but this call could not confirm it was recorded: %v", e.VolumeID, e.Err)
}

func (e *UnclaimedVolumeError) Unwrap() error { return e.Err }

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
