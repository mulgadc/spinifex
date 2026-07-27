package handlers_acm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

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

// CertRecord is the stored representation of an imported certificate.
// AccountID scopes ownership so list/describe never cross account boundaries.
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
}

// Store provides CRUD for ACM certificate records backed by JetStream KV.
type Store struct {
	kv jetstream.KeyValue
}

// NewStore creates an ACM store using the provided NATS connection. ctx bounds
// the bucket get-or-create only; each operation carries its own.
func NewStore(ctx context.Context, nc *nats.Conn) (*Store, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketACM, KVBucketACMVersion)
	if err != nil {
		return nil, err
	}

	slog.Info("ACM store initialized", "bucket", KVBucketACM)
	return &Store{kv: kv}, nil
}

// certKey derives the KV key from a certificate ARN using the UUID after "certificate/".
func certKey(certArn string) string {
	id := certArn
	if i := strings.LastIndex(certArn, "/"); i >= 0 {
		id = certArn[i+1:]
	}
	return KeyPrefixCert + id
}

// PutCert stores (or replaces) a certificate record.
func (s *Store) PutCert(ctx context.Context, rec *CertRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal cert: %w", err)
	}
	_, err = s.kv.Put(ctx, certKey(rec.CertificateArn), data)
	return err
}

// GetCert retrieves a certificate by ARN, returning (nil, nil) when absent.
func (s *Store) GetCert(ctx context.Context, certArn string) (*CertRecord, error) {
	entry, err := s.kv.Get(ctx, certKey(certArn))
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
	return &rec, nil
}

// DeleteCert removes a certificate by ARN. Returns (false, nil) when absent.
func (s *Store) DeleteCert(ctx context.Context, certArn string) (bool, error) {
	if _, err := s.kv.Get(ctx, certKey(certArn)); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.kv.Delete(ctx, certKey(certArn)); err != nil {
		return false, err
	}
	return true, nil
}

// ListCerts returns all certificate records owned by accountID.
func (s *Store) ListCerts(ctx context.Context, accountID string) ([]*CertRecord, error) {
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
		if rec.AccountID != accountID {
			continue
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
			if errors.Is(err, jetstream.ErrKeyExists) {
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
