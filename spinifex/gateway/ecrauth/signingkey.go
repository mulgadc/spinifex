// Package gateway_ecrauth implements the ECR auth bridge: it mints and verifies
// the self-contained ES256 JWT that GetAuthorizationToken issues and the
// /v2/* registry surface accepts (Authorization: Bearer | Basic AWS:<jwt>).
// Signing keys live in the cluster-replicated JetStream KV bucket awsgw-keys
// under jwt-signing/{kid}, encrypted at rest with the IAM master key.
package gateway_ecrauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// SigningBucket is the cluster-replicated KV bucket holding awsgw signing keys.
	SigningBucket = "awsgw-keys"
	// signingKeyPrefix namespaces JWT signing keys within SigningBucket.
	signingKeyPrefix = "jwt-signing/"
	// signingKeyHistory keeps one revision; rotation writes new kids, not revisions.
	signingKeyHistory = 1
)

// SigningKey is an ES256 (P-256) private key plus its key id. kid is the
// base64url SHA-256 of the SPKI DER, matching the EKS OIDC convention so the
// same JWK tooling applies.
type SigningKey struct {
	Kid  string
	priv *ecdsa.PrivateKey
}

func signingKeyName(kid string) string { return signingKeyPrefix + kid }

// keyMeta is a stored signing key's id and KV creation time. created is assigned
// by the JetStream server, so every node orders keys identically — the rotator
// uses it to pick the active (newest) key and to drive retention pruning.
type keyMeta struct {
	kid     string
	created time.Time
}

// newerKey reports whether a should win over b as the active key: later created
// wins, ties broken by lexically-greater kid for cross-node determinism.
func newerKey(a, b keyMeta) bool {
	if !a.created.Equal(b.created) {
		return a.created.After(b.created)
	}
	return a.kid > b.kid
}

// kidFor derives the key id from a public key as base64url(sha256(SPKI DER)).
func kidFor(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal pkix public key: %w", err)
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// LoadOrCreateSigningKey opens (or creates) the awsgw-keys KV bucket, loads
// every stored signing key into the kid->public-key verification set, and
// returns the active signing key. On first run it generates and persists one.
// The active key is the newest by KV creation time (ties broken by kid), so all
// nodes converge on the same signer and a freshly rotated key takes over.
func LoadOrCreateSigningKey(ctx context.Context, js jetstream.JetStream, masterKey []byte, replicas int) (*SigningKey, map[string]*ecdsa.PublicKey, error) {
	kv, err := openSigningBucket(ctx, js, masterKey, replicas)
	if err != nil {
		return nil, nil, err
	}
	active, verify, _, err := reloadKeys(ctx, kv, masterKey)
	if err != nil {
		return nil, nil, err
	}
	if active == nil {
		active, err = generateSigningKey(ctx, kv, masterKey)
		if err != nil {
			return nil, nil, err
		}
		verify[active.Kid] = &active.priv.PublicKey
	}
	return active, verify, nil
}

// openSigningBucket validates inputs and returns the awsgw-keys KV handle,
// creating the cluster-replicated bucket on first use.
func openSigningBucket(ctx context.Context, js jetstream.JetStream, masterKey []byte, replicas int) (jetstream.KeyValue, error) {
	if js == nil {
		return nil, errors.New("ecrauth: nil JetStream context")
	}
	if len(masterKey) == 0 {
		return nil, errors.New("ecrauth: empty master key")
	}
	return kvutil.GetOrCreateBucketWithReplicas(ctx, js, SigningBucket, signingKeyHistory, replicas)
}

// reloadKeys reads every stored signing key, returning the verify set, each
// key's metadata, and the active (newest) decrypted key. active is nil for an
// empty bucket. Used by both startup and the rotation scheduler.
func reloadKeys(ctx context.Context, kv jetstream.KeyValue, masterKey []byte) (active *SigningKey, verify map[string]*ecdsa.PublicKey, metas []keyMeta, err error) {
	names, err := kvutil.Keys(ctx, kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil, nil, fmt.Errorf("list signing keys: %w", err)
	}

	verify = make(map[string]*ecdsa.PublicKey)
	var activeMeta keyMeta
	for _, name := range names {
		if !strings.HasPrefix(name, signingKeyPrefix) {
			continue
		}
		entry, err := kv.Get(ctx, name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("kv get %s: %w", name, err)
		}
		sk, err := decodeSigningKey(strings.TrimPrefix(name, signingKeyPrefix), entry.Value(), masterKey)
		if err != nil {
			return nil, nil, nil, err
		}
		verify[sk.Kid] = &sk.priv.PublicKey
		m := keyMeta{kid: sk.Kid, created: entry.Created()}
		metas = append(metas, m)
		if active == nil || newerKey(m, activeMeta) {
			active, activeMeta = sk, m
		}
	}
	return active, verify, metas, nil
}

// deleteSigningKey removes a rotated-out key from the bucket once its retention
// window has elapsed.
func deleteSigningKey(ctx context.Context, kv jetstream.KeyValue, kid string) error {
	if err := kv.Delete(ctx, signingKeyName(kid)); err != nil {
		return fmt.Errorf("kv delete %s: %w", signingKeyName(kid), err)
	}
	return nil
}

// decodeSigningKey decrypts and parses one stored signing key's encrypted PKCS#8
// PEM bytes into a SigningKey under kid.
func decodeSigningKey(kid string, ciphertext []byte, masterKey []byte) (*SigningKey, error) {
	pemStr, err := handlers_iam.DecryptSecret(string(ciphertext), masterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt signing key %s: %w", kid, err)
	}
	priv, err := parseECPrivateKeyPEM(pemStr)
	if err != nil {
		return nil, err
	}
	return &SigningKey{Kid: kid, priv: priv}, nil
}

// generateSigningKey creates a fresh ES256 key, persists the encrypted PEM under
// jwt-signing/{kid}, and returns it.
func generateSigningKey(ctx context.Context, kv jetstream.KeyValue, masterKey []byte) (*SigningKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ES256 key: %w", err)
	}
	kid, err := kidFor(&priv.PublicKey)
	if err != nil {
		return nil, err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal pkcs8 private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	ciphertext, err := handlers_iam.EncryptSecret(string(pemBytes), masterKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt signing key: %w", err)
	}
	if _, err := kv.Put(ctx, signingKeyName(kid), []byte(ciphertext)); err != nil {
		return nil, fmt.Errorf("kv put %s: %w", signingKeyName(kid), err)
	}
	return &SigningKey{Kid: kid, priv: priv}, nil
}

// parseECPrivateKeyPEM decodes a PKCS#8 PEM into a P-256 ECDSA private key.
func parseECPrivateKeyPEM(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("ecrauth: no PEM block in signing key")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkcs8 private key: %w", err)
	}
	priv, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ecrauth: unexpected key type %T (want *ecdsa.PrivateKey)", keyAny)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("ecrauth: unexpected curve %s (want P-256)", priv.Curve.Params().Name)
	}
	return priv, nil
}
