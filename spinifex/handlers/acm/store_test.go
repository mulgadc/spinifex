package handlers_acm

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupACMStore(t *testing.T) *Store {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := NewStore(t.Context(), nc)
	require.NoError(t, err)
	return store
}

func TestAddInUseBy_AddsAndDedupes(t *testing.T) {
	store := setupACMStore(t)
	rec := &CertRecord{CertificateArn: "arn:aws:acm:ap-southeast-2:000000000001:certificate/inuse-1", AccountID: testAccountID}
	require.NoError(t, store.PutCert(t.Context(), rec))

	require.NoError(t, store.AddInUseBy(t.Context(), rec.CertificateArn, "arn:lb/one"))
	got, err := store.GetCert(t.Context(), rec.CertificateArn)
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:lb/one"}, got.InUseBy)

	// Adding the same LB again is a no-op, not a duplicate entry.
	require.NoError(t, store.AddInUseBy(t.Context(), rec.CertificateArn, "arn:lb/one"))
	got, err = store.GetCert(t.Context(), rec.CertificateArn)
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:lb/one"}, got.InUseBy)

	// A second, distinct LB is appended.
	require.NoError(t, store.AddInUseBy(t.Context(), rec.CertificateArn, "arn:lb/two"))
	got, err = store.GetCert(t.Context(), rec.CertificateArn)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"arn:lb/one", "arn:lb/two"}, got.InUseBy)
}

func TestAddInUseBy_NoopOnMissingCert(t *testing.T) {
	store := setupACMStore(t)
	// No cert exists under this ARN — must not error and must not create one.
	require.NoError(t, store.AddInUseBy(t.Context(), "arn:aws:acm:ap-southeast-2:000000000001:certificate/missing", "arn:lb/one"))

	got, err := store.GetCert(t.Context(), "arn:aws:acm:ap-southeast-2:000000000001:certificate/missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRemoveInUseBy_RemovesEntry(t *testing.T) {
	store := setupACMStore(t)
	rec := &CertRecord{
		CertificateArn: "arn:aws:acm:ap-southeast-2:000000000001:certificate/inuse-2",
		AccountID:      testAccountID,
		InUseBy:        []string{"arn:lb/one", "arn:lb/two"},
	}
	require.NoError(t, store.PutCert(t.Context(), rec))

	require.NoError(t, store.RemoveInUseBy(t.Context(), rec.CertificateArn, "arn:lb/one"))
	got, err := store.GetCert(t.Context(), rec.CertificateArn)
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:lb/two"}, got.InUseBy)
}

func TestRemoveInUseBy_NoopWhenAbsentOrMissingCert(t *testing.T) {
	store := setupACMStore(t)
	rec := &CertRecord{
		CertificateArn: "arn:aws:acm:ap-southeast-2:000000000001:certificate/inuse-3",
		AccountID:      testAccountID,
		InUseBy:        []string{"arn:lb/one"},
	}
	require.NoError(t, store.PutCert(t.Context(), rec))

	// Removing an LB that was never in the set is a no-op.
	require.NoError(t, store.RemoveInUseBy(t.Context(), rec.CertificateArn, "arn:lb/never-there"))
	got, err := store.GetCert(t.Context(), rec.CertificateArn)
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:lb/one"}, got.InUseBy)

	// Removing against a nonexistent cert must not error.
	require.NoError(t, store.RemoveInUseBy(t.Context(), "arn:aws:acm:ap-southeast-2:000000000001:certificate/missing", "arn:lb/one"))
}
