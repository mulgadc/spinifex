package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// ClusterConfigBucket holds settings fixed when a cluster forms. Unlike
	// ClusterStateBucket it has no TTL, so a key written once stays written.
	ClusterConfigBucket = "spinifex-cluster-config"
	// EBSProviderKey records the one EBS provider the cluster runs.
	EBSProviderKey = "ebs-provider"
)

// EBSProviderRecord is the value at EBSProviderKey.
type EBSProviderRecord struct {
	Provider string `json:"provider"`
	// RecordedBy names the node whose Create won, for the refusal message.
	RecordedBy string `json:"recorded_by"`
}

// EBSProviderMismatchError is the refusal a daemon returns when its configured
// provider is not the one the cluster recorded. Retrying cannot clear it.
type EBSProviderMismatchError struct {
	Node       string
	Configured string
	Recorded   EBSProviderRecord
}

func (e *EBSProviderMismatchError) Error() string {
	return fmt.Sprintf("EBS provider mismatch: node %s is configured with provider %q, but this cluster runs %q (recorded by node %s in KV bucket %s, key %s); "+
		"set [nodes.%s.ebs] provider = %q to join it, or change the cluster's provider by wiping and reforming the cluster, since volumes record no provider to migrate by",
		e.Node, e.Configured, e.Recorded.Provider, e.Recorded.RecordedBy, ClusterConfigBucket, EBSProviderKey, e.Node, e.Recorded.Provider)
}

// Permanent marks the mismatch as one startup retries must not wait out.
func (e *EBSProviderMismatchError) Permanent() bool { return true }

var _ permanentError = (*EBSProviderMismatchError)(nil)

// clusterConfigConfig describes the cluster-config bucket. RecreateIfMissing
// stays false: a lost lock is not reconstructible by recovery, only by the
// next daemon start, which claims it again.
func clusterConfigConfig() kvstore.Config {
	return kvstore.Config{
		Name:        ClusterConfigBucket,
		Description: "Spinifex cluster configuration fixed at formation (EBS provider)",
		History:     1,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, ClusterConfigBucket, kv, ClusterConfigBucketVersion)
		},
		Missing: "cluster config KV bucket not initialized",
	}
}

// InitClusterConfigBucket initializes the cluster-config KV bucket, creating it
// if it doesn't exist.
func (m *JetStreamManager) InitClusterConfigBucket() error {
	m.ebsLock = kvstore.New[EBSProviderRecord](m.js, clusterConfigConfig())
	_, err := m.ebsLock.KV(context.Background())
	return err
}

// ClaimEBSProvider records provider as the cluster's EBS provider if none is
// recorded, and otherwise checks it against the recorded one. Create decides
// a race between nodes, so exactly one provider is ever recorded.
func (m *JetStreamManager) ClaimEBSProvider(ctx context.Context, node, provider string) error {
	if m.ebsLock == nil {
		return errors.New("cluster config KV not initialized")
	}
	_, err := m.ebsLock.Create(ctx, EBSProviderKey, &EBSProviderRecord{Provider: provider, RecordedBy: node})
	if err == nil {
		return nil
	}
	if !errors.Is(err, kvstore.ErrExists) {
		return fmt.Errorf("record EBS provider: %w", err)
	}
	recorded, _, err := m.ebsLock.Get(ctx, EBSProviderKey)
	if err != nil {
		return fmt.Errorf("read recorded EBS provider: %w", err)
	}
	if recorded.Provider != provider {
		return &EBSProviderMismatchError{Node: node, Configured: provider, Recorded: *recorded}
	}
	return nil
}
