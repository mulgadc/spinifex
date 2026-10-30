package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// sqsWorkerVolumeGiB sizes the worker's key disk, which holds one key file.
const sqsWorkerVolumeGiB = 1

// sqsWorkerEnsureTimeout bounds one launch; the caller's own timeout does not
// cancel it, so a retry after a lost reply observes the outcome instead.
const sqsWorkerEnsureTimeout = 30 * time.Minute

// sqsWorkerEnsurer serializes this node's worker launches over one composed
// launcher, so the network allocator's per-claim lock is shared by every call.
type sqsWorkerEnsurer struct {
	sem    chan struct{}
	build  func(ctx context.Context) (*workerlaunch.Launcher, error)
	mu     sync.Mutex
	cached *workerlaunch.Launcher
}

func newSQSWorkerEnsurer(build func(ctx context.Context) (*workerlaunch.Launcher, error)) *sqsWorkerEnsurer {
	return &sqsWorkerEnsurer{sem: make(chan struct{}, 1), build: build}
}

func (e *sqsWorkerEnsurer) launcher(ctx context.Context) (*workerlaunch.Launcher, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cached != nil {
		return e.cached, nil
	}
	l, err := e.build(ctx)
	if err != nil {
		return nil, err
	}
	e.cached = l
	return l, nil
}

// ensure runs one launch at a time; a waiter whose context ends gives up.
func (e *sqsWorkerEnsurer) ensure(ctx context.Context, region, instanceType string) (workerlaunch.Result, error) {
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return workerlaunch.Result{}, ctx.Err()
	}
	defer func() { <-e.sem }()
	if err := ctx.Err(); err != nil {
		return workerlaunch.Result{}, err
	}
	l, err := e.launcher(ctx)
	if err != nil {
		return workerlaunch.Result{}, err
	}
	return l.Ensure(ctx, region, instanceType)
}

// buildSQSWorkerLauncher composes the launcher from this daemon's real volume,
// role, network and direct-boot services. The daemon never holds a worker key.
func (d *Daemon) buildSQSWorkerLauncher(ctx context.Context) (*workerlaunch.Launcher, error) {
	if d.natsConn == nil || d.volumeService == nil || d.ebsProvider == nil || d.vpcService == nil {
		return nil, fmt.Errorf("%w: cluster services are not ready", workerlaunch.ErrIncomplete)
	}
	js, err := jetstream.New(d.natsConn)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: jetstream: %w", err)
	}
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		return nil, err
	}
	first, err := firststore.Open(ctx, js)
	if err != nil {
		return nil, err
	}
	prep := &storevolume.Preparer{Volumes: d.volumeService, Provider: d.ebsProvider, NodeID: d.node,
		ScratchDir: d.config.DataDir, WriteImage: nbd.WriteRawImage}
	vols := sqsWorkerVolumes{prep: prep, volumes: d.volumeService, availabilityZone: d.config.AZ,
		sizeGiB: sqsWorkerVolumeGiB, encryptionKeyFile: d.config.Viperblock.EncryptionKeyFile}
	return &workerlaunch.Launcher{
		Identities: ids,
		FirstStore: first,
		Volumes:    vols,
		Roles:      sqsWorkerRoles{provider: d.systemRoleEnsurer},
		Network:    newSQSWorkerNetwork(d.vpcService, &d.config.SQS),
		Host:       sqsWorkerHost{check: d.sqsWorkerHostReady},
		VMs:        sqsWorkerVMs{launch: d.LaunchSQSWorkerInstance},
		NodeID:     d.node,
	}, nil
}

// handleSQSWorkerEnsure answers sqs.worker.Ensure.v1.<node>. The launch runs in
// the daemon's own context, so an abandoned request still completes it.
func (d *Daemon) handleSQSWorkerEnsure(msg *nats.Msg) string {
	var req workerlaunch.EnsureRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}
	if req.NodeID != d.node {
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(
			awserrors.ErrorInvalidParameterValue, "request names a different node"))
		return outcomeForCode(awserrors.ErrorInvalidParameterValue)
	}
	if _, ok := d.resourceMgr.instanceTypes[req.InstanceType]; !ok || req.InstanceType == "" {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidInstanceType)
	}
	base := d.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, sqsWorkerEnsureTimeout)
	defer cancel()
	res, err := d.sqsWorker.ensure(ctx, d.config.Region, req.InstanceType)
	if err != nil {
		code, message := sqsWorkerEnsureError(err)
		slog.WarnContext(ctx, "SQS worker ensure failed", "node", d.node, "code", code, "err", err)
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(code, message))
		return outcomeForCode(code)
	}
	respondWithJSON(d.node, msg, workerlaunch.EnsureResponse{
		NodeID: d.node, Region: d.config.Region, InstanceID: res.InstanceID,
		Generation: res.Generation, InstanceType: res.InstanceType, State: workerlaunch.StateBound,
	})
	return outcomeSuccess
}

// sqsWorkerEnsureError maps a launch failure onto a code the caller can act
// on. An ambiguous dispatched launch has its own code and is never resent.
func sqsWorkerEnsureError(err error) (code, message string) {
	if errors.Is(err, workerlaunch.ErrOutcomeUnknown) {
		return awserrors.ErrorIncorrectState, workerlaunch.ErrOutcomeUnknown.Error()
	}
	if errors.Is(err, workerlaunch.ErrNetworkOutcomeUnknown) {
		return awserrors.ErrorIncorrectState, workerlaunch.ErrNetworkOutcomeUnknown.Error()
	}
	if unclaimed, ok := errors.AsType[*workerlaunch.UnclaimedVolumeError](err); ok {
		return awserrors.ErrorServerInternal, unclaimed.Error()
	}
	switch {
	case errors.Is(err, workerident.ErrTypeChanged):
		return awserrors.ErrorIncorrectState, err.Error()
	case errors.Is(err, workerident.ErrWrongNode):
		return awserrors.ErrorInvalidParameterValue, "the Region's SQS worker is pinned to another node"
	case errors.Is(err, workerlaunch.ErrUnencrypted):
		return awserrors.ErrorUnsupportedOperation, err.Error()
	case errors.Is(err, workerlaunch.ErrIncomplete), errors.Is(err, workerlaunch.ErrHostNotReady):
		return awserrors.ErrorServiceUnavailable, err.Error()
	default:
		return awserrors.ErrorServerInternal, err.Error()
	}
}
