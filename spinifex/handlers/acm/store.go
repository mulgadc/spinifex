package handlers_acm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
