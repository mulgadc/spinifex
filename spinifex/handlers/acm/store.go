package handlers_acm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
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
	kv nats.KeyValue
}

// NewStore creates an ACM store using the provided NATS connection.
func NewStore(nc *nats.Conn) (*Store, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := utils.GetOrCreateKVBucket(js, KVBucketACM, KVBucketACMVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketACM, err)
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
func (s *Store) PutCert(rec *CertRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal cert: %w", err)
	}
	_, err = s.kv.Put(certKey(rec.CertificateArn), data)
	return err
}

// GetCert retrieves a certificate by ARN, returning (nil, nil) when absent.
func (s *Store) GetCert(certArn string) (*CertRecord, error) {
	entry, err := s.kv.Get(certKey(certArn))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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
func (s *Store) DeleteCert(certArn string) (bool, error) {
	if _, err := s.kv.Get(certKey(certArn)); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := s.kv.Delete(certKey(certArn)); err != nil {
		return false, err
	}
	return true, nil
}

// ListCerts returns all certificate records owned by accountID.
func (s *Store) ListCerts(accountID string) ([]*CertRecord, error) {
	keys, err := s.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []*CertRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, KeyPrefixCert) {
			continue
		}
		entry, err := s.kv.Get(key)
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
