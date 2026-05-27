package handlers_sts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/nats-io/nats.go"
)

const (
	//nolint:gosec // G101: bucket name string, not a credential value
	KVBucketSessionCredentials        = "spinifex-iam-session-credentials"
	KVBucketSessionCredentialsVersion = 1

	// SessionAccessKeyIDPrefix is the AWS-defined prefix for STS-issued
	// temporary credentials. Long-lived IAM access keys use AKIA*; the two
	// namespaces live in disjoint KV buckets so a SigV4 prefix-first
	// dispatch cannot be confused by a misfiled record.
	SessionAccessKeyIDPrefix = "ASIA"
)

// SessionCredential is the on-disk record for an STS-issued temporary
// credential. The raw SessionToken is returned to the client once and never
// persisted; only an HMAC of it is stored, so a bucket read by a process that
// lacks the master key cannot recover the token.
type SessionCredential struct {
	AccessKeyID       string    `json:"access_key_id"`
	SecretEncrypted   string    `json:"secret_encrypted"`
	SessionTokenHMAC  string    `json:"session_token_hmac"`
	AccountID         string    `json:"account_id"`
	AssumedRoleARN    string    `json:"assumed_role_arn"`
	UnderlyingRoleARN string    `json:"underlying_role_arn"`
	RoleID            string    `json:"role_id"`
	AssumedRoleID     string    `json:"assumed_role_id"`
	SessionName       string    `json:"session_name"`
	SourceIdentity    string    `json:"source_identity,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	CreatedAt         time.Time `json:"created_at"`
}

// initSessionCredentialsBucket opens (or creates) the session-credentials KV
// bucket and runs any pending migrations.
//
// History is fixed at 1: session credentials are write-once at mint and
// delete-once at expiry, never updated, so retained revisions waste storage
// and lengthen tombstone lifetime with no recovery benefit. This is
// deliberately lower than the IAM buckets (5-10).
func initSessionCredentialsBucket(js nats.JetStreamContext, replicas int) (nats.KeyValue, error) {
	if replicas < 1 {
		replicas = 1
	}

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   KVBucketSessionCredentials,
		History:  1,
		Replicas: replicas,
	})
	if err != nil {
		kv, err = js.KeyValue(KVBucketSessionCredentials)
		if err != nil {
			return nil, fmt.Errorf("open session credentials bucket: %w", err)
		}
	}

	if err := migrate.DefaultRegistry.RunKV(
		KVBucketSessionCredentials, kv, KVBucketSessionCredentialsVersion,
	); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSessionCredentials, err)
	}

	return kv, nil
}

// putSessionCredential persists a SessionCredential via a CAS-safe create.
// All writes to the session-credentials bucket MUST go through this helper:
// the ASIA-prefix invariant is enforced here at the writer, and a regression
// is caught by the bucket-prefix invariants test.
//
// Returns nats.ErrKeyExists on AKID collision so callers can retry with a
// freshly generated AKID.
func putSessionCredential(bucket nats.KeyValue, cred *SessionCredential) error {
	if cred == nil {
		return errors.New("nil session credential")
	}
	if !strings.HasPrefix(cred.AccessKeyID, SessionAccessKeyIDPrefix) {
		return fmt.Errorf("session AKID must start with %q, got %q",
			SessionAccessKeyIDPrefix, cred.AccessKeyID)
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("marshal session credential: %w", err)
	}
	if _, err := bucket.Create(cred.AccessKeyID, data); err != nil {
		return fmt.Errorf("store session credential: %w", err)
	}
	return nil
}

// LookupSessionCredential resolves an access-key ID to its stored
// SessionCredential. Returns (nil, nil) when the AKID does not start with
// "ASIA" or when no record exists for it — the SigV4 verifier translates
// that miss into InvalidClientTokenId on the ASIA path. Any other failure
// (unmarshal, transport) is returned as an error.
func (s *STSServiceImpl) LookupSessionCredential(accessKeyID string) (*SessionCredential, error) {
	if !strings.HasPrefix(accessKeyID, SessionAccessKeyIDPrefix) {
		return nil, nil
	}
	entry, err := s.sessionsBucket.Get(accessKeyID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get session credential: %w", err)
	}
	var cred SessionCredential
	if err := json.Unmarshal(entry.Value(), &cred); err != nil {
		return nil, fmt.Errorf("unmarshal session credential: %w", err)
	}
	return &cred, nil
}
