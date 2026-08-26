package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// bedrockCredentialsBucket is the cluster-replicated KV bucket holding
// per-account provider API keys, encrypted at rest with the IAM master key.
const bedrockCredentialsBucket = "bedrock-credentials" //nolint:gosec // G101: KV bucket name, not a credential

// bedrockCredentialsHistory keeps one revision; rotation overwrites in place.
const bedrockCredentialsHistory = 1

// credentialKey returns the KV key for accountID's vendor credential.
func credentialKey(accountID, vendor string) string {
	return fmt.Sprintf("providers/%s/%s", accountID, vendor)
}

// CredentialStore resolves per-account provider API keys from the
// bedrock-credentials JetStream KV bucket, falling back to a platform-wide
// default when an account has none. Keys are encrypted at rest.
// Values are IAM-encrypted ciphertext rather than JSON, so this is a Bucket:
// a Store[string] would wrap the ciphertext in a second encoding.
type CredentialStore struct {
	bucket           *kvstore.Bucket
	masterKey        []byte
	platformDefaults map[string]string
}

var _ CredentialResolver = (*CredentialStore)(nil)

// NewCredentialStore constructs a CredentialStore. platformDefaults maps
// vendor to a platform-wide API key used when an account has no key of its
// own; pass an empty map to disable platform defaults entirely.
func NewCredentialStore(js jetstream.JetStream, masterKey []byte, replicas int, platformDefaults map[string]string) *CredentialStore {
	return &CredentialStore{
		bucket: kvstore.NewBucket(js, kvstore.Config{
			Name:     bedrockCredentialsBucket,
			History:  bedrockCredentialsHistory,
			Replicas: replicas,
			Missing:  "bedrock: credential store has no JetStream client configured",
		}),
		masterKey:        masterKey,
		platformDefaults: platformDefaults,
	}
}

// Resolve returns accountID's vendor API key: a per-account key if one is
// stored, else the platform default, else ("", false, nil). A nil js is
// tolerated so a store built with only platformDefaults (e.g. in tests) can
// still serve the default-only path without touching JetStream.
func (s *CredentialStore) Resolve(ctx context.Context, accountID, vendor string) (string, bool, error) {
	if s.bucket.Configured() {
		kv, err := s.bucket.KV(ctx)
		if err != nil {
			return "", false, err
		}
		entry, err := kv.Get(ctx, credentialKey(accountID, vendor))
		switch {
		case err == nil:
			plaintext, err := handlers_iam.DecryptSecret(string(entry.Value()), s.masterKey)
			if err != nil {
				return "", false, fmt.Errorf("decrypt credential for %s/%s: %w", accountID, vendor, err)
			}
			return plaintext, true, nil
		case !errors.Is(err, jetstream.ErrKeyNotFound):
			return "", false, fmt.Errorf("kv get credential for %s/%s: %w", accountID, vendor, err)
		}
	}
	if key, ok := s.platformDefaults[vendor]; ok && key != "" {
		return key, true, nil
	}
	return "", false, nil
}

// PutCredential encrypts and stores key as accountID's vendor credential.
func (s *CredentialStore) PutCredential(ctx context.Context, accountID, vendor, key string) error {
	kv, err := s.bucket.KV(ctx)
	if err != nil {
		return err
	}
	ciphertext, err := handlers_iam.EncryptSecret(key, s.masterKey)
	if err != nil {
		return fmt.Errorf("encrypt credential for %s/%s: %w", accountID, vendor, err)
	}
	if _, err := kv.Put(ctx, credentialKey(accountID, vendor), []byte(ciphertext)); err != nil {
		return fmt.Errorf("kv put credential for %s/%s: %w", accountID, vendor, err)
	}
	return nil
}

// noopCredentialResolver is the zero-value fallback used when no
// CredentialStore is configured (e.g. unit tests of unrelated routes): it
// resolves no vendors.
type noopCredentialResolver struct{}

var _ CredentialResolver = (*noopCredentialResolver)(nil)

func (noopCredentialResolver) Resolve(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

// NoopCredentialResolver resolves no vendors. Used as the fallback wherever
// no CredentialStore is configured (nil GatewayConfig.BedrockCredentials).
var NoopCredentialResolver CredentialResolver = noopCredentialResolver{}
