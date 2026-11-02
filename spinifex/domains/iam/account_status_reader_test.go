package iam_test

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStatusReaderFixture starts a real IAM service (which creates and
// migrates spinifex-accounts) and a reader attached to the same JetStream.
func newStatusReaderFixture(t *testing.T) (iamdomain.IAMService, *iamdomain.AccountStatusReader, jetstream.JetStream) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)

	return svc, iamdomain.NewAccountStatusReader(js), js
}

func TestAccountStatusReader_TerminatingIsTrue(t *testing.T) {
	t.Parallel()
	svc, reader, _ := newStatusReaderFixture(t)
	// A fresh service hands out 000000000001 first, an ID IAM never moves off ACTIVE.
	_, err := svc.CreateAccount("reserved-admin")
	require.NoError(t, err)
	account, err := svc.CreateAccount("tenant@example.com")
	require.NoError(t, err)
	_, err = svc.SetAccountStatus(account.AccountID, iamdomain.AccountStatusTerminating)
	require.NoError(t, err)

	terminating, err := reader.AccountTerminating(t.Context(), account.AccountID)
	require.NoError(t, err)
	assert.True(t, terminating)
}

func TestAccountStatusReader_ActiveAndSuspendedAreFalse(t *testing.T) {
	t.Parallel()
	svc, reader, _ := newStatusReaderFixture(t)
	active, err := svc.CreateAccount("active@example.com")
	require.NoError(t, err)
	terminating, err := reader.AccountTerminating(t.Context(), active.AccountID)
	require.NoError(t, err)
	assert.False(t, terminating)

	suspended, err := svc.CreateAccount("suspended@example.com")
	require.NoError(t, err)
	_, err = svc.SetAccountStatus(suspended.AccountID, iamdomain.AccountStatusSuspended)
	require.NoError(t, err)
	terminating, err = reader.AccountTerminating(t.Context(), suspended.AccountID)
	require.NoError(t, err)
	assert.False(t, terminating)
}

func TestAccountStatusReader_MissingRecordIsFalseNotError(t *testing.T) {
	t.Parallel()
	_, reader, _ := newStatusReaderFixture(t)
	terminating, err := reader.AccountTerminating(t.Context(), "999999999999")
	require.NoError(t, err)
	assert.False(t, terminating)
}

// A missing bucket is an error, and the reader never creates one: a caller
// that only reads account status must not bootstrap IAM's bucket.
func TestAccountStatusReader_MissingBucketIsErrorAndNeverCreated(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	reader := iamdomain.NewAccountStatusReader(js)

	_, err := reader.AccountTerminating(t.Context(), "111111111111")
	require.Error(t, err)

	_, bucketErr := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	assert.ErrorIs(t, bucketErr, jetstream.ErrBucketNotFound)
	_ = nc
}

func TestAccountStatusReader_UndecodableValueIsError(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	_, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, mustMasterKey(t))
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), "222222222222", []byte(`{"status":`))
	require.NoError(t, err)

	reader := iamdomain.NewAccountStatusReader(js)
	_, err = reader.AccountTerminating(t.Context(), "222222222222")
	require.Error(t, err)
}

func mustMasterKey(t *testing.T) []byte {
	t.Helper()
	key, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	return key
}
