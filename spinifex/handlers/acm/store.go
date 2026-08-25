package handlers_acm

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/arn"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	KVBucketACM        = "spinifex-acm"
	KVBucketACMVersion = 1

	// KeyPrefixCert namespaces certificate records within the bucket.
	KeyPrefixCert = "cert."
)

// Validation modes for a managed (RequestCertificate) certificate. The mode is
// derived from deployment state at request time, never configured, and stored
// on CertRecord.ValidationMethod so DescribeCertificate and the future
// issuance/renewal workers can see how a certificate is being validated
// without re-deriving it.
const (
	// ValidationModeProviderAPI: Spinifex writes the DNS-01 challenge via the
	// operator's DNS provider API. No ResourceRecord is returned — there is
	// nothing for the caller to publish. Automatic renewal.
	ValidationModeProviderAPI = "PROVIDER_API"
	// ValidationModeManualTXT: the operator publishes a rotating TXT challenge
	// record by hand or via Terraform. Renewal is INELIGIBLE because the
	// challenge token rotates per order, so unattended renewal cannot succeed.
	ValidationModeManualTXT = "MANUAL_TXT"
	// ValidationModeCNAMEDelegation: the operator publishes one stable CNAME
	// once, and Spinifex answers every subsequent challenge behind it. Deferred
	// until northstar can serve public authoritative queries; never selected by
	// deriveValidationMode today, but CertRecord.DelegationToken is minted for
	// every managed certificate now so this mode is a non-breaking addition
	// later.
	ValidationModeCNAMEDelegation = "CNAME_DELEGATION"
	// ValidationModePrivateCA: signed from the tenant root CA with no domain
	// validation at all. The only mode that issues synchronously.
	ValidationModePrivateCA = "PRIVATE_CA"
)

// DomainValidationEntry is the resumable per-domain validation state for one
// name on a managed certificate. Converted to the AWS-shaped
// []*acm.DomainValidation in recordToDetail. RecordType/RecordName/RecordValue
// stay empty for any mode where Spinifex (or, for PRIVATE_CA, nobody) owns the
// record write — that omission is what makes DescribeCertificate AWS-shaped
// without fabricating a ResourceRecord the caller has nothing to do with.
type DomainValidationEntry struct {
	DomainName string `json:"domain_name"`
	// RecordType/RecordName/RecordValue mirror acm.ResourceRecord. RecordType
	// empty means "no ResourceRecord" in the AWS-shaped output.
	RecordType  string `json:"record_type,omitempty"`
	RecordName  string `json:"record_name,omitempty"`
	RecordValue string `json:"record_value,omitempty"`
	// ValidationStatus is one of the ACM DomainStatus values (PENDING_VALIDATION,
	// SUCCESS, FAILED).
	ValidationStatus string `json:"validation_status"`
	// ValidationMethod is DNS for the DNS-based modes and empty for PRIVATE_CA,
	// which validates nothing. Real ACM omits it on a private certificate too,
	// and the AWS provider reads it to decide whether the configured
	// validation_method still matches.
	ValidationMethod string `json:"validation_method,omitempty"`
}

// RenewalSummaryRecord is the stored form of acm.RenewalSummary, populated by
// the future renewal worker. RenewalStatusReason is what lets a certificate
// that issued cleanly but can no longer renew surface before it expires
// rather than after.
type RenewalSummaryRecord struct {
	RenewalStatus       string    `json:"renewal_status"`
	RenewalStatusReason string    `json:"renewal_status_reason,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// CertRecord is the stored representation of a certificate, whether imported
// or issued by RequestCertificate. AccountID scopes ownership so list/describe
// never cross account boundaries.
type CertRecord struct {
	CertificateArn   string            `json:"certificate_arn"`
	AccountID        string            `json:"account_id"`
	Certificate      string            `json:"certificate"`
	CertificateChain string            `json:"certificate_chain,omitempty"`
	PrivateKey       string            `json:"private_key"`
	DomainName       string            `json:"domain_name"`
	SubjectAltNames  []string          `json:"subject_alt_names,omitempty"`
	Serial           string            `json:"serial"`
	Subject          string            `json:"subject"`
	Issuer           string            `json:"issuer"`
	KeyAlgorithm     string            `json:"key_algorithm"`
	NotBefore        time.Time         `json:"not_before"`
	NotAfter         time.Time         `json:"not_after"`
	ImportedAt       time.Time         `json:"imported_at"`
	Tags             map[string]string `json:"tags,omitempty"`
	// InUseBy is the set of load balancer ARNs whose listeners currently
	// reference this certificate. Maintained by handlers/elbv2 as listeners are
	// created, modified and deleted; also surfaces as the public
	// CertificateDetail.InUseBy field.
	InUseBy []string `json:"in_use_by,omitempty"`

	// Type is the ACM certificate type: AMAZON_ISSUED, PRIVATE or IMPORTED.
	Type string `json:"type,omitempty"`
	// Status is the ACM certificate status (PENDING_VALIDATION, ISSUED,
	// FAILED, ...).
	Status string `json:"status,omitempty"`
	// ValidationMethod is the derived validation mode (see the
	// ValidationMode* constants above). Empty for imported certificates, which
	// have no validation.
	ValidationMethod string `json:"validation_method,omitempty"`
	// DomainValidationOptions carries one entry per name on the certificate
	// (DomainName plus any SubjectAltNames).
	DomainValidationOptions []DomainValidationEntry `json:"domain_validation_options,omitempty"`
	// FailureReason is populated on terminal issuance failure, using the ACM
	// FailureReason enum.
	FailureReason string `json:"failure_reason,omitempty"`
	// RenewalEligibility reports whether the renewal worker may reissue this
	// certificate under its existing ARN.
	RenewalEligibility string `json:"renewal_eligibility,omitempty"`
	// RenewalSummary is nil until a renewal has been attempted.
	RenewalSummary *RenewalSummaryRecord `json:"renewal_summary,omitempty"`
	// DelegationToken is minted once at RequestCertificate time and persisted
	// for the certificate's lifetime, so that CNAME_DELEGATION mode, when it
	// lands, has a stable CNAME target that never changes retroactively. Unused
	// by any mode today.
	DelegationToken string `json:"delegation_token,omitempty"`

	// --- Resumable ACME order state, populated by the future issuance worker.
	// Persisted so a crashed lease holder's work resumes elsewhere instead of
	// minting a fresh order (and burning rate-limit budget) on every retry.
	ACMEOrderURL       string    `json:"acme_order_url,omitempty"`
	ACMEAuthzURLs      []string  `json:"acme_authz_urls,omitempty"`
	ACMEChallengeToken string    `json:"acme_challenge_token,omitempty"`
	ACMEAttemptCount   int       `json:"acme_attempt_count,omitempty"`
	ACMENextAttemptAt  time.Time `json:"acme_next_attempt_at"`

	// --- Per-certificate issuance lease (see handlers/eks/cluster_reconciler.go
	// for the pattern this follows). Only the holder may drive this
	// certificate's order; empty/zero when unleased.
	LeaseHolder    string    `json:"lease_holder,omitempty"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Store provides CRUD for ACM certificate records backed by JetStream KV.
type Store struct {
	kv jetstream.KeyValue
	// masterKey encrypts/decrypts CertRecord.PrivateKey at rest (AES-256-GCM via
	// handlers_iam.EncryptSecret/DecryptSecret). Every Store over the ACM bucket
	// — the ACM service and ELBv2's independent read-only Store alike — must be
	// constructed with the same deployment key: a keyed writer and an unkeyed (or
	// differently keyed) reader of the same bucket disagree silently, with the
	// reader getting ciphertext where it expects PEM. NewStore therefore requires
	// a non-empty key; there is no unkeyed Store.
	masterKey []byte
}

// NewStore creates an ACM store using the provided NATS connection. ctx bounds
// the bucket get-or-create only; each operation carries its own. masterKey
// encrypts CertRecord.PrivateKey on write and decrypts it on read; it must be
// non-empty so every caller sharing the bucket agrees on the same key.
func NewStore(ctx context.Context, nc *nats.Conn, masterKey []byte) (*Store, error) {
	if len(masterKey) == 0 {
		return nil, fmt.Errorf("ACM store requires a master key to encrypt certificate private keys at rest; none provided")
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketACM, KVBucketACMVersion)
	if err != nil {
		return nil, err
	}

	slog.Info("ACM store initialized", "bucket", KVBucketACM)
	return &Store{kv: kv, masterKey: masterKey}, nil
}

// certKey derives the KV key from a certificate ARN, reading the id exactly as
// the policy gate reads it. Anything else yields "", which every caller treats
// as absent: the gate authorizes an ARN it cannot read account-wide, so a
// lookup accepting a shape the gate rejects would reach a certificate that no
// statement named.
func certKey(certArn string) string {
	id, ok := arn.ParseACMCertificateID(certArn)
	if !ok {
		return ""
	}
	return KeyPrefixCert + id
}

// PutCert stores (or replaces) a certificate record. PrivateKey is
// AES-256-GCM encrypted before it is written; a plaintext record read back
// via the legacy-passthrough path in decryptPrivateKey is therefore
// re-encrypted the next time it is put.
func (s *Store) PutCert(ctx context.Context, rec *CertRecord) error {
	ciphertext, err := handlers_iam.EncryptSecret(rec.PrivateKey, s.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt private key: %w", err)
	}
	// Encrypt a copy so the caller's in-memory record keeps holding
	// plaintext — ImportCertificate and the tag handlers reuse *rec after
	// this call.
	clone := *rec
	clone.PrivateKey = ciphertext
	data, err := json.Marshal(&clone)
	if err != nil {
		return fmt.Errorf("marshal cert: %w", err)
	}
	key := certKey(rec.CertificateArn)
	if key == "" {
		return fmt.Errorf("acm store: not a certificate ARN: %q", rec.CertificateArn)
	}
	_, err = s.kv.Put(ctx, key, data)
	return err
}

// GetCert retrieves a certificate by ARN with PrivateKey decrypted to
// plaintext PEM, returning (nil, nil) when absent. Use it only where the key
// itself is needed (HAProxy fan-out, reissue); everything else wants
// GetCertMetadata.
func (s *Store) GetCert(ctx context.Context, certArn string) (*CertRecord, error) {
	return s.getCert(ctx, certArn, true)
}

// GetCertMetadata retrieves a certificate by ARN with PrivateKey left empty
// rather than decrypted, returning (nil, nil) when absent.
//
// This is the right read for every caller that answers a question about a
// certificate rather than serving it: ownership checks, Describe, Delete, tag
// reads and GetCertificate all ignore the key, and decrypting it for them
// produces plaintext of exactly the material this store exists to protect
// with no caller to consume it.
//
// A record whose key cannot be decrypted is still returned here, since
// nothing on this path reads it — an unreadable key must not make a
// certificate undescribable or undeletable.
func (s *Store) GetCertMetadata(ctx context.Context, certArn string) (*CertRecord, error) {
	return s.getCert(ctx, certArn, false)
}

// getCert reads and unmarshals one certificate record, resolving PrivateKey to
// plaintext PEM when decrypt is true and clearing it to "" otherwise, so a
// caller cannot mistake ciphertext for usable key material in a field typed as
// plaintext PEM everywhere else in this package.
func (s *Store) getCert(ctx context.Context, certArn string, decrypt bool) (*CertRecord, error) {
	key := certKey(certArn)
	if key == "" {
		return nil, nil
	}
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var rec CertRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal cert: %w", err)
	}
	if !decrypt {
		rec.PrivateKey = ""
		return &rec, nil
	}
	if err := s.decryptPrivateKey(&rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// legacyPrivateKeyPEMTypes are the PEM block types ImportCertificate accepts
// for a private key (see tls.X509KeyPair / crypto/x509 parsing in
// service_impl.go). Gates the legacy-plaintext passthrough in
// decryptPrivateKey below.
var legacyPrivateKeyPEMTypes = map[string]bool{
	"RSA PRIVATE KEY": true,
	"EC PRIVATE KEY":  true,
	"PRIVATE KEY":     true, // PKCS#8
}

// isPlaintextPrivateKeyPEM reports whether raw is unambiguously a single
// PEM-encoded private key block: pem.Decode must consume the entire string
// (no trailing bytes) and the block type must be a recognized private-key
// type. Deliberately strict — this is the sole gate that lets decryptPrivateKey
// treat a decrypt failure as "pre-encryption legacy record" rather than
// "corrupt or tampered ciphertext", so loosening it would turn the fallback
// into a downgrade oracle that trusts arbitrary bytes as plaintext.
func isPlaintextPrivateKeyPEM(raw string) bool {
	block, rest := pem.Decode([]byte(raw))
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return false
	}
	return legacyPrivateKeyPEMTypes[block.Type]
}

// decryptPrivateKey resolves rec.PrivateKey to plaintext PEM in place.
//
// Attempts AES-256-GCM decryption first. A successful decrypt is the common
// case once a record has been through PutCert under this Store. A failed
// decrypt falls back to legacy-plaintext ONLY when the raw value is
// unambiguously a PEM private key (isPlaintextPrivateKeyPEM) — i.e. a record
// written before encryption was wired up. It is re-encrypted automatically
// the next time PutCert runs. Anything else (wrong key, truncated/tampered
// ciphertext, garbage) is a hard error rather than a silent plaintext
// fallback.
func (s *Store) decryptPrivateKey(rec *CertRecord) error {
	if plaintext, err := handlers_iam.DecryptSecret(rec.PrivateKey, s.masterKey); err == nil {
		rec.PrivateKey = plaintext
		return nil
	}
	if isPlaintextPrivateKeyPEM(rec.PrivateKey) {
		return nil
	}
	return fmt.Errorf("cert %s: private key is neither valid ciphertext nor a recognizable PEM key", rec.CertificateArn)
}

// DeleteCert removes a certificate by ARN. Returns (false, nil) when absent.
func (s *Store) DeleteCert(ctx context.Context, certArn string) (bool, error) {
	key := certKey(certArn)
	if key == "" {
		return false, nil
	}
	if _, err := s.kv.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.kv.Delete(ctx, key); err != nil {
		return false, err
	}
	return true, nil
}

// ListCertMetadata returns metadata for every certificate owned by accountID,
// with PrivateKey left empty rather than decrypted.
//
// Listing is a summary operation: ListCertificates, its only caller, reads
// ARN/domain/status/type/algorithm/validity/InUseBy and never the key. A
// decrypting variant existed here and cost an AES-GCM open per stored
// certificate on every list call, scaling with certificate count, to produce
// plaintext of exactly the material this store exists to protect and that no
// caller then read. A caller needing key material fetches the one certificate
// it wants via GetCert.
func (s *Store) ListCertMetadata(ctx context.Context, accountID string) ([]*CertRecord, error) {
	return s.listCerts(ctx, func(rec *CertRecord) bool { return rec.AccountID == accountID }, false)
}

// ListAllCertMetadata returns every certificate record's metadata, across
// every account, with PrivateKey left empty rather than decrypted.
//
// This exists for the renewal worker's scan, which only ever reads
// Type/Status/RenewalEligibility/NotBefore/NotAfter/RenewalSummary/
// CertificateArn to decide what is due — decrypting every stored private key
// on every scan tick, forever, to answer a question that never looks at the
// key is needless AES-GCM work that scales with certificate count, and
// needless plaintext exposure of exactly the material this store exists to
// protect. The worker re-fetches the one certificate it actually renews via
// GetCert (which does decrypt) once it has won that certificate's lease.
//
// PrivateKey is cleared to "" here rather than left as ciphertext, so a
// caller cannot mistake ciphertext for usable key material in a field typed
// as plaintext PEM everywhere else in this package.
func (s *Store) ListAllCertMetadata(ctx context.Context) ([]*CertRecord, error) {
	return s.listCerts(ctx, func(*CertRecord) bool { return true }, false)
}

// listCerts walks every cert key in the bucket, keeping only the records for
// which keep returns true. When decrypt is true, PrivateKey is resolved to
// plaintext PEM as GetCert does, and a record whose key cannot be decrypted
// is skipped and logged rather than failing the whole scan. When decrypt is
// false, PrivateKey is cleared to "" instead of decrypted — see
// ListAllCertMetadata — and an undecryptable key is not a reason to skip the
// record, since nothing here reads it.
func (s *Store) listCerts(ctx context.Context, keep func(*CertRecord) bool, decrypt bool) ([]*CertRecord, error) {
	keys, err := s.kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []*CertRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, KeyPrefixCert) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var rec CertRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		if !keep(&rec) {
			continue
		}
		if decrypt {
			if err := s.decryptPrivateKey(&rec); err != nil {
				slog.Warn("listCerts: skipping cert with undecryptable private key", "arn", rec.CertificateArn, "err", err)
				continue
			}
		} else {
			rec.PrivateKey = ""
		}
		out = append(out, &rec)
	}
	return out, nil
}

// maxInUseByCASAttempts bounds the AddInUseBy/RemoveInUseBy optimistic-
// concurrency retry loop so a pathologically contended key fails loudly
// instead of retrying forever. Set high enough to ride out a burst of
// listeners referencing the same certificate all writing at once (e.g. a
// load balancer created with several HTTPS listeners in short succession).
const maxInUseByCASAttempts = 50

// inUseByCASBackoffBase is the base delay between CAS retries. A small
// jittered backoff spreads out contending writers instead of having every
// retry immediately re-collide on the same revision.
const inUseByCASBackoffBase = 2 * time.Millisecond

// AddInUseBy adds resourceArn (a load balancer ARN) to certArn's InUseBy set.
// No-op if the certificate does not exist or already lists resourceArn.
//
// InUseBy is the sole mechanism by which a re-imported certificate reaches
// the data plane (see UpdateStoredConfigForCert in handlers/elbv2), so a lost
// update here is not a cosmetic race: it silently drops a load balancer from
// fan-out, and that load balancer's certificate expires in HAProxy while ACM
// still reports it as renewed. Two listeners on different load balancers can
// legitimately attach the same certificate concurrently, so this uses
// JetStream KV revision-based compare-and-swap rather than a plain
// get/mutate/put, retrying on a conflicting concurrent writer.
func (s *Store) AddInUseBy(ctx context.Context, certArn, resourceArn string) error {
	return s.updateInUseByCAS(ctx, certArn, func(cur []string) []string {
		if slices.Contains(cur, resourceArn) {
			return nil // no-op: already present
		}
		next := append(slices.Clone(cur), resourceArn)
		slices.Sort(next)
		return next
	})
}

// RemoveInUseBy removes resourceArn from certArn's InUseBy set. No-op if the
// certificate or the entry does not exist. See AddInUseBy for why this uses
// CAS rather than get/mutate/put.
func (s *Store) RemoveInUseBy(ctx context.Context, certArn, resourceArn string) error {
	return s.updateInUseByCAS(ctx, certArn, func(cur []string) []string {
		idx := slices.Index(cur, resourceArn)
		if idx == -1 {
			return nil // no-op: not present
		}
		return slices.Delete(slices.Clone(cur), idx, idx+1)
	})
}

// updateInUseByCAS applies mutate to certArn's current InUseBy set and writes
// the result back with a revision-checked kv.Update, retrying against the
// latest revision whenever a concurrent writer wins the race. mutate returns
// nil to mean "no change needed", in which case nothing is written. Returns
// nil (no-op, no retry) if the certificate does not exist.
func (s *Store) updateInUseByCAS(ctx context.Context, certArn string, mutate func(cur []string) []string) error {
	key := certKey(certArn)
	if key == "" {
		return nil
	}
	for attempt := range maxInUseByCASAttempts {
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil
			}
			return err
		}
		var rec CertRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return fmt.Errorf("unmarshal cert: %w", err)
		}

		next := mutate(rec.InUseBy)
		if next == nil {
			return nil
		}
		rec.InUseBy = next

		data, err := json.Marshal(&rec)
		if err != nil {
			return fmt.Errorf("marshal cert: %w", err)
		}
		if _, err := s.kv.Update(ctx, key, data, entry.Revision()); err != nil {
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				// Another writer updated the record between our Get and
				// Update; back off briefly (jittered, so contending writers
				// don't all re-collide on the same revision) and retry
				// against whatever is there now.
				backoff := inUseByCASBackoffBase * time.Duration(attempt+1)
				jitter := time.Duration(rand.Int64N(int64(backoff))) //nolint:gosec // jitter, not cryptographic
				select {
				case <-time.After(backoff/2 + jitter):
				case <-ctx.Done():
					return ctx.Err()
				}
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("acm store: exceeded %d CAS attempts updating InUseBy for %s", maxInUseByCASAttempts, certArn)
}

// AcquireLease attempts to take the per-certificate issuance/renewal lease on
// certArn for holderID, valid until now+ttl. The lease lives on the record
// itself (CertRecord.LeaseHolder/LeaseExpiresAt) rather than in a separate
// leader-election bucket — contrast handlers/eks/cluster_reconciler.go, which
// leases a whole cluster for the life of a Run loop — because the unit of
// work here is one certificate, reissued and released in a single pass.
//
// Succeeds (true) when the lease is unheld, expired, or already held by
// holderID, so a worker that restarts mid-renewal with the same holderID
// does not need to release first. Returns (false, nil), not an error, when a
// different holder's lease is still live or a concurrent acquirer won the
// CAS race — the caller should skip this certificate and let the current
// holder finish or the lease expire.
func (s *Store) AcquireLease(ctx context.Context, certArn, holderID string, ttl time.Duration, now time.Time) (bool, error) {
	key := certKey(certArn)
	if key == "" {
		return false, nil
	}
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	var rec CertRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return false, fmt.Errorf("unmarshal cert: %w", err)
	}
	if rec.LeaseHolder != "" && rec.LeaseHolder != holderID && rec.LeaseExpiresAt.After(now) {
		return false, nil
	}
	rec.LeaseHolder = holderID
	rec.LeaseExpiresAt = now.Add(ttl)
	data, err := json.Marshal(&rec)
	if err != nil {
		return false, fmt.Errorf("marshal cert: %w", err)
	}
	if _, err := s.kv.Update(ctx, key, data, entry.Revision()); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			// Lost the race to a concurrent acquirer between Get and Update;
			// the caller skips this tick rather than retrying immediately.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReleaseLease clears certArn's lease if it is still held by holderID, so the
// next scan (on this node or another) can pick the certificate up immediately
// instead of waiting out the TTL. Best-effort: a failed release just leaves
// the lease for LeaseExpiresAt to reap.
func (s *Store) ReleaseLease(ctx context.Context, certArn, holderID string) error {
	key := certKey(certArn)
	if key == "" {
		return nil
	}
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return err
	}
	var rec CertRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return fmt.Errorf("unmarshal cert: %w", err)
	}
	if rec.LeaseHolder != holderID {
		return nil // already released, expired, or taken over by another holder
	}
	rec.LeaseHolder = ""
	rec.LeaseExpiresAt = time.Time{}
	data, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("marshal cert: %w", err)
	}
	if _, err := s.kv.Update(ctx, key, data, entry.Revision()); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return nil // record changed concurrently; nothing to clean up
		}
		return err
	}
	return nil
}
