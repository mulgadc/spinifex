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
	_, _, js := testutil.StartTestJetStream(t)
	acctKV, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)

	d, err := LoadRecoveryDirective(acctKV, "alpha", "i-missing")
	require.NoError(t, err)
	assert.Equal(t, int64(0), d.Epoch, "absent directive reads as epoch 0")
	assert.Equal(t, RecoveryActionNone, d.Action, "absent directive is a none action")
}

func TestStoreRecoveryDirective_EpochIncrements(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	acctKV, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)

	first, err := StoreRecoveryDirective(acctKV, "alpha", "i-0", RecoveryActionClusterReset, "")
	require.NoError(t, err)
	assert.Equal(t, int64(1), first.Epoch, "first set starts at epoch 1")

	second, err := StoreRecoveryDirective(acctKV, "alpha", "i-0", RecoveryActionWipeRejoin, "snap.snap")
	require.NoError(t, err)
	assert.Equal(t, int64(2), second.Epoch, "each set advances the epoch")

	got, err := LoadRecoveryDirective(acctKV, "alpha", "i-0")
	require.NoError(t, err)
	assert.Equal(t, RecoveryActionWipeRejoin, got.Action)
	assert.Equal(t, "snap.snap", got.Snapshot)
	assert.Equal(t, int64(2), got.Epoch)
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
