package handlers_eks

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// kvBucketClusterTokens is the JetStream KV bucket for CreateCluster
	// ClientRequestToken idempotency records.
	kvBucketClusterTokens = "spinifex-eks-clustertokens" //nolint:gosec // G101 false positive: KV bucket name, not a credential

	// clusterTokenTTL only needs to outlast SDK retry windows around the
	// synchronous phase of CreateCluster, not the async launch: a duplicate
	// replays the meta by name, kept current by the cluster's own KV record.
	clusterTokenTTL = 15 * time.Minute
)

// ClusterTokenStore is the CreateCluster idempotency store. It replays the
// created cluster's name rather than a response, so a duplicate reads the
// cluster's current state instead of a frozen snapshot.
type ClusterTokenStore = idempotency.Store[string]

// getClusterTokenStore lazily initialises s's cluster-token store via sync.Once,
// scoped to the service instance rather than the process, so each EKSServiceImpl
// binds its own NATSConn instead of bleeding a stale one between instances.
func (s *EKSServiceImpl) getClusterTokenStore(ctx context.Context) (*ClusterTokenStore, error) {
	s.clusterTokenOnce.Do(func() {
		js, err := jetstream.New(s.deps.NATSConn)
		if err != nil {
			s.clusterTokenErr = fmt.Errorf("clustertoken jetstream: %w", err)
			return
		}
		// The bind happens once per instance, so it must not inherit the first
		// caller's cancellation: a disconnect mid-open would poison the store for
		// every later create. Deadline-free, so JetStream's own timeout applies.
		s.clusterTokenStore, s.clusterTokenErr = newClusterTokenStore(context.WithoutCancel(ctx), js)
	})
	return s.clusterTokenStore, s.clusterTokenErr
}

func newClusterTokenStore(ctx context.Context, js jetstream.JetStream) (*ClusterTokenStore, error) {
	return idempotency.OpenStore[string](ctx, js, kvBucketClusterTokens, clusterTokenTTL)
}

// clusterTokenParamHash hashes the request excluding ClientRequestToken, so
// identical params always produce the same hash. Must run before any input mutation.
func clusterTokenParamHash(input *eks.CreateClusterInput) string {
	clone := *input
	clone.ClientRequestToken = nil
	return idempotency.ParamHash(&clone)
}
