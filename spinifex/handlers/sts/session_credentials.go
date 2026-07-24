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

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	//nolint:gosec // G101: bucket name string, not a credential value
	KVBucketSessionCredentials        = "spinifex-iam-session-credentials"
	KVBucketSessionCredentialsVersion = 1

	// SessionAccessKeyIDPrefix is the AWS-defined prefix for STS temporary credentials.
	// Long-lived keys use AKIA; the two namespaces live in separate KV buckets.
	SessionAccessKeyIDPrefix = "ASIA"

	// janitorInterval is how often the credential sweep runs.
	janitorInterval = 5 * time.Minute

	// janitorGracePeriod keeps just-expired records so a client with a slightly fast
	// clock gets ExpiredToken (diagnosable) rather than InvalidClientTokenId.
	janitorGracePeriod = 1 * time.Hour
)

// SessionCredential is the on-disk record for an STS-issued temporary credential.
// Only the HMAC of the session token is stored; the raw token is returned once and never persisted.
type SessionCredential struct {
	AccessKeyID      string `json:"access_key_id"`
	SecretEncrypted  string `json:"secret_encrypted"`
	SessionTokenHMAC string `json:"session_token_hmac"`
	AccountID        string `json:"account_id"`

	// PrincipalType is "assumed-role" or "user" (GetSessionToken). Empty is treated as "assumed-role"
	// for backward compatibility with in-flight records minted before this field existed.
	PrincipalType string `json:"principal_type,omitempty"`

	AssumedRoleARN    string    `json:"assumed_role_arn"`
	UnderlyingRoleARN string    `json:"underlying_role_arn"`
	RoleID            string    `json:"role_id"`
	AssumedRoleID     string    `json:"assumed_role_id"`
	SessionName       string    `json:"session_name"`
	SourceIdentity    string    `json:"source_identity,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	CreatedAt         time.Time `json:"created_at"`
}

// initSessionCredentialsBucket opens (or creates) the session-credentials KV bucket.
// History is fixed at 1: credentials are write-once at mint and delete-once at expiry.
func initSessionCredentialsBucket(ctx context.Context, js jetstream.JetStream, replicas int) (jetstream.KeyValue, error) {
	// kvutil clamps replicas to a minimum of 1, so a zero clusterSize still creates.
	kv, err := kvutil.GetOrCreateBucketWithReplicas(ctx, js, KVBucketSessionCredentials, 1, replicas)
	if err != nil {
		return nil, fmt.Errorf("open session credentials bucket: %w", err)
	}

	if err := migrate.DefaultRegistry.RunKV(
		ctx, KVBucketSessionCredentials, kv, KVBucketSessionCredentialsVersion,
	); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSessionCredentials, err)
	}

	return kv, nil
}

// putSessionCredential persists a SessionCredential via CAS create, enforcing the ASIA-prefix
// invariant. Returns jetstream.ErrKeyExists on collision so callers can retry.
func putSessionCredential(ctx context.Context, bucket jetstream.KeyValue, cred *SessionCredential) error {
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
	if _, err := bucket.Create(ctx, cred.AccessKeyID, data); err != nil {
		return fmt.Errorf("store session credential: %w", err)
	}
	return nil
}

// VerifySessionToken recomputes the HMAC of the wire token and constant-time-compares
// it against the stored value. Returns true on match.
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
		return false // computeTokenHMAC always emits valid base64; defence in depth
	}
	return subtle.ConstantTimeCompare(got, expected) == 1
}

// LookupSessionCredential resolves an AKID to its stored SessionCredential.
// Returns (nil, nil) when the AKID lacks the ASIA prefix or has no record.
func (s *STSServiceImpl) LookupSessionCredential(accessKeyID string) (*SessionCredential, error) {
	ctx := context.Background()
	if !strings.HasPrefix(accessKeyID, SessionAccessKeyIDPrefix) {
		return nil, nil
	}
	entry, err := s.sessionsBucket.Get(ctx, accessKeyID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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

// RunJanitor periodically removes expired session credentials. Idempotent; multiple
// awsgw instances may run it concurrently. Blocks until ctx is cancelled.
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
			s.sweepExpired(ctx, time.Now().UTC())
		}
	}
}

// sweepExpired deletes all records whose ExpiresAt is past the grace period.
// Per-key errors are logged and skipped; returns the delete count.
func (s *STSServiceImpl) sweepExpired(ctx context.Context, now time.Time) int {
	cutoff := now.Add(-janitorGracePeriod)

	keys, err := kvutil.Keys(ctx, s.sessionsBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
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
		entry, err := s.sessionsBucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
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

		if err := s.sessionsBucket.Delete(ctx, key); err != nil {
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
