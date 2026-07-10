package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go"
)

// eksBackupsBucket is the predastore bucket the guest etcd-snapshot cron PUTs
// to and this restore path lists/reads from. Keys are
// {accountID}/{clusterName}/etcd-{tier}-{timestamp}.snap.
const eksBackupsBucket = "eks-backups-system"

// Old-CP fence retry budget. A wedged-but-alive old CP left running while the
// new one takes over is split-brain (two live etcds), so terminate is retried
// before the restore is declared complete.
const (
	restoreFenceAttempts = 3
	restoreFenceDelay    = 2 * time.Second
)

// RestoreSnapshotInput names the cluster to restore and, optionally, the exact
// snapshot key (basename) to apply. Empty Snapshot resolves the latest
// frequent-tier snapshot, falling back to the latest of any tier.
type RestoreSnapshotInput struct {
	ClusterName string `json:"clusterName"`
	Snapshot    string `json:"snapshot,omitempty"`
}

// RestoreSnapshotOutput reports the fresh control-plane instance and the
// snapshot key its boot-time recovery agent will apply. Status is deliberately
// provisional: a successful return means the new CP was launched, its restore
// directive queued, meta re-pointed and the old CP fenced — NOT that etcd is
// restored and the apiserver is serving. That is observed asynchronously by the
// reconciler, so the caller must verify cluster health before relying on it.
type RestoreSnapshotOutput struct {
	ClusterName   string `json:"clusterName"`
	NewInstanceID string `json:"newInstanceId"`
	Snapshot      string `json:"snapshot"`
	Status        string `json:"status"`
}

// etcdSnapshotKey is a parsed "etcd-{tier}-{timestamp}.snap" object basename.
type etcdSnapshotKey struct {
	tier      string
	timestamp string
	basename  string
}

// parseEtcdSnapshotKey parses a snapshot object basename (not the full S3 key)
// into tier + timestamp. The timestamp (YYYYMMDDTHHMMSSZ) is fixed-width, so
// lexicographic comparison of the timestamp alone is chronological — tier names
// vary in length, so comparing the whole basename would not be.
func parseEtcdSnapshotKey(basename string) (etcdSnapshotKey, bool) {
	const prefix, suffix = "etcd-", ".snap"
	if !strings.HasPrefix(basename, prefix) || !strings.HasSuffix(basename, suffix) {
		return etcdSnapshotKey{}, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(basename, prefix), suffix)
	i := strings.LastIndex(mid, "-")
	if i <= 0 || i == len(mid)-1 {
		return etcdSnapshotKey{}, false
	}
	return etcdSnapshotKey{tier: mid[:i], timestamp: mid[i+1:], basename: basename}, true
}

// resolveLatestSnapshot lists the cluster's snapshot prefix and returns the
// newest object's basename, preferring the frequent tier (tightest RPO) and
// falling back to the newest snapshot of any tier.
func resolveLatestSnapshot(ctx context.Context, store objectstore.ObjectStore, accountID, clusterName string) (string, error) {
	if store == nil {
		return "", errors.New("eks: no snapshot store configured; pass --snapshot explicitly")
	}
	prefix := accountID + "/" + clusterName + "/"
	out, err := store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(eksBackupsBucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return "", fmt.Errorf("list etcd snapshots: %w", err)
	}
	var bestFrequent, bestAny etcdSnapshotKey
	haveFrequent, haveAny := false, false
	for _, obj := range out.Contents {
		base := strings.TrimPrefix(aws.StringValue(obj.Key), prefix)
		parsed, ok := parseEtcdSnapshotKey(base)
		if !ok {
			continue
		}
		if !haveAny || parsed.timestamp > bestAny.timestamp {
			bestAny, haveAny = parsed, true
		}
		if parsed.tier == "frequent" && (!haveFrequent || parsed.timestamp > bestFrequent.timestamp) {
			bestFrequent, haveFrequent = parsed, true
		}
	}
	switch {
	case haveFrequent:
		return bestFrequent.basename, nil
	case haveAny:
		return bestAny.basename, nil
	default:
		return "", fmt.Errorf("eks: no etcd snapshots found under %s/%s", eksBackupsBucket, prefix)
	}
}

// validateSnapshotExists HEADs the snapshot object before any mutation. An
// explicit --snapshot that is typo'd or expired would otherwise be handed to the
// guest verbatim, its fetch fail, and — without the required-snapshot guard — a
// fresh seed cluster-reset into an EMPTY datastore reported as success. A nil
// store cannot verify, so it is a hard error rather than an optimistic pass.
func validateSnapshotExists(ctx context.Context, store objectstore.ObjectStore, accountID, clusterName, basename string) error {
	if store == nil {
		return errors.New("eks: no snapshot store configured; cannot verify the snapshot exists before a destructive restore")
	}
	key := accountID + "/" + clusterName + "/" + basename
	if _, err := store.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(eksBackupsBucket),
		Key:    aws.String(key),
	}); err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return fmt.Errorf("eks: snapshot %q not found under %s/%s/%s/; refusing to restore (a cluster-reset with no snapshot boots an EMPTY cluster)",
				basename, eksBackupsBucket, accountID, clusterName)
		}
		return fmt.Errorf("eks: verify snapshot %q exists: %w", basename, err)
	}
	return nil
}

// replaceControlPlaneForRestore overwrites meta's single control-plane member
// with the freshly provisioned node, mirroring the create-path scalar fields
// (service_impl.go CreateCluster). restore-snapshot only ever targets a
// single-CP cluster (isHAControlPlane guards the caller), so this replaces
// wholesale rather than matching a dead instance ID like SwapControlPlaneMember.
func replaceControlPlaneForRestore(kv nats.KeyValue, name string, newNode ControlPlaneNode) error {
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
		m.ControlPlaneNodes = []ControlPlaneNode{newNode}
		m.ControlPlaneInstanceID = newNode.InstanceID
		m.ControlPlaneENIID = newNode.ENIID
		m.ControlPlaneENIIP = newNode.ENIIP
		m.ControlPlaneMgmtIP = newNode.MgmtIP
		return true
	})
}

// unwindFreshCP rolls back a just-launched fresh CP when a step before the meta
// commit fails. It supersedes the recovery directive with a no-op (so a VM that
// boots before terminate lands does not destructively reset) then terminates the
// VM. Both are best-effort and logged: leaving an orphan boot a resetting CP,
// which the operator would then compound by re-running restore into a second one.
func (s *EKSServiceImpl) unwindFreshCP(ctx context.Context, accountID, clusterName string, node ControlPlaneNode) {
	if _, err := s.SetRecoveryDirective(ctx, &SetRecoveryDirectiveInput{
		ClusterName: clusterName,
		InstanceID:  node.InstanceID,
		Action:      RecoveryActionNone,
	}, accountID); err != nil {
		slog.WarnContext(ctx, "RestoreSnapshot: clear directive during unwind failed; guest may still reset if it boots",
			"cluster", clusterName, "instanceId", node.InstanceID, "err", err)
	}
	if err := TerminateK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, admin.SystemAccountID(), node.InstanceID, node.ENIID); err != nil {
		slog.WarnContext(ctx, "RestoreSnapshot: terminate fresh CP during unwind failed; may orphan a resetting control plane",
			"cluster", clusterName, "instanceId", node.InstanceID, "err", err)
	}
}

// confirmOldCPTerminated fences the old control plane: it retries terminate until
// the instance is confirmed gone (TerminateK3sServerVM treats an already-absent
// instance as success, the primary DR case). A persistent failure means a
// wedged-but-alive old CP that would run its etcd alongside the new one —
// split-brain — so the caller must surface it loudly rather than declare success.
func (s *EKSServiceImpl) confirmOldCPTerminated(ctx context.Context, accountID string, node ControlPlaneNode, attempts int, delay time.Duration) error {
	if node.InstanceID == "" && node.ENIID == "" {
		return nil
	}
	var lastErr error
	for i := range attempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		if err := TerminateK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, accountID, node.InstanceID, node.ENIID); err != nil {
			lastErr = err
			slog.WarnContext(ctx, "RestoreSnapshot: old control plane terminate attempt failed",
				"instanceId", node.InstanceID, "attempt", i+1, "err", err)
			continue
		}
		return nil
	}
	return lastErr
}

// RestoreSnapshot drives the single-CP total-loss DR path: it validates the
// snapshot exists, launches a fresh control-plane VM as a cluster-init seed,
// directs its boot-time recovery agent to `k3s server --cluster-reset` restoring
// that snapshot, persists the new CP into meta, re-points the cluster NLB target
// groups, and fences the old CP. HA clusters are out of scope — they have a
// potentially surviving quorum and should be recovered via quorum reformation,
// not a destructive single-node rebuild. The returned status is provisional:
// success means the sequence completed, not that etcd is restored and serving.
func (s *EKSServiceImpl) RestoreSnapshot(ctx context.Context, input *RestoreSnapshotInput, accountID string) (*RestoreSnapshotOutput, error) {
	if input == nil || input.ClusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	meta, err := GetClusterMeta(acctKV, input.ClusterName)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	if isHAControlPlane(meta) {
		return nil, errors.New("eks: restore-snapshot only supports a single control-plane cluster; " +
			"this cluster runs an HA spread with a potentially surviving quorum — use quorum reformation instead")
	}
	if meta.ControlPlaneTemplate == nil {
		return nil, errors.New("eks: cluster has no persisted control-plane template; cannot restore")
	}

	// Resolve + validate the snapshot BEFORE any mutation. resolveLatestSnapshot
	// already guarantees existence (it lists then picks); an explicit key is HEADed
	// so a typo/expired key hard-fails here rather than booting an empty cluster.
	snapshot := input.Snapshot
	if snapshot == "" {
		snapshot, err = resolveLatestSnapshot(ctx, s.deps.SnapshotStore, accountID, input.ClusterName)
		if err != nil {
			return nil, err
		}
	} else if verr := validateSnapshotExists(ctx, s.deps.SnapshotStore, accountID, input.ClusterName, snapshot); verr != nil {
		return nil, verr
	}

	var oldNode ControlPlaneNode
	if oldNodes := controlPlaneTeardownNodes(meta); len(oldNodes) > 0 {
		oldNode = oldNodes[0]
	}
	var excludeHosts []string
	if oldNode.NodeID != "" {
		excludeHosts = []string{oldNode.NodeID}
	}

	newNode, err := s.ProvisionFreshControlPlane(ctx, FreshCPRequest{
		AccountID:    accountID,
		ClusterName:  input.ClusterName,
		Template:     meta.ControlPlaneTemplate,
		ExcludeHosts: excludeHosts,
	})
	if err != nil {
		return nil, fmt.Errorf("eks: provision fresh control plane: %w", err)
	}

	// Wire the recovery directive immediately after launch: the guest applies it
	// before k3s starts on this very first boot, and it is keyed by InstanceID,
	// which only exists once launched. SnapshotRequired makes the guest abort
	// (not reset into an empty datastore) if it cannot fetch the snapshot.
	// Any failure up to and including the meta commit unwinds the fresh CP, so a
	// re-run does not stack a second resetting control plane.
	if _, derr := s.SetRecoveryDirective(ctx, &SetRecoveryDirectiveInput{
		ClusterName:      input.ClusterName,
		InstanceID:       newNode.InstanceID,
		Action:           RecoveryActionClusterReset,
		Snapshot:         snapshot,
		SnapshotRequired: true,
	}, accountID); derr != nil {
		s.unwindFreshCP(ctx, accountID, input.ClusterName, newNode)
		return nil, fmt.Errorf("eks: set recovery directive: %w", derr)
	}

	// Persist the new CP into meta BEFORE re-pointing the NLB. Once committed the
	// new CP is canonical, so a later NLB failure is convergeable by the reconciler
	// (provisional success) rather than a bare error implying nothing happened.
	if perr := replaceControlPlaneForRestore(acctKV, input.ClusterName, newNode); perr != nil {
		s.unwindFreshCP(ctx, accountID, input.ClusterName, newNode)
		return nil, fmt.Errorf("eks: persist replacement control plane: %w", perr)
	}

	sysAcct := admin.SystemAccountID()
	// Re-point the cluster NLB: deregister the old (likely already-dead) CP,
	// register the new one, on both the apiserver and konnectivity TGs. Deregister
	// is best-effort (the old target is likely already unhealthy/absent). A failed
	// register leaves meta already naming the new CP, so the reconciler converges —
	// surfaced as a provisional status, not an error.
	if oldNode.ENIIP != "" {
		if meta.NLBTargetGroupArn != "" {
			if derr := DeregisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.NLBTargetGroupArn, oldNode.ENIIP, k3sAPIServerPort); derr != nil {
				slog.WarnContext(ctx, "RestoreSnapshot: deregister old apiserver target failed", "cluster", input.ClusterName, "err", derr)
			}
		}
		if meta.KonnTargetGroupArn != "" {
			if derr := DeregisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.KonnTargetGroupArn, oldNode.ENIIP, konnectivityAgentPort); derr != nil {
				slog.WarnContext(ctx, "RestoreSnapshot: deregister old konnectivity target failed", "cluster", input.ClusterName, "err", derr)
			}
		}
	}
	nlbComplete := true
	if meta.NLBTargetGroupArn != "" {
		if rerr := RegisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.NLBTargetGroupArn, newNode.ENIIP, k3sAPIServerPort); rerr != nil {
			nlbComplete = false
			slog.WarnContext(ctx, "RestoreSnapshot: register new apiserver target failed; reconciler will converge",
				"cluster", input.ClusterName, "err", rerr)
		}
	}
	if meta.KonnTargetGroupArn != "" {
		if rerr := RegisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.KonnTargetGroupArn, newNode.ENIIP, konnectivityAgentPort); rerr != nil {
			nlbComplete = false
			slog.WarnContext(ctx, "RestoreSnapshot: register new konnectivity target failed; reconciler will converge",
				"cluster", input.ClusterName, "err", rerr)
		}
	}

	// Fence the old CP before declaring success: a wedged-but-alive old CP running
	// its etcd next to the new one is split-brain. Terminate is retried; if the old
	// instance cannot be confirmed gone, fail loudly (the new CP is up and canonical
	// — the operator must fence the old one manually, NOT re-run restore).
	if ferr := s.confirmOldCPTerminated(ctx, sysAcct, oldNode, restoreFenceAttempts, restoreFenceDelay); ferr != nil {
		return nil, fmt.Errorf("eks: restore of %q brought up new control plane %s but the old control plane %s could not be confirmed terminated after %d attempts (split-brain risk) — fence it manually, do NOT re-run restore: %w",
			input.ClusterName, newNode.InstanceID, oldNode.InstanceID, restoreFenceAttempts, ferr)
	}

	status := fmt.Sprintf("restore initiated: new control plane %s is booting and will restore etcd from snapshot %s — verify cluster health before relying on it",
		newNode.InstanceID, snapshot)
	if !nlbComplete {
		status = fmt.Sprintf("control plane %s restored and recorded, but NLB re-point is incomplete; the reconciler will converge — verify cluster health",
			newNode.InstanceID)
	}
	slog.InfoContext(ctx, "RestoreSnapshot: control-plane restore sequence complete",
		"cluster", input.ClusterName, "newInstanceId", newNode.InstanceID, "snapshot", snapshot, "nlbComplete", nlbComplete)

	return &RestoreSnapshotOutput{
		ClusterName:   input.ClusterName,
		NewInstanceID: newNode.InstanceID,
		Snapshot:      snapshot,
		Status:        status,
	}, nil
}
