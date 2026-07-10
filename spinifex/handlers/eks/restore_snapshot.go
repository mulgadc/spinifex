package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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

// RestoreSnapshotInput names the cluster to restore and, optionally, the exact
// snapshot key (basename) to apply. Empty Snapshot resolves the latest
// frequent-tier snapshot, falling back to the latest of any tier.
type RestoreSnapshotInput struct {
	ClusterName string `json:"clusterName"`
	Snapshot    string `json:"snapshot,omitempty"`
}

// RestoreSnapshotOutput reports the fresh control-plane instance and the
// snapshot key its boot-time recovery agent will apply.
type RestoreSnapshotOutput struct {
	ClusterName   string `json:"clusterName"`
	NewInstanceID string `json:"newInstanceId"`
	Snapshot      string `json:"snapshot"`
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

// RestoreSnapshot drives the single-CP total-loss DR path: launches a fresh
// control-plane VM as a cluster-init seed, directs its boot-time recovery agent
// to `k3s server --cluster-reset` (optionally restoring an etcd snapshot), and
// re-points the cluster NLB target groups at the new CP. HA clusters are out of
// scope — they have a potentially surviving quorum and should be recovered via
// quorum reformation, not a destructive single-node rebuild.
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

	snapshot := input.Snapshot
	if snapshot == "" {
		snapshot, err = resolveLatestSnapshot(ctx, s.deps.SnapshotStore, accountID, input.ClusterName)
		if err != nil {
			return nil, err
		}
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
	// before k3s starts on this very first boot, and the directive is keyed by
	// InstanceID, which only exists once the VM has been launched.
	if _, err := s.SetRecoveryDirective(ctx, &SetRecoveryDirectiveInput{
		ClusterName: input.ClusterName,
		InstanceID:  newNode.InstanceID,
		Action:      RecoveryActionClusterReset,
		Snapshot:    snapshot,
	}, accountID); err != nil {
		return nil, fmt.Errorf("eks: set recovery directive: %w", err)
	}

	sysAcct := admin.SystemAccountID()
	// Re-point the cluster NLB: deregister the old (likely already-dead) CP,
	// register the new one, on both the apiserver and konnectivity TGs.
	// Deregister is best-effort — the primary DR scenario is a CP that is
	// already gone, so its target is likely already unhealthy/absent.
	if oldNode.ENIIP != "" {
		if meta.NLBTargetGroupArn != "" {
			if err := DeregisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.NLBTargetGroupArn, oldNode.ENIIP, k3sAPIServerPort); err != nil {
				slog.WarnContext(ctx, "RestoreSnapshot: deregister old apiserver target failed", "cluster", input.ClusterName, "err", err)
			}
		}
		if meta.KonnTargetGroupArn != "" {
			if err := DeregisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.KonnTargetGroupArn, oldNode.ENIIP, konnectivityAgentPort); err != nil {
				slog.WarnContext(ctx, "RestoreSnapshot: deregister old konnectivity target failed", "cluster", input.ClusterName, "err", err)
			}
		}
	}
	if meta.NLBTargetGroupArn != "" {
		if err := RegisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.NLBTargetGroupArn, newNode.ENIIP, k3sAPIServerPort); err != nil {
			return nil, fmt.Errorf("eks: register new apiserver target: %w", err)
		}
	}
	if meta.KonnTargetGroupArn != "" {
		if err := RegisterClusterTarget(ctx, s.deps.NLB, sysAcct, meta.KonnTargetGroupArn, newNode.ENIIP, konnectivityAgentPort); err != nil {
			return nil, fmt.Errorf("eks: register new konnectivity target: %w", err)
		}
	}

	if err := replaceControlPlaneForRestore(acctKV, input.ClusterName, newNode); err != nil {
		return nil, fmt.Errorf("eks: persist replacement control plane: %w", err)
	}

	// Best-effort: the old CP is a total loss in the primary DR scenario
	// (VM + volume gone), so a terminate failure here is expected, not fatal.
	if oldNode.InstanceID != "" || oldNode.ENIID != "" {
		if err := TerminateK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance, sysAcct, oldNode.InstanceID, oldNode.ENIID); err != nil {
			slog.WarnContext(ctx, "RestoreSnapshot: terminate old control plane failed (already gone in the primary DR scenario)",
				"cluster", input.ClusterName, "instanceId", oldNode.InstanceID, "err", err)
		}
	}

	slog.InfoContext(ctx, "RestoreSnapshot: restored control plane from snapshot",
		"cluster", input.ClusterName, "newInstanceId", newNode.InstanceID, "snapshot", snapshot)

	return &RestoreSnapshotOutput{
		ClusterName:   input.ClusterName,
		NewInstanceID: newNode.InstanceID,
		Snapshot:      snapshot,
	}, nil
}
