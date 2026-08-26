package handlers_acm

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupACMStore returns a Store wired with a master key, mirroring how
// NewACMServiceImplWithNATS constructs one for the ACM service. A Store is
// never unkeyed, so there is no plain variant.
func setupACMStore(t *testing.T) *Store {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)
	store, err := NewStore(t.Context(), nc, masterKey)
	require.NoError(t, err)
	return store
}

// rawKV returns the underlying bucket so a test can assert on the bytes as
// stored. Reading through Store would decode them, which is the one thing
// these tests must not do.
func rawKV(t *testing.T, s *Store) jetstream.KeyValue {
	t.Helper()
	kv, err := s.store.KV(t.Context())
	require.NoError(t, err)
	return kv
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

// TestAddInUseBy_ConcurrentDistinctResourcesAllSurvive is the correctness
// regression test for the InUseBy index: it asserts final state, not merely
// "no error/panic". A plain get-modify-put loses concurrent writers silently
// — every goroutine reads the same starting record, mutates its own copy,
// and the last Put wins, discarding the others' additions with no error
// raised anywhere. That failure mode is exactly what makes this bug
// dangerous: the record still looks well-formed, just short an entry, and
// the missing load balancer never gets a fan-out re-render when its
// certificate is renewed — it silently expires in HAProxy while ACM reports
// a healthy, renewed certificate. Only asserting the exact final set
// catches that; asserting "no errors" does not.
func TestAddInUseBy_ConcurrentDistinctResourcesAllSurvive(t *testing.T) {
	store := setupACMStore(t)
	certArn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/inuse-concurrent"
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{CertificateArn: certArn, AccountID: testAccountID}))

	const n = 20
	want := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		want[i] = fmt.Sprintf("arn:lb/concurrent-%02d", i)
		wg.Add(1)
		go func(resourceArn string) {
			defer wg.Done()
			if err := store.AddInUseBy(t.Context(), certArn, resourceArn); err != nil {
				errs <- err
			}
		}(want[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("AddInUseBy failed: %v", err)
	}

	got, err := store.GetCert(t.Context(), certArn)
	require.NoError(t, err)
	assert.ElementsMatch(t, want, got.InUseBy, "every concurrent AddInUseBy call must survive, none silently lost")
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

func TestStore_PutCert_EncryptsPrivateKeyAtRest(t *testing.T) {
	store := setupACMStore(t)
	const plaintext = "-----BEGIN EC PRIVATE KEY-----\nZm9v\n-----END EC PRIVATE KEY-----\n"
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/enc"
	rec := &CertRecord{CertificateArn: arn, AccountID: "000000000001", PrivateKey: plaintext}
	require.NoError(t, store.PutCert(t.Context(), rec))

	// The raw bytes landed in KV must not contain the plaintext key.
	entry, err := rawKV(t, store).Get(t.Context(), certKey(arn))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "BEGIN EC PRIVATE KEY")

	// GetCert decrypts back to the original plaintext.
	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got.PrivateKey)

	// PutCert must not mutate the caller's record in place.
	assert.Equal(t, plaintext, rec.PrivateKey)
}

func TestStore_GetCert_LegacyPlaintextPassthroughAndReencrypt(t *testing.T) {
	store := setupACMStore(t)
	const plaintext = "-----BEGIN RSA PRIVATE KEY-----\nYmFy\n-----END RSA PRIVATE KEY-----\n"
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/legacy"

	// Simulate a pre-encryption record written before this Store ever had a
	// master key: plaintext PEM landed directly in KV via json.Marshal, the
	// same shape PutCert produced before this change.
	legacy := &CertRecord{CertificateArn: arn, AccountID: "000000000001", PrivateKey: plaintext}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	_, err = rawKV(t, store).Put(t.Context(), certKey(arn), data)
	require.NoError(t, err)

	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got.PrivateKey, "legacy plaintext record must still be readable with no migration step")

	// The next write re-encrypts it — no operator action required.
	require.NoError(t, store.PutCert(t.Context(), got))
	entry, err := rawKV(t, store).Get(t.Context(), certKey(arn))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "BEGIN RSA PRIVATE KEY", "record must be re-encrypted after the next PutCert")
}

// TestStore_GetCert_RejectsGarbageAsLegacyPlaintext is the downgrade-vector
// guard: a value that decrypts under no key and does not unambiguously parse
// as a PEM private key block must be a hard error, never silently trusted as
// legacy plaintext.
func TestStore_GetCert_RejectsGarbageAsLegacyPlaintext(t *testing.T) {
	store := setupACMStore(t)
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/garbage"

	garbage := &CertRecord{CertificateArn: arn, AccountID: "000000000001", PrivateKey: "not-pem-and-not-ciphertext"}
	data, err := json.Marshal(garbage)
	require.NoError(t, err)
	_, err = rawKV(t, store).Put(t.Context(), certKey(arn), data)
	require.NoError(t, err)

	_, err = store.GetCert(t.Context(), arn)
	require.Error(t, err, "garbage that is neither valid ciphertext nor PEM must not be silently treated as plaintext")
}

// TestNewStore_RequiresMasterKey guards against a Store ever being
// constructed unkeyed: a keyed writer and an unkeyed reader of the same
// bucket would disagree silently, with the reader getting ciphertext where
// it expects PEM.
func TestNewStore_RequiresMasterKey(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := NewStore(t.Context(), nc, nil)
	require.Error(t, err)
}

// TestStore_PutGetCert_RoundTripsManagedIssuanceFields asserts the
// RequestCertificate-era fields on CertRecord — validation state, the
// delegation token, and the resumable ACME/lease state — survive a
// PutCert/GetCert round trip unchanged, alongside the encrypted PrivateKey.
func TestStore_PutGetCert_RoundTripsManagedIssuanceFields(t *testing.T) {
	store := setupACMStore(t)
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/managed-1"
	rec := &CertRecord{
		CertificateArn:   arn,
		AccountID:        testAccountID,
		DomainName:       "managed.example.com",
		Type:             "AMAZON_ISSUED",
		Status:           "PENDING_VALIDATION",
		ValidationMethod: ValidationModeManualTXT,
		DomainValidationOptions: []DomainValidationEntry{
			{
				DomainName:       "managed.example.com",
				RecordType:       "TXT",
				RecordName:       "_acme-challenge.managed.example.com.",
				ValidationStatus: "PENDING_VALIDATION",
			},
		},
		RenewalEligibility: "INELIGIBLE",
		DelegationToken:    "delegation-token-abc",
		ACMEOrderURL:       "https://acme.example/order/1",
		LeaseHolder:        "node-a",
	}
	require.NoError(t, store.PutCert(t.Context(), rec))

	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rec.ValidationMethod, got.ValidationMethod)
	assert.Equal(t, rec.DomainValidationOptions, got.DomainValidationOptions)
	assert.Equal(t, rec.DelegationToken, got.DelegationToken, "the delegation token must be ARN-stable across reads")
	assert.Equal(t, rec.ACMEOrderURL, got.ACMEOrderURL)
	assert.Equal(t, rec.LeaseHolder, got.LeaseHolder)
	assert.Equal(t, rec.RenewalEligibility, got.RenewalEligibility)
}

// TestListAllCertMetadata_DoesNotDecryptPrivateKey is the renewal worker's
// scan path (Worker.scanOnce calls this directly): it must return metadata
// without ever touching AES-GCM or materialising plaintext key PEM, because
// the renewal predicate never reads PrivateKey and the scan runs on every
// tick for every certificate in the store.
func TestListAllCertMetadata_DoesNotDecryptPrivateKey(t *testing.T) {
	store := setupACMStore(t)
	const plaintext = "-----BEGIN EC PRIVATE KEY-----\nZm9v\n-----END EC PRIVATE KEY-----\n"
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/metadata-only"
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: arn, AccountID: testAccountID, DomainName: "metadata.example.com", PrivateKey: plaintext,
	}))

	recs, err := store.ListAllCertMetadata(t.Context())
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "metadata.example.com", recs[0].DomainName, "metadata fields must still be populated")
	assert.Empty(t, recs[0].PrivateKey, "ListAllCertMetadata must not return the plaintext key")
	assert.NotContains(t, recs[0].PrivateKey, "BEGIN EC PRIVATE KEY")

	// And GetCert on the same record still decrypts fine — proves the scan
	// path never touched (or corrupted) the stored ciphertext.
	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got.PrivateKey)
}

// TestListAllCertMetadata_CrossesAccounts asserts the renewal worker's scan
// sees every account's certificates, unlike the account-scoped ListCerts used
// by the ListCertificates API.
func TestListAllCertMetadata_CrossesAccounts(t *testing.T) {
	store := setupACMStore(t)
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: "arn:aws:acm:ap-southeast-2:1:certificate/acct-a", AccountID: "000000000001",
	}))
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: "arn:aws:acm:ap-southeast-2:2:certificate/acct-b", AccountID: "000000000002",
	}))

	recs, err := store.ListAllCertMetadata(t.Context())
	require.NoError(t, err)
	assert.Len(t, recs, 2)
}

// TestAcquireLease_LeavesPrivateKeyEncrypted mirrors
// TestAddInUseBy_LeavesPrivateKeyEncrypted: AcquireLease is the same
// raw-CAS-write shape (get, mutate a couple of fields, marshal, CAS Update),
// and that shape has already produced one silent-plaintext-write bug in this
// package, so every write path over CertRecord gets this guard.
func TestAcquireLease_LeavesPrivateKeyEncrypted(t *testing.T) {
	store := setupACMStore(t)
	const plaintext = "-----BEGIN EC PRIVATE KEY-----\nZm9v\n-----END EC PRIVATE KEY-----\n"
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/lease-encrypted"
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: arn, AccountID: testAccountID, PrivateKey: plaintext,
	}))

	acquired, err := store.AcquireLease(t.Context(), arn, "node-a", 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.True(t, acquired)

	entry, err := rawKV(t, store).Get(t.Context(), certKey(arn))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "BEGIN EC PRIVATE KEY",
		"AcquireLease must not write the private key back in plaintext")

	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got.PrivateKey, "the key must still decrypt after a lease acquire")
	assert.Equal(t, "node-a", got.LeaseHolder)
}

// TestReleaseLease_LeavesPrivateKeyEncrypted is AcquireLease's counterpart
// for the release path — same raw-CAS shape, same guard.
func TestReleaseLease_LeavesPrivateKeyEncrypted(t *testing.T) {
	store := setupACMStore(t)
	const plaintext = "-----BEGIN EC PRIVATE KEY-----\nZm9v\n-----END EC PRIVATE KEY-----\n"
	arn := "arn:aws:acm:ap-southeast-2:000000000001:certificate/lease-release-encrypted"
	require.NoError(t, store.PutCert(t.Context(), &CertRecord{
		CertificateArn: arn, AccountID: testAccountID, PrivateKey: plaintext,
	}))
	acquired, err := store.AcquireLease(t.Context(), arn, "node-a", 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.True(t, acquired)

	require.NoError(t, store.ReleaseLease(t.Context(), arn, "node-a"))

	entry, err := rawKV(t, store).Get(t.Context(), certKey(arn))
	require.NoError(t, err)
	assert.NotContains(t, string(entry.Value()), "BEGIN EC PRIVATE KEY",
		"ReleaseLease must not write the private key back in plaintext")

	got, err := store.GetCert(t.Context(), arn)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got.PrivateKey, "the key must still decrypt after a lease release")
	assert.Empty(t, got.LeaseHolder, "a released lease must clear the holder")
}
