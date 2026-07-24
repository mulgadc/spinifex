package handlers_eks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStream KV bucket and key-path constants for the EKS control plane.
//
// Per-account bucket "eks-account-{accountID}" holds all customer-visible
// cluster state. It is created lazily on first cluster touch (no daemon-boot
// pre-creation) so accounts without any EKS clusters never grow a bucket.
//
// Shared leader bucket "spinifex-eks-leader" holds 60s-TTL CAS locks
// keyed by "{accountID}/{clusterName}" for the per-cluster reconciler
// leader election. Created once at daemon boot.
const (
	KVBucketEKSAccountPrefix  = "eks-account-"
	KVBucketEKSAccountVersion = 1

	// KVBucketEKSAccountHistory pins the per-account bucket to one revision per
	// key. This MUST stay 1: ZeroizeClusterOIDCKey relies on Purge dropping the
	// key's whole history so no prior revision of the encrypted OIDC signing key
	// survives a DeleteCluster. Keep this decoupled from the schema version so a
	// future version bump can't silently widen history and resurrect ciphertext.
	KVBucketEKSAccountHistory = 1

	KVBucketEKSLeader        = "spinifex-eks-leader"
	KVBucketEKSLeaderVersion = 1
	KVBucketEKSLeaderTTL     = 60 * time.Second
)

// Key-path helpers for the per-account bucket. The layout matches the
// EKS v1 spec (Q2):
//
//	clusters/{name}/meta
//	clusters/{name}/nodegroups/{ngName}
//	clusters/{name}/access-entries/{principalARN}
//	clusters/{name}/oidc-providers/{issuerHash}
//	clusters/{name}/oidc-signing-key.pem.enc
//	clusters/{name}/oidc-jwks.json
//	clusters/{name}/admin-kubeconfig.enc
//	clusters/{name}/k3s-node-token.enc
//	clusters/{name}/events/{ts}

// ClusterMetaKey returns the KV key for a cluster's meta record.
func ClusterMetaKey(name string) string {
	return fmt.Sprintf("clusters/%s/meta", name)
}

// NodegroupsPrefix returns the KV key prefix under which all of a cluster's
// nodegroup records live. Used by ListNodegroups to enumerate.
func NodegroupsPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/nodegroups/", cluster)
}

// NodegroupKey returns the KV key for a nodegroup record under a cluster.
func NodegroupKey(cluster, ng string) string {
	return NodegroupsPrefix(cluster) + ng
}

// AccessEntriesPrefix returns the KV key prefix under which all of a cluster's
// AccessEntry records live. Used by ListAccessEntries to enumerate.
func AccessEntriesPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/access-entries/", cluster)
}

// AccessEntryKey returns the KV key for an AccessEntry record under a cluster.
// The principal ARN is hashed because IAM ARNs contain ':' which is not a legal
// NATS JetStream KV key character; the record itself carries the plaintext ARN.
func AccessEntryKey(cluster, principalARN string) string {
	return AccessEntriesPrefix(cluster) + PrincipalARNHash(principalARN)
}

// PrincipalARNHash maps an IAM principal ARN to a KV-key-safe token.
func PrincipalARNHash(principalARN string) string {
	sum := sha256.Sum256([]byte(principalARN))
	return hex.EncodeToString(sum[:])
}

// OIDCProviderKey returns the KV key for a registered OIDC provider config
// under a cluster.
func OIDCProviderKey(cluster, issuerHash string) string {
	return fmt.Sprintf("clusters/%s/oidc-providers/%s", cluster, issuerHash)
}

// OIDCSigningKeyKey returns the KV key for a cluster's encrypted ECDSA-P256
// OIDC signing private key.
func OIDCSigningKeyKey(cluster string) string {
	return fmt.Sprintf("clusters/%s/oidc-signing-key.pem.enc", cluster)
}

// OIDCJWKSKey returns the KV key for a cluster's public OIDC JWKS document.
func OIDCJWKSKey(cluster string) string {
	return fmt.Sprintf("clusters/%s/oidc-jwks.json", cluster)
}

// OIDCJWKSVerifiedKey returns the KV key marking that the K3s-VM-published JWKS
// passed the controller cross-check (kid + kty match the controller-generated
// keypair). The reconciler gates the ACTIVE transition on this marker, NOT on
// OIDCJWKSKey — the controller pre-seeds OIDCJWKSKey at create time, so its
// presence proves nothing about the running cluster's actual signing key.
func OIDCJWKSVerifiedKey(cluster string) string {
	return fmt.Sprintf("clusters/%s/oidc-jwks-verified", cluster)
}

// AdminKubeconfigKey returns the KV key for a cluster's encrypted admin
// kubeconfig (used by the spinifex-side nodegroup reconciler).
func AdminKubeconfigKey(cluster string) string {
	return fmt.Sprintf("clusters/%s/admin-kubeconfig.enc", cluster)
}

// NodeTokenKey returns the KV key for a cluster's encrypted K3s bootstrap
// node-token (shared across all nodegroups in the cluster).
func NodeTokenKey(cluster string) string {
	return fmt.Sprintf("clusters/%s/k3s-node-token.enc", cluster)
}

// EventKey returns the KV key for a cluster-scoped event record.
func EventKey(cluster, ts string) string {
	return fmt.Sprintf("clusters/%s/events/%s", cluster, ts)
}

// AddonsPrefix returns the KV key prefix under which all of a cluster's managed
// add-on records live. Used by ListAddons to enumerate.
func AddonsPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/addons/", cluster)
}

// AddonKey returns the KV key for a managed add-on record under a cluster.
func AddonKey(cluster, addon string) string {
	return AddonsPrefix(cluster) + addon
}

// AddonManifestKey returns the KV key under which the rendered manifest for a
// managed add-on is staged for the VM-side delivery transport to consume.
func AddonManifestKey(cluster, addon string) string {
	return AddonsPrefix(cluster) + addon + "/manifest"
}

// RecoveryPrefix returns the KV key prefix under which a cluster's per-member
// control-plane recovery directives live.
func RecoveryPrefix(cluster string) string {
	return fmt.Sprintf("clusters/%s/recovery/", cluster)
}

// RecoveryDirectiveKey returns the KV key for a control-plane member's recovery
// directive, keyed by instance ID so a replacement VM (new ID) starts with none.
func RecoveryDirectiveKey(cluster, instanceID string) string {
	return RecoveryPrefix(cluster) + instanceID
}

// Store is the per-daemon EKS KV handle. Per-account and leader buckets are
// accessed via the package-level factories below.
type Store struct {
	nc *nats.Conn
}

// NewStore constructs a Store bound to the supplied NATS connection. It does
// not touch JetStream — per-account buckets are created lazily by
// GetOrCreateAccountBucket and the leader bucket by InitLeaderBucket.
func NewStore(nc *nats.Conn) (*Store, error) {
	if nc == nil {
		return nil, errors.New("eks store: nats connection is nil")
	}
	return &Store{nc: nc}, nil
}

// AccountBucketName returns the per-account JetStream KV bucket name for the
// given AWS account ID.
func AccountBucketName(accountID string) string {
	return KVBucketEKSAccountPrefix + accountID
}

// accountBucketNames returns the name of every EKS per-account KV bucket. It
// fails rather than returning a short list when the enumeration could not be
// completed: the lister behind it closes its channel identically on success and
// on error, so a caller that ignored the failure would read an unreachable
// JetStream as "no accounts" — and prune every tenant's endpoint record on that
// empty view.
func accountBucketNames(ctx context.Context, nc *nats.Conn) ([]string, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	all, err := kvutil.BucketNames(ctx, js)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, name := range all {
		if strings.HasPrefix(name, KVBucketEKSAccountPrefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetOrCreateAccountBucket returns the per-account KV bucket for accountID,
// creating it on first use at the given replica count (clamped to a minimum
// of 1). Idempotent: subsequent calls with the same accountID return the
// existing handle.
func GetOrCreateAccountBucket(js nats.JetStreamContext, accountID string, replicas int) (nats.KeyValue, error) {
	bucket := AccountBucketName(accountID)
	kv, err := utils.GetOrCreateKVBucketWithReplicas(js, bucket, KVBucketEKSAccountHistory, replicas)
	if err != nil {
		return nil, fmt.Errorf("failed to create EKS per-account KV bucket %s: %w", bucket, err)
	}
	if err := migrate.DefaultRegistry.RunKV(bucket, kv, KVBucketEKSAccountVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", bucket, err)
	}
	return kv, nil
}

// InitLeaderBucket creates (or attaches to) the shared spinifex-eks-leader
// bucket used for per-cluster reconciler leader-lease CAS locks, at the given
// replica count (clamped to a minimum of 1). The bucket is configured with
// History=1 and a 60s TTL so stale leases expire on their own when a leader
// dies mid-cycle. utils.GetOrCreateKVBucketWithReplicas doesn't expose a TTL
// knob, so this function sets Replicas directly on its own js.CreateKeyValue
// call and falls back to js.KeyValue on already-exists.
func InitLeaderBucket(js nats.JetStreamContext, replicas int) (nats.KeyValue, error) {
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   KVBucketEKSLeader,
		History:  1,
		TTL:      KVBucketEKSLeaderTTL,
		Replicas: max(replicas, 1),
	})
	if err != nil {
		kv, err = js.KeyValue(KVBucketEKSLeader)
		if err != nil {
			return nil, fmt.Errorf("failed to create or open EKS leader bucket %s: %w", KVBucketEKSLeader, err)
		}
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketEKSLeader, kv, KVBucketEKSLeaderVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketEKSLeader, err)
	}
	slog.Info("EKS leader bucket initialized", "bucket", KVBucketEKSLeader, "ttl", KVBucketEKSLeaderTTL)
	return kv, nil
}
