package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ebsobligation "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/obligation"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// InstanceNodeLocator reports the node that last ran an instance. found is
// false when no record of the instance exists.
type InstanceNodeLocator interface {
	InstanceLastNode(instanceID string) (node string, found bool, err error)
}

// SetUnpublishObligations wires the obligation store a cleared attachment
// records into, the locator that names the node holding the publish, and the
// cluster members to name when the instance has no record.
func (s *VolumeServiceImpl) SetUnpublishObligations(store *ebsobligation.Store, locator InstanceNodeLocator, members func() []string) {
	s.obligations = store
	s.instanceNodes = locator
	s.members = members
}

// publishingNodes names the node holding instanceID's publish. With no record
// of the instance there is no node to name, so every member is named and each
// settles its own.
func (s *VolumeServiceImpl) publishingNodes(instanceID string) ([]string, error) {
	if s.instanceNodes == nil {
		return nil, errors.New("no instance node locator configured")
	}
	node, found, err := s.instanceNodes.InstanceLastNode(instanceID)
	if err != nil {
		return nil, fmt.Errorf("read instance %s: %w", instanceID, err)
	}
	if found && node != "" {
		return []string{node}, nil
	}
	var members []string
	if s.members != nil {
		members = s.members()
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("instance %s has no record and no cluster member is known", instanceID)
	}
	return members, nil
}

// recordObligations writes one obligation per node for the attachment meta
// still carries. Every write must land before the caller clears it.
func (s *VolumeServiceImpl) recordObligations(ctx context.Context, meta ebsmetadata.Volume, nodes []string) error {
	if s.obligations == nil {
		return errors.New("no unpublish obligation store configured")
	}
	now := time.Now()
	for _, node := range nodes {
		if err := s.obligations.Record(ctx, ebsobligation.UnpublishObligation{
			VolumeID:   meta.VolumeID,
			AccountID:  meta.TenantID,
			Handle:     meta.ProviderHandle,
			NodeID:     node,
			InstanceID: meta.AttachedInstance,
			AttachedAt: meta.AttachedAt,
			RecordedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// recordTerminateObligation records the obligation for a terminate that clears
// owed's attachment with its publish still in place. A volume no longer
// attached to that instance has nothing of it left to clear.
func (s *VolumeServiceImpl) recordTerminateObligation(ctx context.Context, accountID, volumeID string, owed ec2instance.OwedUnpublish) error {
	if !owed.Owed() {
		return nil
	}
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if objectstore.IsNoSuchKeyError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read volume %s before recording its unpublish obligation: %w", volumeID, err)
	}
	if meta.AttachedInstance != owed.InstanceID {
		return nil
	}
	if err := s.recordObligations(ctx, meta, []string{owed.NodeID}); err != nil {
		return fmt.Errorf("record unpublish obligation for %s: %w", volumeID, err)
	}
	return nil
}

// ObligationUnpublisher is this node's mounter as the obligation reaper drives
// it. Its lock is the one every publish and unpublish on this node takes.
type ObligationUnpublisher interface {
	// HoldVolume takes volumeID's lock without waiting. false means a publish
	// or unpublish of it is in flight here, and the sweep comes back later.
	HoldVolume(volumeID string) (VolumeHold, bool)
}

// VolumeHold is one volume's publish lock on this node.
type VolumeHold interface {
	// PublishedSince reports whether this process published the volume after
	// t and has not unpublished it since.
	PublishedSince(t time.Time) bool
	// Unpublish tears down this node's export of the volume. A volume the
	// provider holds no export of counts as done.
	Unpublish(ctx context.Context, accountID, handle string, noSeal bool) error
	Release()
}

// obligationAction is what settling an obligation does with the volume as it
// is now.
type obligationAction int

const (
	// obligationWait leaves the obligation open: the attachment it names has
	// not been cleared yet, or a publish here is newer than it.
	obligationWait obligationAction = iota
	// obligationSuperseded closes it without a provider call: a newer
	// attachment owns the export.
	obligationSuperseded
	// obligationSeal unpublishes with a seal: nobody else writes the volume,
	// so the copy here is the last owner's and its writes are kept.
	obligationSeal
	// obligationNoSeal unpublishes without sealing a volume being deleted.
	obligationNoSeal
)

func (a obligationAction) String() string {
	switch a {
	case obligationSuperseded:
		return "superseded"
	case obligationSeal:
		return "seal"
	case obligationNoSeal:
		return "no-seal"
	default:
		return "wait"
	}
}

// decideObligation is the settlement policy over the volume's current record;
// meta is nil when the volume has none. It never unpublishes an attachment the
// volume still carries.
func decideObligation(o ebsobligation.UnpublishObligation, meta *ebsmetadata.Volume) obligationAction {
	if meta == nil || meta.State == volumeStateDeleting {
		return obligationNoSeal
	}
	if meta.AttachedInstance != "" {
		if meta.AttachedInstance != o.InstanceID || meta.AttachedAt.After(o.AttachedAt) {
			return obligationSuperseded
		}
		return obligationWait
	}
	return obligationSeal
}

// obligationSince is the time a publish on this node must follow to be newer
// than the one an obligation undoes.
func obligationSince(o ebsobligation.UnpublishObligation) time.Time {
	if o.AttachedAt.IsZero() {
		return o.RecordedAt
	}
	return o.AttachedAt
}

// unpublishObligationReaper settles the obligations naming its node through
// that node's mounter, once the node answers.
type unpublishObligationReaper struct {
	svc         *VolumeServiceImpl
	node        string
	unpublisher ObligationUnpublisher
}

var _ vm.Reaper = (*unpublishObligationReaper)(nil)

// NewUnpublishObligationReaper builds the reaper that settles node's
// obligations through unpublisher, which must be the node's own mounter.
func (s *VolumeServiceImpl) NewUnpublishObligationReaper(node string, unpublisher ObligationUnpublisher) vm.Reaper {
	return &unpublishObligationReaper{svc: s, node: node, unpublisher: unpublisher}
}

func (r *unpublishObligationReaper) Class() string { return "ebs-unpublish-obligation" }

// Scope is node-local: an obligation is settled only by the node it names,
// because only that node can tear its own export down.
func (r *unpublishObligationReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep settles what it can and reports how many obligations it closed. A
// failure is recorded on the obligation and retried on the next sweep.
func (r *unpublishObligationReaper) Sweep(ctx context.Context) (int, error) {
	if r.svc.obligations == nil || r.unpublisher == nil {
		return 0, nil
	}
	open, err := r.svc.obligations.ListNode(ctx, r.node)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, o := range open {
		if err := ctx.Err(); err != nil {
			return closed, err
		}
		done, err := r.settle(ctx, o)
		if err != nil {
			slog.WarnContext(ctx, "ebs-unpublish-obligation: settlement failed, retrying next sweep",
				"volumeId", o.VolumeID, "node", o.NodeID, "instanceId", o.InstanceID, "attempts", o.Attempts+1, "err", err)
			if recErr := r.svc.obligations.RecordFailure(ctx, o, err); recErr != nil {
				slog.WarnContext(ctx, "ebs-unpublish-obligation: could not record the failure",
					"volumeId", o.VolumeID, "node", o.NodeID, "err", recErr)
			}
			continue
		}
		if done {
			closed++
		}
	}
	return closed, nil
}

// settle acts on one obligation under the volume's lock, so no publish on this
// node falls between reading the volume and unpublishing it.
func (r *unpublishObligationReaper) settle(ctx context.Context, o ebsobligation.UnpublishObligation) (bool, error) {
	hold, ok := r.unpublisher.HoldVolume(o.VolumeID)
	if !ok {
		return false, nil
	}
	defer hold.Release()

	var current *ebsmetadata.Volume
	meta, err := r.svc.metadata.GetVolume(ctx, o.AccountID, o.VolumeID)
	switch {
	case err == nil:
		current = &meta
	case objectstore.IsNoSuchKeyError(err):
	default:
		return false, fmt.Errorf("read volume %s: %w", o.VolumeID, err)
	}

	action := decideObligation(o, current)
	// A newer publish here may own the export, so only a superseded obligation,
	// which sends nothing, closes past it.
	if action != obligationSuperseded && hold.PublishedSince(obligationSince(o)) {
		return false, nil
	}
	switch action {
	case obligationWait:
		return false, nil
	case obligationSuperseded:
		slog.WarnContext(ctx, "ebs-unpublish-obligation: a newer attachment owns the volume, closing without unpublishing",
			"volumeId", o.VolumeID, "node", o.NodeID, "instanceId", o.InstanceID, "attachedInstance", current.AttachedInstance)
	case obligationSeal, obligationNoSeal:
		if err := hold.Unpublish(ctx, o.AccountID, o.Handle, action == obligationNoSeal); err != nil {
			return false, err
		}
		slog.InfoContext(ctx, "ebs-unpublish-obligation: unpublished the export a cleared attachment left",
			"volumeId", o.VolumeID, "node", o.NodeID, "instanceId", o.InstanceID, "noSeal", action == obligationNoSeal)
	}
	if err := r.svc.obligations.Close(ctx, o); err != nil {
		return false, fmt.Errorf("close obligation: %w", err)
	}
	recordObligationClosed(o.NodeID, action.String())
	return true, nil
}

// NodeDeparted reports whether node has left the cluster, rather than being
// down, stale or partitioned, which must not count.
type NodeDeparted func(ctx context.Context, node string) (bool, error)

// unpublishObligationCloser closes, without a provider call, the obligations of
// nodes removed from the cluster: such a node holds nothing a member can reach.
type unpublishObligationCloser struct {
	svc      *VolumeServiceImpl
	departed NodeDeparted
}

var _ vm.Reaper = (*unpublishObligationCloser)(nil)

// NewUnpublishObligationCloser builds the cluster-wide pass that closes the
// obligations of nodes departed reports as gone.
func (s *VolumeServiceImpl) NewUnpublishObligationCloser(departed NodeDeparted) vm.Reaper {
	return &unpublishObligationCloser{svc: s, departed: departed}
}

func (c *unpublishObligationCloser) Class() string { return "ebs-unpublish-obligation-close" }

// Scope is cluster-wide: membership is the cluster's, and one pass is enough.
func (c *unpublishObligationCloser) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

func (c *unpublishObligationCloser) Sweep(ctx context.Context) (int, error) {
	if c.svc.obligations == nil || c.departed == nil {
		return 0, nil
	}
	open, err := c.svc.obligations.List(ctx)
	if err != nil {
		return 0, err
	}
	gone := map[string]bool{}
	closed := 0
	for _, o := range open {
		departed, seen := gone[o.NodeID]
		if !seen {
			departed, err = c.departed(ctx, o.NodeID)
			if err != nil {
				slog.WarnContext(ctx, "ebs-unpublish-obligation: cannot tell whether a node left the cluster, keeping its obligations",
					"node", o.NodeID, "err", err)
				departed = false
			}
			gone[o.NodeID] = departed
		}
		if !departed {
			continue
		}
		if err := c.svc.obligations.Close(ctx, o); err != nil {
			slog.WarnContext(ctx, "ebs-unpublish-obligation: failed to close a departed node's obligation",
				"volumeId", o.VolumeID, "node", o.NodeID, "err", err)
			continue
		}
		slog.WarnContext(ctx, "ebs-unpublish-obligation: closed for a node removed from the cluster",
			"volumeId", o.VolumeID, "node", o.NodeID, "instanceId", o.InstanceID)
		recordObligationClosed(o.NodeID, "departed")
		closed++
	}
	return closed, nil
}

const obligationMeterName = "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume/obligation"

var (
	obligationClosedOnce    sync.Once
	obligationClosedCounter metric.Int64Counter
)

// recordObligationClosed counts one closed obligation by the node it named
// and how it closed.
func recordObligationClosed(node, outcome string) {
	obligationClosedOnce.Do(func() {
		var err error
		obligationClosedCounter, err = otel.Meter(obligationMeterName).Int64Counter("mulga.ebs.unpublish_obligation.closed",
			metric.WithDescription("Unpublish obligations closed, by the node they named and how they closed."),
			metric.WithUnit("{obligation}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	if obligationClosedCounter == nil {
		return
	}
	obligationClosedCounter.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("node", node), attribute.String("outcome", outcome)))
}
