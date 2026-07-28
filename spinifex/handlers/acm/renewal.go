package handlers_acm

// renewal.go implements the PRIVATE_CA renewal worker described in the ACM
// design doc's "Renewal" section: a scan-and-lease worker, sharing the
// per-certificate lease pattern reserved on CertRecord, that reissues a
// tenant-CA-signed certificate under its existing ARN once it crosses a
// proportional fraction of its lifetime. Re-signing from the tenant CA is
// fully offline, so this worker never touches ACME, lego, or the network —
// unlike PROVIDER_API/MANUAL_TXT renewal, which is deferred along with the
// rest of public issuance.

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	// renewalLifetimeFraction is the proportion of a certificate's total
	// lifetime (NotAfter - NotBefore) that must elapse before renewal is due.
	// Proportional rather than a fixed calendar window: a 30-day window is
	// correct for a 90-day certificate and wrong for a short-lived one, where
	// it degenerates into "renew immediately, forever". Two-thirds self-tunes
	// to whatever lifetime a certificate actually carries.
	renewalLifetimeFraction = 2.0 / 3.0

	// minRenewalWindow floors the computed window so a very short-lived
	// certificate does not renew in a near-constant loop — two-thirds of a
	// ten-minute lifetime is under seven minutes, which is thrash, not
	// maintenance.
	minRenewalWindow = 1 * time.Hour

	// renewalJitterSpread bounds the deterministic per-ARN offset added on
	// top of the renewal window, so certificates minted together in one
	// `apply` do not all become due in the same scan tick a lifetime later.
	renewalJitterSpread = 10 * time.Minute

	// renewalFailureBackoff is the minimum time between renewal attempts for
	// a certificate whose most recent attempt failed. A certificate outside
	// the tenant CA's name constraints, or issued while no tenant CA was
	// wired, cannot be fixed by retrying — the fix is an operator action
	// (regenerate the CA, or create one) — so retrying every scan tick would
	// just spin. An hour, well over the default scan interval, still notices
	// promptly once the operator has actually fixed it.
	renewalFailureBackoff = 1 * time.Hour

	// defaultRenewalScanInterval is how often the worker looks for
	// certificates past their renewal window.
	defaultRenewalScanInterval = 5 * time.Minute

	// defaultRenewalLeaseTTL bounds how long a per-certificate lease survives
	// without being released, so a holder that crashes mid-renewal does not
	// block that certificate forever — the next scan (here or on another
	// node) reclaims it once the TTL passes.
	defaultRenewalLeaseTTL = 2 * time.Minute
)

// renewalWindow returns the elapsed-lifetime duration after which a
// certificate with the given total lifetime becomes due for renewal:
// two-thirds of lifetime, floored at minRenewalWindow.
func renewalWindow(lifetime time.Duration) time.Duration {
	window := time.Duration(float64(lifetime) * renewalLifetimeFraction)
	if window < minRenewalWindow {
		return minRenewalWindow
	}
	return window
}

// renewalJitter derives a deterministic offset in [0, renewalJitterSpread)
// from certArn by hashing it with FNV-1a. Deterministic, not random: the same
// ARN yields the same offset on every call, including across process
// restarts, while distinct ARNs spread across the jitter range instead of
// all becoming due on the same scan tick.
func renewalJitter(certArn string) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write([]byte(certArn)) // hash.Hash.Write never errors
	return time.Duration(h.Sum64() % uint64(renewalJitterSpread))
}

// renewalDueAt returns the instant at which rec becomes due for renewal:
// NotBefore, plus the proportional (floored) window, plus this ARN's
// deterministic jitter offset.
func renewalDueAt(rec *CertRecord) time.Time {
	lifetime := rec.NotAfter.Sub(rec.NotBefore)
	return rec.NotBefore.Add(renewalWindow(lifetime)).Add(renewalJitter(rec.CertificateArn))
}

// renewable reports whether rec is a candidate for automatic renewal at now:
// a PRIVATE (tenant-CA-issued) certificate, ISSUED, marked ELIGIBLE, past its
// renewal window, and — if its last attempt failed — past the failure
// backoff. Imported certificates (Type IMPORTED) and MANUAL_TXT-validated
// certificates are never renewable (see renewalEligibilityForMode in
// service_impl.go); AMAZON_ISSUED (PROVIDER_API) renewal is out of scope for
// this worker, which drives PRIVATE_CA reissuance only.
func renewable(rec *CertRecord, now time.Time) bool {
	if rec.Type != acm.CertificateTypePrivate {
		return false
	}
	if rec.Status != certStatusIssued {
		return false
	}
	if rec.RenewalEligibility != acm.RenewalEligibilityEligible {
		return false
	}
	if rec.RenewalSummary != nil && rec.RenewalSummary.RenewalStatus == acm.RenewalStatusFailed &&
		now.Sub(rec.RenewalSummary.UpdatedAt) < renewalFailureBackoff {
		return false
	}
	return !renewalDueAt(rec).After(now)
}

// Worker scans stored certificates for PRIVATE_CA-issued ones past their
// renewal window and reissues them under their existing ARN with a fresh
// leaf key, then pushes the new material to every load balancer that
// references the ARN.
//
// One Worker runs per node (wired alongside svc in the daemon), mirroring the
// issuance-worker model in the ACM design doc: every node is a candidate
// driver, and the per-certificate lease in svc's Store ensures only one of
// them actually renews a given certificate. This costs nothing extra on a
// single-node deployment and needs no leader election.
//
// Worker holds svc rather than a snapshot of its TenantCA/CertMaterialUpdated
// fields so that a tenant CA loaded after the worker starts (the daemon does
// not treat a missing CA as fatal) is picked up on the next scan without a
// restart.
type Worker struct {
	svc      *ACMServiceImpl
	holderID string
	now      func() time.Time

	scanInterval time.Duration
	leaseTTL     time.Duration
}

// WorkerOption tunes a Worker's scan cadence, lease TTL, or clock. All three
// have sane production defaults; options exist for tests (which cannot wait
// out real renewal windows) and are not exposed as config-file or
// environment-variable knobs.
type WorkerOption func(*Worker)

// WithRenewalScanInterval overrides how often the worker scans for
// certificates past their renewal window.
func WithRenewalScanInterval(d time.Duration) WorkerOption {
	return func(w *Worker) { w.scanInterval = d }
}

// WithRenewalLeaseTTL overrides the per-certificate lease TTL.
func WithRenewalLeaseTTL(d time.Duration) WorkerOption {
	return func(w *Worker) { w.leaseTTL = d }
}

// WithRenewalClock overrides the worker's notion of "now", so a test can move
// a certificate across its renewal window without waiting for real time to
// pass.
func WithRenewalClock(now func() time.Time) WorkerOption {
	return func(w *Worker) { w.now = now }
}

// NewWorker builds a renewal worker over svc. holderID identifies this node
// in the per-certificate lease (the daemon passes its node ID, mirroring
// handlers_ecs.NewScheduler's holder identity).
func NewWorker(svc *ACMServiceImpl, holderID string, opts ...WorkerOption) *Worker {
	w := &Worker{
		svc:          svc,
		holderID:     holderID,
		now:          time.Now,
		scanInterval: defaultRenewalScanInterval,
		leaseTTL:     defaultRenewalLeaseTTL,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run scans on WithRenewalScanInterval's cadence until ctx is cancelled. The
// daemon starts one Run per node in its own goroutine and stops it by
// cancelling ctx on shutdown.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.scanInterval)
	defer ticker.Stop()

	// A scan with nothing due logs nothing, so without this an operator cannot
	// distinguish a running worker from one that never started.
	slog.InfoContext(ctx, "ACM renewal worker started", "scan_interval", w.scanInterval)

	w.scanOnce(ctx) // don't wait a full interval before the first pass
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scanOnce(ctx)
		}
	}
}

// scanOnce lists every certificate in the deployment and attempts to renew
// each one renewable at the current time. Per-certificate errors (lease loss,
// CA authorization failure, store errors) are logged and do not stop the
// scan — one bad certificate must not block renewal of the rest.
func (w *Worker) scanOnce(ctx context.Context) {
	if w.svc.TenantCA == nil {
		// No tenant CA wired: nothing this worker can renew. An operator
		// using only public certificates has no reason to own one, so this
		// is an expected steady state, not a fault — logged at Debug.
		slog.DebugContext(ctx, "acm renewal: no tenant CA wired, skipping scan")
		return
	}
	// Metadata only: the predicate below never reads PrivateKey, and the
	// winner is re-fetched (decrypted) via GetCert in renewOne once it has
	// actually won the lease — see ListAllCertMetadata.
	recs, err := w.svc.store.ListAllCertMetadata(ctx)
	if err != nil {
		slog.WarnContext(ctx, "acm renewal: list certificates failed", "err", err)
		return
	}
	now := w.now()
	for _, rec := range recs {
		if !renewable(rec, now) {
			continue
		}
		if _, err := w.renewWithLease(ctx, rec.CertificateArn); err != nil {
			slog.WarnContext(ctx, "acm renewal: renewal attempt failed", "arn", rec.CertificateArn, "err", err)
		}
	}
}

// renewWithLease acquires certArn's per-certificate lease, renews it, and
// releases the lease, all under holderID. acquired reports whether the lease
// was won: false with a nil error means another node currently holds it, a
// case the scheduled scan treats as "someone else has this" (not logged) and
// ForceRenewCertificate treats as an error to report to the caller.
func (w *Worker) renewWithLease(ctx context.Context, certArn string) (acquired bool, err error) {
	acquired, err = w.svc.store.AcquireLease(ctx, certArn, w.holderID, w.leaseTTL, w.now())
	if err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	if !acquired {
		return false, nil
	}
	defer func() {
		if relErr := w.svc.store.ReleaseLease(ctx, certArn, w.holderID); relErr != nil {
			// The TTL reaps it either way; this just means another node
			// waits out the TTL instead of picking it up immediately.
			slog.WarnContext(ctx, "acm renewal: lease release failed (TTL will reap)", "arn", certArn, "err", relErr)
		}
	}()
	return true, w.renewOne(ctx, certArn)
}

// renewOne re-fetches certArn under the caller's lease, verifies it is still
// PRIVATE_CA-authorized, and reissues its leaf from the tenant CA under the
// same ARN, domain and SANs with a fresh key — then pushes the new material
// out via CertMaterialUpdated so every load balancer referencing the ARN
// re-renders. Terminal authorization failures (no tenant CA, or a domain that
// no longer falls within the CA's name constraints — e.g. the operator
// regenerated the root with a narrower domain list) are recorded on
// RenewalSummary via recordRenewalFailure rather than retried immediately.
func (w *Worker) renewOne(ctx context.Context, certArn string) error {
	rec, err := w.svc.store.GetCert(ctx, certArn)
	if err != nil {
		return fmt.Errorf("get cert: %w", err)
	}
	if rec == nil {
		return nil // deleted since the scan listed it; nothing to do
	}

	if w.svc.TenantCA == nil {
		return w.recordRenewalFailure(ctx, rec, acm.FailureReasonOther,
			fmt.Sprintf("no tenant CA wired; run %q", tenantCACreateCommandHint))
	}
	domains := uniqueDomains(rec.DomainName, rec.SubjectAltNames)
	for _, d := range domains {
		if !w.svc.TenantCA.Authorized(d) {
			return w.recordRenewalFailure(ctx, rec, acm.FailureReasonPcaNameConstraintsValidation,
				fmt.Sprintf("domain %q is outside the tenant CA's permitted domains", d))
		}
	}

	certPEM, chainPEM, keyPEM, err := w.svc.TenantCA.IssueLeaf(rec.DomainName, rec.SubjectAltNames)
	if err != nil {
		return w.recordRenewalFailure(ctx, rec, acm.FailureReasonOther, err.Error())
	}
	leaf, err := parseLeaf([]byte(certPEM))
	if err != nil {
		return w.recordRenewalFailure(ctx, rec, acm.FailureReasonOther, "issued leaf was unparseable: "+err.Error())
	}

	// Same ARN, domain and SANs; everything derived from the fresh leaf
	// changes. rec.DomainName/SubjectAltNames are left untouched.
	rec.Certificate = certPEM
	rec.CertificateChain = chainPEM
	rec.PrivateKey = keyPEM
	rec.Serial = leaf.SerialNumber.Text(16)
	rec.Subject = leaf.Subject.String()
	rec.Issuer = leaf.Issuer.String()
	rec.KeyAlgorithm = keyAlgorithm(leaf)
	rec.NotBefore = leaf.NotBefore
	rec.NotAfter = leaf.NotAfter
	rec.RenewalSummary = &RenewalSummaryRecord{
		RenewalStatus: acm.RenewalStatusSuccess,
		UpdatedAt:     w.now(),
	}

	if err := w.svc.store.PutCert(ctx, rec); err != nil {
		return fmt.Errorf("store renewed cert: %w", err)
	}
	slog.InfoContext(ctx, "acm renewal: reissued", "arn", certArn, "domain", rec.DomainName, "not_after", rec.NotAfter)

	if w.svc.CertMaterialUpdated != nil {
		// The store write already succeeded; a fan-out failure is logged,
		// never propagated, mirroring ImportCertificate's re-import fan-out —
		// a rendering problem on one load balancer must not fail the
		// renewal that already succeeded.
		if fanErr := w.svc.CertMaterialUpdated(ctx, certArn); fanErr != nil {
			slog.ErrorContext(ctx, "acm renewal: fan-out to load balancers failed", "arn", certArn, "err", fanErr)
		}
	}
	return nil
}

// recordRenewalFailure persists a FAILED RenewalSummary with reason on rec —
// the sole diagnostic surface DescribeCertificate exposes for a certificate
// that issued cleanly but can no longer renew — and returns an error
// describing the failure for the caller/log. It does not touch
// RenewalEligibility: a certificate that is temporarily outside the CA's
// constraints (an operator can regenerate the CA to cover it again) is still
// eligible in principle, just not renewable right now.
func (w *Worker) recordRenewalFailure(ctx context.Context, rec *CertRecord, reason, detail string) error {
	rec.RenewalSummary = &RenewalSummaryRecord{
		RenewalStatus:       acm.RenewalStatusFailed,
		RenewalStatusReason: reason,
		UpdatedAt:           w.now(),
	}
	if err := w.svc.store.PutCert(ctx, rec); err != nil {
		return fmt.Errorf("store renewal failure: %w", err)
	}
	slog.WarnContext(ctx, "acm renewal: failed", "arn", rec.CertificateArn, "reason", reason, "detail", detail)
	return fmt.Errorf("acm renewal: %s: %s", reason, detail)
}

// ForceRenewCertificateInput names the certificate to renew immediately.
type ForceRenewCertificateInput struct {
	CertificateArn string `json:"certificate_arn"`
}

// ForceRenewCertificateOutput reports the reissued certificate's new serial
// and expiry.
type ForceRenewCertificateOutput struct {
	CertificateArn string    `json:"certificate_arn"`
	Serial         string    `json:"serial"`
	NotAfter       time.Time `json:"not_after"`
}

// ForceRenewCertificate reissues input.CertificateArn immediately, bypassing
// the renewal window and failure backoff — but not lease contention or CA
// authorization. This is the operator-triggered path (wired to `spx admin
// cert force-renew` over NATS subject "acm.ForceRenewCertificate") used to
// verify the ELBv2 fan-out live without waiting for the proportional window
// to elapse.
func (w *Worker) ForceRenewCertificate(ctx context.Context, input *ForceRenewCertificateInput, accountID string) (*ForceRenewCertificateOutput, error) {
	if input == nil || input.CertificateArn == "" {
		return nil, errors.New(awserrors.ErrorACMInvalidArn)
	}
	rec, err := w.svc.lookupOwned(ctx, input.CertificateArn, accountID)
	if err != nil {
		return nil, err
	}
	if rec.Type != acm.CertificateTypePrivate {
		return nil, fmt.Errorf("acm renewal: %s is a %s certificate; only PRIVATE_CA certificates can be force-renewed", input.CertificateArn, rec.Type)
	}

	acquired, err := w.renewWithLease(ctx, input.CertificateArn)
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, fmt.Errorf("acm renewal: %s is currently leased by another node; try again shortly", input.CertificateArn)
	}

	// Re-read for the post-renewal outcome only: serial, validity and renewal
	// status. The new key was already written by the reissue above and is not
	// read here, so this takes the metadata read.
	renewed, err := w.svc.store.GetCertMetadata(ctx, input.CertificateArn)
	if err != nil {
		return nil, fmt.Errorf("get renewed cert: %w", err)
	}
	if renewed.RenewalSummary != nil && renewed.RenewalSummary.RenewalStatus == acm.RenewalStatusFailed {
		return nil, fmt.Errorf("acm renewal: %s: %s", renewed.RenewalSummary.RenewalStatusReason, input.CertificateArn)
	}
	return &ForceRenewCertificateOutput{
		CertificateArn: renewed.CertificateArn,
		Serial:         renewed.Serial,
		NotAfter:       renewed.NotAfter,
	}, nil
}
