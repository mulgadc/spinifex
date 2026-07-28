package handlers_acm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Proportional renewal window ------------------------------------------

// TestRenewable_ProportionalWindow exercises the four cases the renewal
// predicate must get right: not yet due, exactly at the threshold, overdue,
// and a lifetime short enough that the two-thirds fraction is floored.
// jitter is computed from the fixed test ARN so the boundary cases land
// exactly on renewalDueAt rather than depending on it landing on a whole
// unit of time.
func TestRenewable_ProportionalWindow(t *testing.T) {
	const arn = "arn:aws:acm:ap-southeast-2:1:certificate/window-test"
	jitter := renewalJitter(arn)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newRec := func(lifetime, elapsedBeforeNow time.Duration) *CertRecord {
		notBefore := now.Add(-elapsedBeforeNow)
		return &CertRecord{
			CertificateArn:     arn,
			Type:               acm.CertificateTypePrivate,
			Status:             certStatusIssued,
			RenewalEligibility: acm.RenewalEligibilityEligible,
			NotBefore:          notBefore,
			NotAfter:           notBefore.Add(lifetime),
		}
	}

	t.Run("not yet due", func(t *testing.T) {
		lifetime := 90 * 24 * time.Hour
		window := renewalWindow(lifetime)
		rec := newRec(lifetime, window+jitter-time.Minute)
		assert.False(t, renewable(rec, now))
	})

	t.Run("exactly at threshold", func(t *testing.T) {
		lifetime := 90 * 24 * time.Hour
		window := renewalWindow(lifetime)
		rec := newRec(lifetime, window+jitter)
		assert.True(t, renewable(rec, now))
	})

	t.Run("overdue", func(t *testing.T) {
		lifetime := 90 * 24 * time.Hour
		window := renewalWindow(lifetime)
		rec := newRec(lifetime, window+jitter+24*time.Hour)
		assert.True(t, renewable(rec, now))
	})

	t.Run("very short lifetime hits the floor", func(t *testing.T) {
		lifetime := 10 * time.Minute
		window := renewalWindow(lifetime)
		require.Equal(t, minRenewalWindow, window,
			"a 10-minute lifetime's raw two-thirds fraction (~6m40s) must be floored to minRenewalWindow")

		// Past the raw 2/3 fraction (~6m40s elapsed) but short of the floor:
		// must not be renewable yet, or the floor would be pointless.
		notDue := newRec(lifetime, 7*time.Minute)
		assert.False(t, renewable(notDue, now))

		due := newRec(lifetime, window+jitter)
		assert.True(t, renewable(due, now))
	})
}

// TestRenewable_GatesOnTypeStatusEligibility asserts every non-window gate:
// only a PRIVATE, ISSUED, ELIGIBLE certificate is ever renewable, regardless
// of how overdue it is.
func TestRenewable_GatesOnTypeStatusEligibility(t *testing.T) {
	now := time.Now()
	overdue := func() *CertRecord {
		return &CertRecord{
			CertificateArn:     "arn:aws:acm:ap-southeast-2:1:certificate/gate-test",
			Type:               acm.CertificateTypePrivate,
			Status:             certStatusIssued,
			RenewalEligibility: acm.RenewalEligibilityEligible,
			NotBefore:          now.Add(-365 * 24 * time.Hour),
			NotAfter:           now.Add(-time.Hour),
		}
	}

	tests := []struct {
		name   string
		mutate func(*CertRecord)
	}{
		{"imported certificate", func(r *CertRecord) { r.Type = certTypeImported }},
		{"amazon-issued (public) certificate", func(r *CertRecord) { r.Type = acm.CertificateTypeAmazonIssued }},
		{"pending validation status", func(r *CertRecord) { r.Status = acm.CertificateStatusPendingValidation }},
		{"renewal ineligible", func(r *CertRecord) { r.RenewalEligibility = acm.RenewalEligibilityIneligible }},
		{"eligibility unset (legacy record)", func(r *CertRecord) { r.RenewalEligibility = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := overdue()
			tt.mutate(rec)
			assert.False(t, renewable(rec, now))
		})
	}
}

// --- Deterministic per-ARN jitter -------------------------------------------

func TestRenewalJitter_DeterministicPerARN(t *testing.T) {
	const arn = "arn:aws:acm:ap-southeast-2:1:certificate/jitter-test"
	first := renewalJitter(arn)
	second := renewalJitter(arn)
	assert.Equal(t, first, second, "the same ARN must yield the same offset on every call, including across restarts")
	assert.GreaterOrEqual(t, first, time.Duration(0))
	assert.Less(t, first, renewalJitterSpread)
}

func TestRenewalJitter_SpreadsAcrossDistinctARNs(t *testing.T) {
	offsets := make(map[time.Duration]bool)
	for i := range 8 {
		arn := fmt.Sprintf("arn:aws:acm:ap-southeast-2:1:certificate/cert-%d", i)
		offsets[renewalJitter(arn)] = true
	}
	assert.Greater(t, len(offsets), 1, "eight distinct ARNs issued together must not all collapse onto the same offset")
}

// --- Worker: renews the right certificates, leaves the rest alone ----------

// backdateToOverdue rewrites certArn's NotBefore/NotAfter so it is well past
// its renewal window, without waiting for real time to pass.
func backdateToOverdue(t *testing.T, svc *ACMServiceImpl, certArn string) {
	t.Helper()
	rec, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	rec.NotBefore = time.Now().Add(-100 * 24 * time.Hour)
	rec.NotAfter = rec.NotBefore.Add(90 * 24 * time.Hour)
	require.NoError(t, svc.store.PutCert(context.Background(), rec))
}

// TestWorker_RenewOne_PreservesIdentityChangesKeyAndSerial is the core
// renewal contract: same ARN, same domain and SANs, fresh key and serial.
func TestWorker_RenewOne_PreservesIdentityChangesKeyAndSerial(t *testing.T) {
	svc := setupACMService(t)
	svc.TenantCA = &fakeCertAuthority{permitted: map[string]bool{
		"renew.example.com":     true,
		"www.renew.example.com": true,
	}}

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName:              aws.String("renew.example.com"),
		SubjectAlternativeNames: aws.StringSlice([]string{"www.renew.example.com"}),
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)
	backdateToOverdue(t, svc, certArn)

	before, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)

	worker := NewWorker(svc, "node-1")
	acquired, err := worker.renewWithLease(context.Background(), certArn)
	require.NoError(t, err)
	require.True(t, acquired)

	after, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)

	assert.Equal(t, certArn, after.CertificateArn)
	assert.Equal(t, "renew.example.com", after.DomainName)
	assert.ElementsMatch(t, []string{"www.renew.example.com"}, after.SubjectAltNames)
	assert.NotEqual(t, before.Serial, after.Serial, "renewal must mint a fresh serial")
	assert.NotEqual(t, before.PrivateKey, after.PrivateKey, "renewal must mint a fresh leaf key")
	assert.True(t, after.NotAfter.After(before.NotAfter), "the renewed certificate must have a later expiry")
	require.NotNil(t, after.RenewalSummary)
	assert.Equal(t, acm.RenewalStatusSuccess, after.RenewalSummary.RenewalStatus)
}

// TestWorker_CertMaterialUpdated_InvokedExactlyOnceOnSuccess asserts the
// ELBv2 fan-out hook fires exactly once per successful renewal — the
// mandatory push described in the ACM design doc's ELBv2 propagation
// section.
func TestWorker_CertMaterialUpdated_InvokedExactlyOnceOnSuccess(t *testing.T) {
	svc := setupACMService(t)
	svc.TenantCA = &fakeCertAuthority{permitted: map[string]bool{"fanout.example.com": true}}
	var calls int
	var gotArn string
	svc.CertMaterialUpdated = func(_ context.Context, certArn string) error {
		calls++
		gotArn = certArn
		return nil
	}

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("fanout.example.com"),
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)
	backdateToOverdue(t, svc, certArn)

	worker := NewWorker(svc, "node-1")
	acquired, err := worker.renewWithLease(context.Background(), certArn)
	require.NoError(t, err)
	require.True(t, acquired)

	assert.Equal(t, 1, calls, "CertMaterialUpdated must be invoked exactly once on a successful renewal")
	assert.Equal(t, certArn, gotArn)
}

// TestWorker_ScanOnce_SkipsIneligibleAndImported_RenewsEligible drives a full
// scan over a mix of certificate shapes and asserts only the eligible,
// overdue PRIVATE_CA certificate is renewed.
func TestWorker_ScanOnce_SkipsIneligibleAndImported_RenewsEligible(t *testing.T) {
	svc := setupACMService(t)
	svc.TenantCA = &fakeCertAuthority{permitted: map[string]bool{
		"eligible.example.com": true,
	}}
	var renewedArns []string
	svc.CertMaterialUpdated = func(_ context.Context, certArn string) error {
		renewedArns = append(renewedArns, certArn)
		return nil
	}

	// MANUAL_TXT: INELIGIBLE by construction (challenge token rotates per order).
	svc.NorthstarHostsZone = func(string) bool { return true }
	manualOut, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("manual-skip.example.com"),
	}, testAccountID)
	require.NoError(t, err)

	// Imported: also INELIGIBLE, and Type IMPORTED regardless.
	c, k := genCert(t, "imported-skip.example.com")
	impOut, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c, PrivateKey: k}, testAccountID)
	require.NoError(t, err)

	// Eligible PRIVATE_CA certificate, backdated past its renewal window.
	svc.NorthstarHostsZone = nil
	eligibleOut, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("eligible.example.com"),
	}, testAccountID)
	require.NoError(t, err)
	eligibleArn := aws.StringValue(eligibleOut.CertificateArn)
	backdateToOverdue(t, svc, eligibleArn)

	worker := NewWorker(svc, "node-1")
	worker.scanOnce(context.Background())

	assert.Equal(t, []string{eligibleArn}, renewedArns,
		"only the eligible, overdue PRIVATE_CA certificate may be renewed")

	manualRec, err := svc.store.GetCert(context.Background(), aws.StringValue(manualOut.CertificateArn))
	require.NoError(t, err)
	assert.Nil(t, manualRec.RenewalSummary, "an ineligible certificate must never be touched by the renewal worker")

	impRec, err := svc.store.GetCert(context.Background(), aws.StringValue(impOut.CertificateArn))
	require.NoError(t, err)
	assert.Nil(t, impRec.RenewalSummary, "an imported certificate must never be touched by the renewal worker")
}

// TestWorker_ScanOnce_DomainOutsideConstraints_RecordsFailureAndBacksOff
// simulates an operator regenerating the tenant CA with a narrower domain
// list after a certificate was issued: renewal must record the failure on
// RenewalSummary (the sole DescribeCertificate diagnostic surface) and must
// not re-attempt on the very next scan.
func TestWorker_ScanOnce_DomainOutsideConstraints_RecordsFailureAndBacksOff(t *testing.T) {
	svc := setupACMService(t)
	ca := &fakeCertAuthority{permitted: map[string]bool{"narrowed.example.com": true}}
	svc.TenantCA = ca

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("narrowed.example.com"),
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)
	backdateToOverdue(t, svc, certArn)

	// The root now excludes narrowed.example.com.
	ca.permitted = map[string]bool{}

	fixedNow := time.Now()
	worker := NewWorker(svc, "node-1", WithRenewalClock(func() time.Time { return fixedNow }))
	worker.scanOnce(context.Background())

	rec, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	require.NotNil(t, rec.RenewalSummary)
	assert.Equal(t, acm.RenewalStatusFailed, rec.RenewalSummary.RenewalStatus)
	assert.Equal(t, acm.FailureReasonPcaNameConstraintsValidation, rec.RenewalSummary.RenewalStatusReason)
	firstUpdatedAt := rec.RenewalSummary.UpdatedAt

	// Same fixed clock, so a second scan lands inside the failure backoff
	// window — it must be a no-op, not a second attempt.
	worker.scanOnce(context.Background())
	rec2, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	assert.Equal(t, firstUpdatedAt, rec2.RenewalSummary.UpdatedAt,
		"a scan inside the failure backoff window must not re-attempt renewal")
}

// TestWorker_LeaseContention_OnlyOneNodeRenews asserts the per-certificate
// lease actually excludes a second worker: node-a's live lease must stop
// node-b's worker from reissuing the same certificate underneath it.
func TestWorker_LeaseContention_OnlyOneNodeRenews(t *testing.T) {
	svc := setupACMService(t)
	svc.TenantCA = &fakeCertAuthority{permitted: map[string]bool{"contended.example.com": true}}

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("contended.example.com"),
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)
	backdateToOverdue(t, svc, certArn)

	before, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)

	acquired, err := svc.store.AcquireLease(context.Background(), certArn, "node-a", 5*time.Minute, time.Now())
	require.NoError(t, err)
	require.True(t, acquired, "node-a must win the lease on an unheld certificate")

	workerB := NewWorker(svc, "node-b")
	won, err := workerB.renewWithLease(context.Background(), certArn)
	require.NoError(t, err)
	assert.False(t, won, "node-b must not win a lease node-a already holds live")

	after, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	assert.Equal(t, before.Serial, after.Serial, "the certificate must not be reissued while another node's lease is live")
}

// --- Operator-triggered forced renewal --------------------------------------

// TestForceRenewCertificate_BypassesRenewalWindow is the live-verification
// path: a freshly issued certificate, nowhere near its renewal window, must
// still reissue on demand.
func TestForceRenewCertificate_BypassesRenewalWindow(t *testing.T) {
	svc := setupACMService(t)
	svc.TenantCA = &fakeCertAuthority{permitted: map[string]bool{"force.example.com": true}}

	out, err := svc.RequestCertificate(context.Background(), &acm.RequestCertificateInput{
		DomainName: aws.String("force.example.com"),
	}, testAccountID)
	require.NoError(t, err)
	certArn := aws.StringValue(out.CertificateArn)

	before, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	require.False(t, renewable(before, time.Now()), "a freshly issued certificate must not be naturally due")

	worker := NewWorker(svc, "node-1")
	result, err := worker.ForceRenewCertificate(context.Background(), &ForceRenewCertificateInput{CertificateArn: certArn}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, certArn, result.CertificateArn)

	after, err := svc.store.GetCert(context.Background(), certArn)
	require.NoError(t, err)
	assert.NotEqual(t, before.Serial, after.Serial, "force-renew must reissue even though the certificate is not yet due")
}

// TestForceRenewCertificate_RejectsNonPrivateType asserts force-renew refuses
// an imported certificate rather than silently no-op'ing or crashing — this
// worker only ever drives PRIVATE_CA reissuance.
func TestForceRenewCertificate_RejectsNonPrivateType(t *testing.T) {
	svc := setupACMService(t)
	c, k := genCert(t, "force-import.example.com")
	out, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{Certificate: c, PrivateKey: k}, testAccountID)
	require.NoError(t, err)

	worker := NewWorker(svc, "node-1")
	_, err = worker.ForceRenewCertificate(context.Background(), &ForceRenewCertificateInput{CertificateArn: aws.StringValue(out.CertificateArn)}, testAccountID)
	require.Error(t, err)
}

// TestForceRenewCertificate_UnknownArnRejected mirrors DescribeCertificate's
// ownership check: an ARN that does not exist, or belongs to another
// account, must be rejected rather than disclosing anything about it.
func TestForceRenewCertificate_UnknownArnRejected(t *testing.T) {
	svc := setupACMService(t)
	worker := NewWorker(svc, "node-1")
	_, err := worker.ForceRenewCertificate(context.Background(), &ForceRenewCertificateInput{
		CertificateArn: "arn:aws:acm:ap-southeast-2:1:certificate/does-not-exist",
	}, testAccountID)
	require.Error(t, err)
}
