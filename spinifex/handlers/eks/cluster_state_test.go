package handlers_eks

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClusterStateTestKV(t *testing.T) nats.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
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

	require.Error(t, PutClusterMeta(kv, nil))
	require.Error(t, PutClusterMeta(kv, &ClusterMeta{}))
}

func TestPutClusterMeta_RoundTrip(t *testing.T) {
	kv := newClusterStateTestKV(t)

	in := sampleClusterMeta("alpha")
	require.NoError(t, PutClusterMeta(kv, in))

	got, err := GetClusterMeta(kv, "alpha")
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

	_, err := GetClusterMeta(kv, "missing")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestGetClusterMeta_EmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, err := GetClusterMeta(kv, "")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrClusterNotFound)
}

func TestGetClusterMeta_CorruptBlobRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	_, err := kv.Put(ClusterMetaKey("corrupt"), []byte("{not json"))
	require.NoError(t, err)

	_, err = GetClusterMeta(kv, "corrupt")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterStatus_TransitionsAndIsIdempotent(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("alpha")))

	require.NoError(t, SetClusterStatus(kv, "alpha", ClusterStatusActive))
	got, err := GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)

	require.NoError(t, SetClusterStatus(kv, "alpha", ClusterStatusActive))
	got, err = GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)
}

func TestSetClusterStatus_DeletingAlwaysAllowedFromAnyState(t *testing.T) {
	kv := newClusterStateTestKV(t)

	for _, from := range []ClusterStatus{ClusterStatusCreating, ClusterStatusActive, ClusterStatusFailed} {
		meta := sampleClusterMeta("alpha")
		meta.Status = from
		require.NoError(t, PutClusterMeta(kv, meta))
		require.NoError(t, SetClusterStatus(kv, "alpha", ClusterStatusDeleting))
		got, err := GetClusterMeta(kv, "alpha")
		require.NoError(t, err)
		assert.Equal(t, ClusterStatusDeleting, got.Status, "from=%s", from)
	}
}

func TestSetClusterStatus_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := SetClusterStatus(kv, "missing", ClusterStatusActive)
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterStatus_RecoversFromConcurrentRevisionBump(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("alpha")))

	// Read once to get a stale revision, then bump the record from another
	// goroutine via Put to force one CAS conflict on the next Update.
	entry, err := kv.Get(ClusterMetaKey("alpha"))
	require.NoError(t, err)

	bumped := sampleClusterMeta("alpha")
	bumped.Version = "1.32-bumped"
	data, err := json.Marshal(bumped)
	require.NoError(t, err)
	_, err = kv.Update(ClusterMetaKey("alpha"), data, entry.Revision())
	require.NoError(t, err)

	// SetClusterStatus must succeed even though there has been an intervening
	// revision bump — its internal loop re-reads + retries.
	require.NoError(t, SetClusterStatus(kv, "alpha", ClusterStatusActive))
	got, err := GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusActive, got.Status)
	assert.Equal(t, "1.32-bumped", got.Version)
}

func TestMarkClusterFailed_FromCreatingSetsStatusAndReason(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("alpha")))

	require.NoError(t, MarkClusterFailed(kv, "alpha", "bootstrap failed: boom"))
	got, err := GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, ClusterStatusFailed, got.Status)
	assert.Equal(t, "bootstrap failed: boom", got.StatusReason)
}

func TestMarkClusterFailed_NoopFromNonCreating(t *testing.T) {
	kv := newClusterStateTestKV(t)

	for _, from := range []ClusterStatus{ClusterStatusActive, ClusterStatusDeleting, ClusterStatusFailed} {
		meta := sampleClusterMeta("alpha")
		meta.Status = from
		require.NoError(t, PutClusterMeta(kv, meta))

		require.NoError(t, MarkClusterFailed(kv, "alpha", "late bootstrap error"))
		got, err := GetClusterMeta(kv, "alpha")
		require.NoError(t, err)
		assert.Equal(t, from, got.Status, "from=%s must be untouched", from)
		assert.Empty(t, got.StatusReason, "from=%s reason must not be set", from)
	}
}

func TestMarkClusterFailed_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := MarkClusterFailed(kv, "ghost", "boom")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterCertificateAuthority_WritesAndIsIdempotent(t *testing.T) {
	kv := newClusterStateTestKV(t)
	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("alpha")))

	require.NoError(t, SetClusterCertificateAuthority(kv, "alpha", "ca-blob-b64"))
	got, err := GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "ca-blob-b64", got.CertificateAuthorityB64)

	require.NoError(t, SetClusterCertificateAuthority(kv, "alpha", "ca-blob-b64"))
	got, err = GetClusterMeta(kv, "alpha")
	require.NoError(t, err)
	assert.Equal(t, "ca-blob-b64", got.CertificateAuthorityB64)
}

func TestSetClusterCertificateAuthority_MissingReturnsErrClusterNotFound(t *testing.T) {
	kv := newClusterStateTestKV(t)

	err := SetClusterCertificateAuthority(kv, "ghost", "ca-blob")
	require.ErrorIs(t, err, ErrClusterNotFound)
}

func TestSetClusterCertificateAuthority_EmptyInputsRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.Error(t, SetClusterCertificateAuthority(kv, "", "ca-blob"))
	require.Error(t, SetClusterCertificateAuthority(kv, "alpha", ""))
}

func TestDeleteClusterPrefix_SweepsEveryKey(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("alpha")))
	_, err := kv.Put(NodegroupKey("alpha", "ng-1"), []byte(`{"name":"ng-1"}`))
	require.NoError(t, err)
	_, err = kv.Put(OIDCSigningKeyKey("alpha"), []byte("enc-blob"))
	require.NoError(t, err)
	_, err = kv.Put(OIDCJWKSKey("alpha"), []byte(`{"keys":[]}`))
	require.NoError(t, err)
	_, err = kv.Put(EventKey("alpha", "1700000000"), []byte(`{"ts":"1700000000"}`))
	require.NoError(t, err)
	// Sibling cluster must survive the sweep.
	require.NoError(t, PutClusterMeta(kv, sampleClusterMeta("beta")))

	require.NoError(t, DeleteClusterPrefix(kv, "alpha"))

	for _, k := range []string{
		ClusterMetaKey("alpha"),
		NodegroupKey("alpha", "ng-1"),
		OIDCSigningKeyKey("alpha"),
		OIDCJWKSKey("alpha"),
		EventKey("alpha", "1700000000"),
	} {
		_, err := kv.Get(k)
		assert.True(t, errors.Is(err, nats.ErrKeyNotFound), "key %s should be gone, got err=%v", k, err)
	}

	got, err := GetClusterMeta(kv, "beta")
	require.NoError(t, err)
	assert.Equal(t, "beta", got.Name)
}

func TestDeleteClusterPrefix_EmptyBucketIsNoop(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.NoError(t, DeleteClusterPrefix(kv, "ghost"))
}

func TestDeleteClusterPrefix_EmptyNameRejected(t *testing.T) {
	kv := newClusterStateTestKV(t)

	require.Error(t, DeleteClusterPrefix(kv, ""))
}
