package gateway_bedrock

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// modelAccessBucket is the cluster-replicated KV bucket holding per-account
// model-access grants. A grant is presence-only: the key exists iff the
// account may use the model, so the value carries no meaning.
const modelAccessBucket = "bedrock-model-access"

// modelAccessHistory keeps one revision; a grant is a set membership, not a
// series, so history buys nothing.
const modelAccessHistory = 1

// modelAccessPrefix namespaces grant keys within the bucket, leaving room for
// other per-account access state alongside them.
const modelAccessPrefix = "grants"

// accessKey returns the KV key for accountID's grant on modelID. Model IDs
// contain ':' (e.g. "anthropic.claude-3-5-sonnet-20240620-v1:0"), which NATS
// rejects in a KV key, so the model segment is base64url-encoded: unambiguous
// and reversible, which List relies on.
func accessKey(accountID, modelID string) string {
	return fmt.Sprintf("%s/%s/%s", modelAccessPrefix, accountID, base64.RawURLEncoding.EncodeToString([]byte(modelID)))
}

// modelAccessSeedPrefix namespaces the one-shot seeding markers. It is kept
// out of modelAccessPrefix so a marker can never be read back as a grant.
const modelAccessSeedPrefix = "seeded"

// accessSeedKey returns the key marking accountID's initial grants as seeded.
func accessSeedKey(accountID string) string {
	return fmt.Sprintf("%s/%s", modelAccessSeedPrefix, accountID)
}

// accountGrantPrefix returns the key prefix covering every grant held by
// accountID.
func accountGrantPrefix(accountID string) string {
	return fmt.Sprintf("%s/%s/", modelAccessPrefix, accountID)
}

// AccessResolver reports whether an account may see and invoke a model.
// Access is deny-by-default: a model is usable only where a grant exists, so
// an unconfigured deployment serves nothing rather than everything.
type AccessResolver interface {
	Granted(ctx context.Context, accountID, modelID string) (bool, error)
}

// ModelAccessStore resolves per-account model grants from the
// bedrock-model-access JetStream KV bucket.
type ModelAccessStore struct {
	js       nats.JetStreamContext
	replicas int

	mu sync.Mutex
	kv nats.KeyValue
}

var _ AccessResolver = (*ModelAccessStore)(nil)

// NewModelAccessStore constructs a ModelAccessStore over the cluster's
// JetStream context, replicated across replicas nodes.
func NewModelAccessStore(js nats.JetStreamContext, replicas int) *ModelAccessStore {
	return &ModelAccessStore{js: js, replicas: replicas}
}

// bucket lazily opens (or creates) the bedrock-model-access KV bucket,
// caching the handle, mirroring CredentialStore.bucket.
func (s *ModelAccessStore) bucket() (nats.KeyValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kv != nil {
		return s.kv, nil
	}
	kv, err := s.js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   modelAccessBucket,
		History:  modelAccessHistory,
		Replicas: s.replicas,
	})
	if err != nil {
		if kv, err = s.js.KeyValue(modelAccessBucket); err != nil {
			return nil, fmt.Errorf("open %s bucket: %w", modelAccessBucket, err)
		}
	}
	s.kv = kv
	return kv, nil
}

// Granted reports whether accountID holds a grant on modelID. The system
// account bypasses grants entirely, matching how handlers_quota exempts it
// from every quota dimension.
func (s *ModelAccessStore) Granted(_ context.Context, accountID, modelID string) (bool, error) {
	if accountID == utils.GlobalAccountID {
		return true, nil
	}
	kv, err := s.bucket()
	if err != nil {
		return false, err
	}
	switch _, err := kv.Get(accessKey(accountID, modelID)); {
	case err == nil:
		return true, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("kv get grant for %s/%s: %w", accountID, modelID, err)
	}
}

// Grant gives accountID access to modelID. It is idempotent: re-granting an
// existing grant is a no-op rather than an error.
func (s *ModelAccessStore) Grant(_ context.Context, accountID, modelID string) error {
	kv, err := s.bucket()
	if err != nil {
		return err
	}
	if _, err := kv.Put(accessKey(accountID, modelID), nil); err != nil {
		return fmt.Errorf("kv put grant for %s/%s: %w", accountID, modelID, err)
	}
	return nil
}

// Revoke removes accountID's access to modelID. Revoking a grant that does
// not exist succeeds, so callers need not check first.
func (s *ModelAccessStore) Revoke(_ context.Context, accountID, modelID string) error {
	kv, err := s.bucket()
	if err != nil {
		return err
	}
	if err := kv.Delete(accessKey(accountID, modelID)); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("kv delete grant for %s/%s: %w", accountID, modelID, err)
	}
	return nil
}

// SeedAccountGrants grants accountID every model in modelIDs, once per
// deployment, and reports whether it did. It exists so a fresh install has a
// working operator account without the deny-by-default rule being softened:
// only the account named here is seeded, and only until an operator changes
// it, since the marker means a later revoke is never undone by a restart.
//
// Grants are written before the marker, so a crash midway leaves the marker
// absent and the next start repeats the (idempotent) grants rather than
// leaving a half-seeded account. The marker itself is a conditional create:
// every node runs this at startup and only the first need win.
func (s *ModelAccessStore) SeedAccountGrants(ctx context.Context, accountID string, modelIDs []string) (bool, error) {
	kv, err := s.bucket()
	if err != nil {
		return false, err
	}

	switch _, err := kv.Get(accessSeedKey(accountID)); {
	case err == nil:
		return false, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		// Not seeded yet — fall through and seed.
	default:
		return false, fmt.Errorf("kv get seed marker for %s: %w", accountID, err)
	}

	for _, modelID := range modelIDs {
		if err := s.Grant(ctx, accountID, modelID); err != nil {
			return false, err
		}
	}

	if _, err := kv.Create(accessSeedKey(accountID), nil); err != nil && !errors.Is(err, nats.ErrKeyExists) {
		return false, fmt.Errorf("kv create seed marker for %s: %w", accountID, err)
	}
	return true, nil
}

// List returns the model IDs accountID holds grants on, in no particular
// order. Keys that do not decode are skipped rather than failing the call, so
// one malformed key cannot hide an account's whole grant set.
func (s *ModelAccessStore) List(_ context.Context, accountID string) ([]string, error) {
	kv, err := s.bucket()
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys for %s: %w", accountID, err)
	}

	prefix := accountGrantPrefix(accountID)
	var models []string
	for _, key := range keys {
		encoded, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		modelID, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		models = append(models, string(modelID))
	}
	return models, nil
}

// ModelAccessChange is the response to a grant or revoke: it echoes what was
// changed so a caller driving the API from a script can log the effect without
// a follow-up list.
type ModelAccessChange struct {
	AccountID string `json:"AccountId"`
	ModelID   string `json:"ModelId"`
}

// ModelAccessList is the response to a grant listing.
type ModelAccessList struct {
	AccountID string   `json:"AccountId"`
	ModelIDs  []string `json:"ModelIds"`
}

// denyAllAccessResolver is the zero-value fallback used wherever no
// ModelAccessStore is configured. It grants nothing, so a gateway built
// without an access store serves no models rather than all of them.
type denyAllAccessResolver struct{}

var _ AccessResolver = (*denyAllAccessResolver)(nil)

func (denyAllAccessResolver) Granted(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// DenyAllAccessResolver grants no model to any account. It is the fallback for
// a nil GatewayConfig.BedrockAccess: the insecure direction of this default is
// "nothing works", which surfaces immediately, rather than "everything works",
// which surfaces as a breach.
var DenyAllAccessResolver AccessResolver = denyAllAccessResolver{}
