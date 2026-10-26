package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// OIDCJWKSReader resolves the raw JWKS document published for an EKS
// cluster's OIDC issuer, from the per-account bucket this package owns.
// It is the transitional domains/sts-facing adapter; runtime/roles/awsgw
// wires it into STS as a stsdomain.ClusterJWKSResolver.
type OIDCJWKSReader struct {
	js jetstream.JetStream
}

// NewOIDCJWKSReader constructs an OIDCJWKSReader over js.
func NewOIDCJWKSReader(js jetstream.JetStream) *OIDCJWKSReader {
	return &OIDCJWKSReader{js: js}
}

// LookupClusterJWKS returns the raw JWKS bytes published for clusterName in
// accountID's EKS bucket. A missing account bucket or missing key reads as
// (nil, false, nil); any other KV failure is (nil, false, err).
func (r *OIDCJWKSReader) LookupClusterJWKS(ctx context.Context, accountID, clusterName string) ([]byte, bool, error) {
	if r == nil || r.js == nil {
		return nil, false, errors.New("nil JetStream context")
	}
	if accountID == "" || clusterName == "" {
		return nil, false, errors.New("accountID and clusterName must be non-empty")
	}

	bucketName := AccountBucketName(accountID)
	kv, err := r.js.KeyValue(ctx, bucketName)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open EKS account bucket %s: %w", bucketName, err)
	}

	entry, err := kv.Get(ctx, OIDCJWKSKey(clusterName))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read JWKS for cluster %s: %w", clusterName, err)
	}
	return entry.Value(), true, nil
}
