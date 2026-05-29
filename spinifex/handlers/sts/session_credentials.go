package handlers_sts

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

	// janitorInterval is how often the sweep runs. Tight enough that
	// expired-then-resurrected AKIDs would have to wait < this for cleanup;
	// loose enough that the iterate-all-keys cost is amortised.
	janitorInterval = 5 * time.Minute

	// janitorGracePeriod keeps just-expired records around so a client whose
	// clock is slightly ahead still gets ExpiredToken (a diagnosable error)
	// rather than InvalidClientTokenId (which masks the real cause).
	janitorGracePeriod = 1 * time.Hour
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

// VerifySessionToken recomputes the HMAC of the wire-form session token under
// the IAM master key and constant-time-compares it against the stored value on
// cred. Returns true on match. A bucket-read by any process that lacks the
// master key cannot recover a usable token: the wire token is never persisted.
func (s *STSServiceImpl) VerifySessionToken(cred *SessionCredential, wireToken string) bool {
	if cred == nil || wireToken == "" {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(cred.SessionTokenHMAC)
	if err != nil {
		slog.Error("Stored session token HMAC is not valid base64",
			"accessKeyID", cred.AccessKeyID, "err", err)
		return false
	}
	got, err := base64.StdEncoding.DecodeString(computeTokenHMAC(s.masterKey, wireToken))
	if err != nil {
		// computeTokenHMAC always emits valid base64; defence in depth.
		return false
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
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

// RunJanitor periodically removes session credentials whose ExpiresAt is
// further in the past than janitorGracePeriod. The sweep is idempotent —
// delete-of-already-deleted is a JetStream no-op — so multiple awsgw
// instances may run independently without coordination.
//
// Blocks until ctx is cancelled. Intended to be invoked as `go svc.RunJanitor(ctx)`.
func (s *STSServiceImpl) RunJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	slog.Info("STS session credential janitor started",
		"interval", janitorInterval,
		"grace_period", janitorGracePeriod)

	for {
		select {
		case <-ctx.Done():
			slog.Info("STS session credential janitor stopped")
			return
		case <-ticker.C:
			s.sweepExpired(time.Now().UTC())
		}
	}
}

// sweepExpired iterates the session-credentials bucket and deletes every
// record whose ExpiresAt is older than now - janitorGracePeriod. Per-key
// errors are logged and skipped — one corrupt record must not stall the
// sweep. Returns the number of records deleted (exposed for tests).
func (s *STSServiceImpl) sweepExpired(now time.Time) int {
	cutoff := now.Add(-janitorGracePeriod)

	keys, err := s.sessionsBucket.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return 0
		}
		slog.Warn("STS janitor: list session credential keys failed", "err", err)
		return 0
	}

	var deleted int
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := s.sessionsBucket.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("STS janitor: get session credential failed",
				"key", key, "err", err)
			continue
		}

		var cred SessionCredential
		if err := json.Unmarshal(entry.Value(), &cred); err != nil {
			slog.Warn("STS janitor: unmarshal session credential failed",
				"key", key, "err", err)
			continue
		}

		if !cred.ExpiresAt.Before(cutoff) {
			continue
		}

		if err := s.sessionsBucket.Delete(key); err != nil {
			slog.Warn("STS janitor: delete expired session credential failed",
				"key", key, "err", err)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		slog.Info("session credentials swept", "count", deleted)
	}
	return deleted
}
