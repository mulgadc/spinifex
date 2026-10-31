package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	ebsencryption "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/encryption"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/nbd"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// sqsStoreEnsureTimeout bounds one store launch; the caller's own timeout does
// not cancel it, so a retry after a lost reply observes the outcome instead.
const sqsStoreEnsureTimeout = 30 * time.Minute

// sqsStoreEnsurer serializes this node's store launches over one composed
// launcher, so the network allocator's per-claim lock is shared by every call.
type sqsStoreEnsurer struct {
	sem    chan struct{}
	build  func(ctx context.Context) (*storelaunch.Launcher, error)
	mu     sync.Mutex
	cached *storelaunch.Launcher
}

func newSQSStoreEnsurer(build func(ctx context.Context) (*storelaunch.Launcher, error)) *sqsStoreEnsurer {
	return &sqsStoreEnsurer{sem: make(chan struct{}, 1), build: build}
}

func (e *sqsStoreEnsurer) launcher(ctx context.Context) (*storelaunch.Launcher, error) {
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
func (e *sqsStoreEnsurer) ensure(ctx context.Context, region, instanceType string, sizeGiB int64) (storelaunch.Result, error) {
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return storelaunch.Result{}, ctx.Err()
	}
	defer func() { <-e.sem }()
	if err := ctx.Err(); err != nil {
		return storelaunch.Result{}, err
	}
	l, err := e.launcher(ctx)
	if err != nil {
		return storelaunch.Result{}, err
	}
	return l.Ensure(ctx, region, instanceType, sizeGiB)
}

// buildSQSStoreLauncher composes the store launcher from this daemon's real
// volume, network and direct-boot services. The daemon never holds the
// worker's private key: the store is launched with its recorded public key.
func (d *Daemon) buildSQSStoreLauncher(ctx context.Context) (*storelaunch.Launcher, error) {
	if d.natsConn == nil || d.volumeService == nil || d.ebsProvider == nil || d.vpcService == nil {
		return nil, fmt.Errorf("%w: cluster services are not ready", storelaunch.ErrIncomplete)
	}
	js, err := jetstream.New(d.natsConn)
	if err != nil {
		return nil, fmt.Errorf("sqs store: jetstream: %w", err)
	}
	records, err := storelaunch.Open(ctx, js)
	if err != nil {
		return nil, err
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
	return &storelaunch.Launcher{
		Records:    records,
		Workers:    ids,
		FirstStore: first,
		Volumes: sqsStoreVolumes{inner: sqsWorkerVolumes{prep: prep, volumes: d.volumeService,
			availabilityZone: d.config.AZ, encryptionKeyFile: d.config.Viperblock.EncryptionKeyFile}},
		Network:        sqsStoreNetwork{n: newSQSStoreNetwork(d.vpcService, &d.config.SQS)},
		Host:           sqsStoreHost{check: d.sqsStoreHostReady},
		VMs:            sqsStoreVMs{launch: d.LaunchSQSStoreInstance},
		NodeID:         d.node,
		RetainedVolume: retainedVolume,
	}, nil
}

// sqsStoreHostReady reports whether this node can boot the store now: a
// complete store image and a cluster storage key, so no volume is created
// that would then be refused as unencrypted.
func (d *Daemon) sqsStoreHostReady() error {
	for _, name := range []string{"vmlinuz", "initramfs.cpio.gz"} {
		info, err := os.Stat(filepath.Join(sqsStoreImageDir(), name))
		if err != nil {
			return fmt.Errorf("sqs store image is not installed (%s): %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("sqs store image is incomplete: %s is empty or not a file", name)
		}
	}
	enabled, err := ebsencryption.Enabled(d.config.Viperblock.EncryptionKeyFile)
	if err != nil {
		return fmt.Errorf("sqs store: read cluster storage key: %w", err)
	}
	if !enabled {
		return errors.New("sqs store: the cluster storage key is not configured")
	}
	return nil
}

type sqsStoreHost struct{ check func() error }

func (h sqsStoreHost) Ready(context.Context) error { return h.check() }

// sqsStoreVolumes prepares the store's data volume with the normal
// fresh-volume preparer at the operator's size.
type sqsStoreVolumes struct{ inner sqsWorkerVolumes }

var _ storelaunch.Volumes = sqsStoreVolumes{}

func (v sqsStoreVolumes) Prepare(ctx context.Context, sizeGiB int64) (string, error) {
	w := v.inner
	w.sizeGiB = sizeGiB
	return w.Prepare(ctx)
}

func (v sqsStoreVolumes) Encrypted(ctx context.Context, volumeID string) (bool, error) {
	return v.inner.Encrypted(ctx, volumeID)
}

func retainedVolume(err error) string {
	if r, ok := errors.AsType[*storevolume.RetainedVolumeError](err); ok {
		return r.VolumeID
	}
	return ""
}

// sqsStoreNetwork places the store's ENI in the worker's private subnet.
type sqsStoreNetwork struct{ n *sqsWorkerNetwork }

var _ storelaunch.NetworkAllocator = sqsStoreNetwork{}

func (s sqsStoreNetwork) FindStoreNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error) {
	return s.n.FindWorkerNetwork(ctx, region, claimID)
}

func (s sqsStoreNetwork) CreateStoreNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error) {
	return s.n.CreateWorkerNetwork(ctx, region, claimID)
}

// sqsStoreVMs launches the store on this node through the real direct-boot
// path, which re-checks the volume's encryption and the guest's size.
type sqsStoreVMs struct {
	launch func(in *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error)
}

var _ storelaunch.VMLauncher = sqsStoreVMs{}

func (m sqsStoreVMs) LaunchStore(_ context.Context, spec storelaunch.Spec) (string, error) {
	if m.launch == nil {
		return "", errors.New("sqs store: launcher is not configured")
	}
	out, err := m.launch(sqsStoreInput(spec))
	if err != nil {
		return "", err
	}
	if out == nil || out.InstanceID == "" {
		return "", errors.New("sqs store: launch returned no instance ID")
	}
	return out.InstanceID, nil
}

// sqsStoreInput maps a store spec onto the system-instance launch, with its
// VPC ENI as the only, default NIC.
func sqsStoreInput(spec storelaunch.Spec) *elbv2domain.SystemInstanceInput {
	return &elbv2domain.SystemInstanceInput{
		BootMode:     systeminstance.BootDirect,
		ManagedBy:    tags.ManagedBySQS,
		InstanceType: spec.InstanceType,
		SubnetID:     spec.Network.SubnetID,
		ENIID:        spec.Network.ENIID,
		ENIMac:       spec.Network.ENIMac,
		ENIIP:        spec.Network.ENIIP,
		AccountID:    awsidentifiers.GlobalAccountID,
		NICs: []systeminstance.NICConfig{
			{MAC: spec.Network.ENIMac, CIDR: spec.Network.CIDR, Gateway: spec.Network.Gateway, IsDefault: true},
		},
		Kind:     systeminstance.KindSQSStore,
		SQSStore: &systeminstance.SQSStoreInput{DataVolumeID: spec.DataVolumeID, ClientNKey: spec.ClientNKey},
	}
}

// handleSQSStoreEnsure answers sqs.store.Ensure.v1.<node>. The launch runs in
// the daemon's own context, so an abandoned request still completes it.
func (d *Daemon) handleSQSStoreEnsure(msg *nats.Msg) string {
	var req storelaunch.EnsureRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidParameterValue)
	}
	if req.NodeID != d.node {
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(
			awserrors.ErrorInvalidParameterValue, "request names a different node"))
		return outcomeForCode(awserrors.ErrorInvalidParameterValue)
	}
	it, ok := d.resourceMgr.instanceTypes[req.InstanceType]
	if !ok || req.InstanceType == "" {
		return respondErrorOutcome(d.node, msg, awserrors.ErrorInvalidInstanceType)
	}
	if err := sqsStoreMemoryFits(req.InstanceType, it); err != nil {
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(awserrors.ErrorInvalidParameterValue, err.Error()))
		return outcomeForCode(awserrors.ErrorInvalidParameterValue)
	}
	if req.VolumeSizeGiB < 1 || req.VolumeSizeGiB > storelaunch.MaxSizeGiB {
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(
			awserrors.ErrorInvalidParameterValue, "VolumeSizeGiB must be between 1 and 16384"))
		return outcomeForCode(awserrors.ErrorInvalidParameterValue)
	}
	base := d.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, sqsStoreEnsureTimeout)
	defer cancel()
	res, err := d.sqsStore.ensure(ctx, d.config.Region, req.InstanceType, req.VolumeSizeGiB)
	if err != nil {
		code, message := sqsStoreEnsureError(err)
		slog.WarnContext(ctx, "SQS store ensure failed", "node", d.node, "code", code, "err", err)
		respondNATSMsg(d.node, msg, awserrors.GenerateErrorPayloadWithMessage(code, message))
		return outcomeForCode(code)
	}
	respondWithJSON(d.node, msg, storelaunch.EnsureResponse{
		NodeID: d.node, Region: d.config.Region, InstanceID: res.InstanceID,
		Address: res.Address, InstanceType: res.InstanceType, State: storelaunch.StateBoundResponse,
	})
	return outcomeSuccess
}

// sqsStoreEnsureError maps a launch failure onto a code the caller can act
// on. A step whose outcome is unknown has its own code and is never resent.
func sqsStoreEnsureError(err error) (code, message string) {
	for _, unknown := range []error{storelaunch.ErrOutcomeUnknown, storelaunch.ErrVolumeOutcomeUnknown, storelaunch.ErrNetworkOutcomeUnknown} {
		if errors.Is(err, unknown) {
			msg := unknown.Error()
			if v, ok := errors.AsType[*storelaunch.VolumeError](err); ok {
				msg = v.Error()
			}
			return awserrors.ErrorIncorrectState, msg
		}
	}
	if errors.Is(err, storelaunch.ErrVolumeNotCreated) {
		return awserrors.ErrorServiceUnavailable, err.Error()
	}
	if v, ok := errors.AsType[*storelaunch.VolumeError](err); ok {
		if errors.Is(err, storelaunch.ErrUnencrypted) {
			return awserrors.ErrorUnsupportedOperation, v.Error()
		}
		return awserrors.ErrorServerInternal, v.Error()
	}
	switch {
	case errors.Is(err, storelaunch.ErrWrongNode):
		return awserrors.ErrorInvalidParameterValue, storelaunch.ErrWrongNode.Error()
	case errors.Is(err, storelaunch.ErrWorkerNotBound), errors.Is(err, storelaunch.ErrKeyNotRecorded):
		return awserrors.ErrorIncorrectState, err.Error()
	case errors.Is(err, storelaunch.ErrSizeChanged), errors.Is(err, storelaunch.ErrTypeChanged), errors.Is(err, storelaunch.ErrKeyChanged), errors.Is(err, storelaunch.ErrSuperseded):
		return awserrors.ErrorIncorrectState, err.Error()
	case errors.Is(err, storelaunch.ErrIncomplete), errors.Is(err, storelaunch.ErrHostNotReady):
		return awserrors.ErrorServiceUnavailable, err.Error()
	default:
		return awserrors.ErrorServerInternal, err.Error()
	}
}
