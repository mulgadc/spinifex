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

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
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

	AssumedRoleARN    string `json:"assumed_role_arn"`
	UnderlyingRoleARN string `json:"underlying_role_arn"`
	RoleID            string `json:"role_id"`
	AssumedRoleID     string `json:"assumed_role_id"`
	SessionName       string `json:"session_name"`

	// UserID is the immutable IAM UserId a "user" session was minted for, the
	// counterpart of RoleID. Empty on role sessions and on records minted before
	// the field existed; VerifySessionPrincipal rejects the latter.
	UserID string `json:"user_id,omitempty"`

	SourceIdentity string    `json:"source_identity,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// sessionCredentialsConfig describes the session-credentials bucket. History is
// fixed at 1: credentials are write-once at mint and delete-once at expiry.
func sessionCredentialsConfig() kvstore.Config {
	return kvstore.Config{
		Name:    KVBucketSessionCredentials,
		History: 1,
		Missing: "session credentials KV bucket not initialized",
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, KVBucketSessionCredentials, kv, KVBucketSessionCredentialsVersion)
		},
	}
}

// initSessionCredentialsStore opens (or creates) the session-credentials bucket.
func initSessionCredentialsStore(ctx context.Context, js jetstream.JetStream) (*kvstore.Store[SessionCredential], error) {
	store := kvstore.New[SessionCredential](js, sessionCredentialsConfig())
	// Opened eagerly: a bucket that cannot be created must fail service
	// construction, not the first AssumeRole that needs to mint into it.
	if _, err := store.KV(ctx); err != nil {
		return nil, fmt.Errorf("open session credentials bucket: %w", err)
	}
	return store, nil
}

// putSessionCredential persists a SessionCredential via CAS create, enforcing the ASIA-prefix
// invariant. Returns kvstore.ErrExists on collision so callers can retry.
func putSessionCredential(ctx context.Context, store *kvstore.Store[SessionCredential], cred *SessionCredential) error {
	if cred == nil {
		return errors.New("nil session credential")
	}
	if !strings.HasPrefix(cred.AccessKeyID, SessionAccessKeyIDPrefix) {
		return fmt.Errorf("session AKID must start with %q, got %q",
			SessionAccessKeyIDPrefix, cred.AccessKeyID)
	}
	if _, err := store.Create(ctx, cred.AccessKeyID, cred); err != nil {
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
	cred, _, err := s.sessions.Get(ctx, accessKeyID)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get session credential: %w", err)
	}
	return cred, nil
}

// RunJanitor periodically removes expired session credentials. Idempotent; multiple
// awsgw instances may run it concurrently. Blocks until ctx is cancelled.
func (s *STSServiceImpl) RunJanitor(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	slog.Info("STS session credential janitor started",
		"interval_ms", otelsetup.Millis(janitorInterval),
		"grace_period_ms", otelsetup.Millis(janitorGracePeriod))

	for {
		select {
		case <-ctx.Done():
			slog.Info("STS session credential janitor stopped")
			return
		case <-ticker.C:
			s.sweep(ctx, time.Now().UTC())
		}
	}
}

// sweep deletes every record past the grace period, and every unexpired record
// whose principal no longer verifies: the retry path for a failed revocation at
// deletion and for a mint that raced it. A verification fault keeps the record.
func (s *STSServiceImpl) sweep(ctx context.Context, now time.Time) int {
	cutoff := now.Add(-janitorGracePeriod)

	var orphaned, faults int
	deleted, err := s.deleteSessions(ctx, func(cred *SessionCredential) bool {
		if cred.ExpiresAt.Before(cutoff) {
			return true
		}
		_, verr := s.VerifySessionPrincipal(cred)
		switch {
		case verr == nil:
			return false
		case IsSessionPrincipalVerdict(verr):
			orphaned++
			return true
		default:
			faults++
			return false
		}
	})
	if err != nil {
		slog.Warn("STS janitor: sweep incomplete", "err", err)
	}
	if faults > 0 {
		slog.Warn("STS janitor: session principal verification failed; records kept", "count", faults)
	}
	if deleted > 0 {
		slog.Info("session credentials swept", "count", deleted, "orphaned", orphaned)
	}
	return deleted
}

// RevokeUserSessions deletes the session records of a deleted user. A record
// matches on UserID or, for records predating it, on name: names are unique per
// account, so no live user can hold a session under the deleted user's name.
func (s *STSServiceImpl) RevokeUserSessions(ctx context.Context, accountID, userName, userID string) (int, error) {
	return s.deleteSessions(ctx, func(cred *SessionCredential) bool {
		if cred.PrincipalType != principalTypeUser || cred.AccountID != accountID {
			return false
		}
		return (userID != "" && cred.UserID == userID) || (userName != "" && cred.SessionName == userName)
	})
}

// RevokeRoleSessions deletes the session records of a deleted role, matching on
// RoleID or, for records predating it, on the underlying role ARN.
func (s *STSServiceImpl) RevokeRoleSessions(ctx context.Context, accountID, roleARN, roleID string) (int, error) {
	return s.deleteSessions(ctx, func(cred *SessionCredential) bool {
		if cred.PrincipalType == principalTypeUser || cred.AccountID != accountID {
			return false
		}
		return (roleID != "" && cred.RoleID == roleID) || (roleARN != "" && cred.UnderlyingRoleARN == roleARN)
	})
}

// deleteSessions scans the bucket and deletes every record match selects. The
// bucket is keyed by AKID, so selecting by anything else is a full scan.
// Per-key failures are skipped and joined, so one bad record shields no other.
//
// Deliberately on the raw handle rather than Store.List: the scan needs the key
// to delete it, and one undecodable record must not stop the rest.
func (s *STSServiceImpl) deleteSessions(ctx context.Context, match func(*SessionCredential) bool) (int, error) {
	bucket, err := s.sessions.KV(ctx)
	if err != nil {
		return 0, fmt.Errorf("open session credential bucket: %w", err)
	}

	keys, err := kvutil.Keys(ctx, bucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("list session credential keys: %w", err)
	}

	var deleted int
	var errs []error
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := bucket.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("get session credential %s: %w", key, err))
			continue
		}

		var cred SessionCredential
		if err := json.Unmarshal(entry.Value(), &cred); err != nil {
			errs = append(errs, fmt.Errorf("unmarshal session credential %s: %w", key, err))
			continue
		}
		if !match(&cred) {
			continue
		}

		if err := bucket.Delete(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("delete session credential %s: %w", key, err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}
