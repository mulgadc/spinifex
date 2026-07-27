package handlers_eks

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClusterStateTestKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)
	return kv
}

func sampleClusterMeta(name string) *ClusterMeta {
	return &ClusterMeta{
		Name:    name,
		Arn:     "arn:aws:eks:us-east-1:111122223333:cluster/" + name,
		Status:  ClusterStatusCreating,
		Version: "1.32",
		RoleArn: "arn:aws:iam::111122223333:role/eks-cluster",
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds:        []string{"subnet-aaa"},
			SecurityGroupIds: []string{"sg-aaa"},
		},
		CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestPutClusterMeta_NilOrEmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.Error(t, PutClusterMeta(t.Context(), kv, nil))
	require.Error(t, PutClusterMeta(t.Context(), kv, &ClusterMeta{}))
}

func TestPutClusterMeta_RoundTrip(t *testing.T) {
	kv := newClusterStateTestKV(t)

	in := sampleClusterMeta("alpha")
	require.NoError(t, PutClusterMeta(t.Context(), kv, in))

	got, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, in.Name, got.Name)
	assert.Equal(t, in.Arn, got.Arn)
	assert.Equal(t, in.Status, got.Status)
	assert.Equal(t, in.Version, got.Version)
	assert.Equal(t, in.RoleArn, got.RoleArn)
	require.NotNil(t, got.ResourcesVpcConfig)
	assert.Equal(t, []string{"subnet-aaa"}, got.ResourcesVpcConfig.SubnetIds)
	assert.True(t, got.CreatedAt.Equal(in.CreatedAt))
}

func TestGetClusterMeta_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, err := GetClusterMeta(t.Context(), kv, "missing")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestGetClusterMeta_EmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, err := GetClusterMeta(t.Context(), kv, "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrClusterNotFound)
}

func TestGetClusterMeta_CorruptBlobRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, err := kv.Put(t.Context(), ClusterMetaKey("corrupt"), []byte("{not json"))
	require.NoError(t, err)

	_, err = GetClusterMeta(t.Context(), kv, "corrupt")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterStatus_TransitionsAndIsIdempotent(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))

	require.NoError(t, SetClusterStatus(t.Context(), kv, "alpha", ClusterStatusActive))
	got, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)

	require.NoError(t, SetClusterStatus(t.Context(), kv, "alpha", ClusterStatusActive))
	got, err = GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)
}

func TestSetClusterStatus_DeletingAlwaysAllowedFromAnyState(t *testing.T) {
	kv := newClusterStateTestKV(t)

	for _, from := range []ClusterStatus{ClusterStatusCreating, ClusterStatusActive, ClusterStatusFailed} {
		meta := sampleClusterMeta("alpha")
		meta.Status = from
		require.NoError(t, PutClusterMeta(t.Context(), kv, meta))
		require.NoError(t, SetClusterStatus(t.Context(), kv, "alpha", ClusterStatusDeleting))
		got, err := GetClusterMeta(t.Context(), kv, "alpha")
		require.NoError(t, err)
		assert.Equal(t, ClusterStatusDeleting, got.Status, "from=%s", from)
	}
}

func TestSetClusterStatus_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := SetClusterStatus(t.Context(), kv, "missing", ClusterStatusActive)
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterStatus_RecoversFromConcurrentRevisionBump(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))

	// Read once to get a stale revision, then bump the record from another
	// goroutine via Put to force one CAS conflict on the next Update.
	entry, err := kv.Get(t.Context(), ClusterMetaKey("alpha"))
	require.NoError(t, err)

	bumped := sampleClusterMeta("alpha")
	bumped.Version = "1.32-bumped"
	data, err := json.Marshal(bumped)
	require.NoError(t, err)
	_, err = kv.Update(t.Context(), ClusterMetaKey("alpha"), data, entry.Revision())
	require.NoError(t, err)

	// SetClusterStatus must succeed even though there has been an intervening
	// revision bump — its internal loop re-reads + retries.
	require.NoError(t, SetClusterStatus(t.Context(), kv, "alpha", ClusterStatusActive))
	got, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)
	assert.Equal(t, "1.32-bumped", got.Version)
}

func TestMarkClusterFailed_FromCreatingSetsStatusAndReason(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))

	require.NoError(t, MarkClusterFailed(t.Context(), kv, "alpha", "bootstrap failed: boom"))
	got, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusFailed, got.Status)
	assert.Equal(t, "bootstrap failed: boom", got.StatusReason)
}

func TestMarkClusterFailed_NoopFromNonCreating(t *testing.T) {
	kv := newClusterStateTestKV(t)

	for _, from := range []ClusterStatus{ClusterStatusActive, ClusterStatusDeleting, ClusterStatusFailed} {
		meta := sampleClusterMeta("alpha")
		meta.Status = from
		require.NoError(t, PutClusterMeta(t.Context(), kv, meta))

		require.NoError(t, MarkClusterFailed(t.Context(), kv, "alpha", "late bootstrap error"))
		got, err := GetClusterMeta(t.Context(), kv, "alpha")
		require.NoError(t, err)
		assert.Equal(t, from, got.Status, "from=%s must be untouched", from)
		assert.Empty(t, got.StatusReason, "from=%s reason must not be set", from)
	}
}

func TestMarkClusterFailed_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := MarkClusterFailed(t.Context(), kv, "ghost", "boom")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterCertificateAuthority_WritesAndIsIdempotent(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))

	require.NoError(t, SetClusterCertificateAuthority(t.Context(), kv, "alpha", "ca-blob-b64"))
	got, err := GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "ca-blob-b64", got.CertificateAuthorityB64)

	require.NoError(t, SetClusterCertificateAuthority(t.Context(), kv, "alpha", "ca-blob-b64"))
	got, err = GetClusterMeta(t.Context(), kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "ca-blob-b64", got.CertificateAuthorityB64)
}

func TestSetClusterCertificateAuthority_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := SetClusterCertificateAuthority(t.Context(), kv, "ghost", "ca-blob")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterCertificateAuthority_EmptyInputsRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.Error(t, SetClusterCertificateAuthority(t.Context(), kv, "", "ca-blob"))
	require.Error(t, SetClusterCertificateAuthority(t.Context(), kv, "alpha", ""))
}

func TestDeleteClusterPrefix_SweepsEveryKey(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("alpha")))
	_, err := kv.Put(t.Context(), NodegroupKey("alpha", "ng-1"), []byte(`{"name":"ng-1"}`))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), OIDCSigningKeyKey("alpha"), []byte("enc-blob"))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), OIDCJWKSKey("alpha"), []byte(`{"keys":[]}`))
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), EventKey("alpha", "1700000000"), []byte(`{"ts":"1700000000"}`))
	require.NoError(t, err)
	// Sibling cluster must survive the sweep.
	require.NoError(t, PutClusterMeta(t.Context(), kv, sampleClusterMeta("beta")))

	require.NoError(t, DeleteClusterPrefix(t.Context(), kv, "alpha"))

	for _, k := range []string{
		ClusterMetaKey("alpha"),
		NodegroupKey("alpha", "ng-1"),
		OIDCSigningKeyKey("alpha"),
		OIDCJWKSKey("alpha"),
		EventKey("alpha", "1700000000"),
	} {
		_, err := kv.Get(t.Context(), k)
		assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "key %s should be gone, got err=%v", k, err)
	}

	got, err := GetClusterMeta(t.Context(), kv, "beta")
	require.NoError(t, err)
	assert.Equal(t, "beta", got.Name)
}

func TestDeleteClusterPrefix_EmptyBucketIsNoop(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.NoError(t, DeleteClusterPrefix(t.Context(), kv, "ghost"))
}

func TestDeleteClusterPrefix_EmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.Error(t, DeleteClusterPrefix(t.Context(), kv, ""))
}
