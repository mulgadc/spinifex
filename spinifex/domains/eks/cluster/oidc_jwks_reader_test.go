package cluster

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The literals below are deliberate: they pin the KV layout OIDCJWKSReader
// resolves against, independent of the AccountBucketName/OIDCJWKSKey helpers
// it calls.
const (
	jwksReaderAccountID     = "000000000000"
	jwksReaderCluster       = "demo-cluster"
	jwksReaderBucketLiteral = "eks-account-000000000000"
	jwksReaderKeyLiteral    = "clusters/demo-cluster/oidc-jwks.json"
)

// recordingJS wraps a JetStream to record, and optionally fail, the bucket
// opens and key reads OIDCJWKSReader performs.
type recordingJS struct {
	jetstream.JetStream

	mu      sync.Mutex
	opened  []string
	gotKeys []string
	openErr func(bucket string) error
	getErr  func(bucket, key string) error
}

func (r *recordingJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	r.mu.Lock()
	r.opened = append(r.opened, bucket)
	r.mu.Unlock()
	if r.openErr != nil {
		if err := r.openErr(bucket); err != nil {
			return nil, err
		}
	}
	kv, err := r.JetStream.KeyValue(ctx, bucket)
	if err != nil {
		return nil, err
	}
	return &recordingKV{KeyValue: kv, parent: r, bucket: bucket}, nil
}

func (r *recordingJS) snapshot() (opened, keys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.opened...), append([]string(nil), r.gotKeys...)
}

type recordingKV struct {
	jetstream.KeyValue

	parent *recordingJS
	bucket string
}

func (k *recordingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.parent.mu.Lock()
	k.parent.gotKeys = append(k.parent.gotKeys, k.bucket+"|"+key)
	k.parent.mu.Unlock()
	if k.parent.getErr != nil {
		if err := k.parent.getErr(k.bucket, key); err != nil {
			return nil, err
		}
	}
	return k.KeyValue.Get(ctx, key)
}

func seedJWKSReaderFixture(t *testing.T) (*recordingJS, *OIDCJWKSReader) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv, err := GetOrCreateAccountBucket(t.Context(), js, jwksReaderAccountID)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), OIDCJWKSKey(jwksReaderCluster), []byte(`{"keys":[{"kty":"EC","kid":"k1"}]}`))
	require.NoError(t, err)

	rec := &recordingJS{JetStream: js}
	return rec, NewOIDCJWKSReader(rec)
}

// OIDCJWKSReader opens exactly eks-account-{accountID} and reads exactly
// clusters/{cluster}/oidc-jwks.json, pinned as literals independent of
// AccountBucketName/OIDCJWKSKey.
func TestOIDCJWKSReader_UsesLiteralBucketAndKey(t *testing.T) {
	rec, reader := seedJWKSReaderFixture(t)

	raw, found, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	require.NoError(t, err)
	assert.True(t, found)
	assert.NotEmpty(t, raw)

	opened, keys := rec.snapshot()
	assert.Contains(t, opened, jwksReaderBucketLiteral)
	assert.Contains(t, keys, jwksReaderBucketLiteral+"|"+jwksReaderKeyLiteral)
	eksOpens := 0
	for _, b := range opened {
		if strings.HasPrefix(b, "eks-account-") {
			eksOpens++
		}
	}
	assert.Equal(t, 1, eksOpens, "one EKS bucket open per lookup; opened=%v", opened)
}

// Bucket-open and key-get dependency failures are reported as err, distinct
// from not-found; a tombstoned key reads as not published.
func TestOIDCJWKSReader_DependencyFailureSemantics(t *testing.T) {
	rec, reader := seedJWKSReaderFixture(t)

	rec.openErr = func(string) error { return context.DeadlineExceeded }
	raw, found, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	assert.Nil(t, raw)
	assert.False(t, found)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "open EKS account bucket "+jwksReaderBucketLiteral)

	rec.openErr = nil
	rec.getErr = func(string, string) error { return context.DeadlineExceeded }
	raw, found, err = reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	assert.Nil(t, raw)
	assert.False(t, found)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "read JWKS for cluster "+jwksReaderCluster)

	rec.getErr = nil
	acctKV, err := GetOrCreateAccountBucket(t.Context(), rec.JetStream, jwksReaderAccountID)
	require.NoError(t, err)
	require.NoError(t, acctKV.Delete(t.Context(), OIDCJWKSKey(jwksReaderCluster)))
	raw, found, err = reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	require.NoError(t, err, "a deleted (tombstoned) key reads as not published")
	assert.Nil(t, raw)
	assert.False(t, found)
}

// A missing account bucket is not-found, not an error.
func TestOIDCJWKSReader_AccountBucketMissingIsNotFound(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	reader := NewOIDCJWKSReader(js)

	raw, found, err := reader.LookupClusterJWKS(t.Context(), "999999999999", jwksReaderCluster)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, raw)
}

func TestOIDCJWKSReader_NilJetStream(t *testing.T) {
	reader := NewOIDCJWKSReader(nil)
	_, _, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	require.Error(t, err)
}

func TestOIDCJWKSReader_EmptyArgs(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	reader := NewOIDCJWKSReader(js)

	_, _, err := reader.LookupClusterJWKS(t.Context(), "", jwksReaderCluster)
	require.Error(t, err)
	_, _, err = reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, "")
	require.Error(t, err)
}

// Pins today's behaviour, not the target: the reader returns a published JWKS
// whatever the cluster's ClusterMeta status, or with no cluster record at all.
func TestCurrentBehaviour_ClusterRecordStateIsNotConsulted(t *testing.T) {
	for _, status := range []ClusterStatus{
		"", // no cluster record at all
		ClusterStatusCreating,
		ClusterStatusActive,
		ClusterStatusDeleting,
		ClusterStatusFailed,
	} {
		t.Run(string(status)+"_", func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			js := testutil.NewJetStream(t, nc)
			kv, err := GetOrCreateAccountBucket(t.Context(), js, jwksReaderAccountID)
			require.NoError(t, err)
			if status != "" {
				require.NoError(t, PutClusterMeta(t.Context(), kv,
					&ClusterMeta{Name: jwksReaderCluster, Status: status}))
			}
			raw := []byte(`{"keys":[{"kty":"EC","kid":"k1"}]}`)
			_, err = kv.Put(t.Context(), OIDCJWKSKey(jwksReaderCluster), raw)
			require.NoError(t, err)

			reader := NewOIDCJWKSReader(js)
			got, found, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, raw, got)
		})
	}
}

// Pins today's behaviour: once teardown sweeps clusters/{name}/, a previously
// published JWKS reads as not found.
func TestCurrentBehaviour_ClusterPrefixDeletedIsNotFound(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, jwksReaderAccountID)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(t.Context(), kv,
		&ClusterMeta{Name: jwksReaderCluster, Status: ClusterStatusDeleting}))
	_, err = kv.Put(t.Context(), OIDCJWKSKey(jwksReaderCluster), []byte(`{"keys":[{"kty":"EC","kid":"k1"}]}`))
	require.NoError(t, err)

	reader := NewOIDCJWKSReader(js)
	_, found, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	require.NoError(t, err)
	require.True(t, found, "sanity: JWKS published before the prefix is swept")

	require.NoError(t, DeleteClusterChildren(t.Context(), kv, jwksReaderCluster))
	raw, found, err := reader.LookupClusterJWKS(t.Context(), jwksReaderAccountID, jwksReaderCluster)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, raw)
}
