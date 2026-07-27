package handlers_eks

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRecoveryDirective_AbsentIsZeroNone(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	acctKV, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)

	d, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-missing")
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.Epoch, "absent directive reads as epoch 0")
	assert.Equal(t, RecoveryActionNone, d.Action, "absent directive is a none action")
}

func TestStoreRecoveryDirective_EpochIncrements(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	acctKV, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)

	first, err := StoreRecoveryDirective(t.Context(), acctKV, "alpha", "i-0", RecoveryActionClusterReset, "", false)
	require.NoError(t, err)
	assert.Equal(t, int64(1), first.Epoch, "first set starts at epoch 1")

	second, err := StoreRecoveryDirective(t.Context(), acctKV, "alpha", "i-0", RecoveryActionWipeRejoin, "snap.snap", false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), second.Epoch, "each set advances the epoch")

	got, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-0")
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionWipeRejoin, got.Action)
	assert.Equal(t, "snap.snap", got.Snapshot)
	assert.Equal(t, int64(2), got.Epoch)
}

func TestStoreRecoveryDirective_SnapshotRequiredRoundTrips(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	acctKV, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)

	stored, err := StoreRecoveryDirective(t.Context(), acctKV, "alpha", "i-seed", RecoveryActionClusterReset, "etcd-frequent-20260709T010000Z.snap", true)
	require.NoError(t, err)
	assert.True(t, stored.SnapshotRequired, "a required snapshot is recorded so the guest aborts on fetch failure")

	got, err := LoadRecoveryDirective(t.Context(), acctKV, "alpha", "i-seed")
	require.NoError(t, err)
	assert.True(t, got.SnapshotRequired, "SnapshotRequired survives the KV round-trip")
	assert.Equal(t, "etcd-frequent-20260709T010000Z.snap", got.Snapshot)
}

func TestSetRecoveryDirective_RejectsBadInput(t *testing.T) {
	s := &EKSServiceImpl{}

	_, err := s.SetRecoveryDirective(context.Background(), &SetRecoveryDirectiveInput{ClusterName: "", InstanceID: "i-0", Action: RecoveryActionNone}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = s.SetRecoveryDirective(context.Background(), &SetRecoveryDirectiveInput{ClusterName: "alpha", InstanceID: "", Action: RecoveryActionNone}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = s.SetRecoveryDirective(context.Background(), &SetRecoveryDirectiveInput{ClusterName: "alpha", InstanceID: "i-0", Action: "bogus"}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue, "an unknown action is rejected before any KV write")
}

func TestGetRecoveryDirective_RejectsBadInput(t *testing.T) {
	s := &EKSServiceImpl{}

	_, err := s.GetRecoveryDirective(context.Background(), &GetRecoveryDirectiveInput{ClusterName: "", InstanceID: "i-0"}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = s.GetRecoveryDirective(context.Background(), &GetRecoveryDirectiveInput{ClusterName: "alpha", InstanceID: ""}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue)
}
