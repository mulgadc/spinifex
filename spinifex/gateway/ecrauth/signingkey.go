// Package gateway_ecrauth implements the ECR auth bridge: it mints and verifies
// the self-contained ES256 JWT that GetAuthorizationToken issues and the
// /v2/* registry surface accepts (Authorization: Bearer | Basic AWS:<jwt>).
// Signing keys live in the cluster-replicated JetStream KV bucket awsgw-keys
// under jwt-signing/{kid}, encrypted at rest with the IAM master key.
package gateway_ecrauth

import (
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

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
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
// The active key is the lexically-greatest kid so all nodes converge on the
// same signer without coordination.
func LoadOrCreateSigningKey(js nats.JetStreamContext, masterKey []byte, replicas int) (*SigningKey, map[string]*ecdsa.PublicKey, error) {
	if js == nil {
		return nil, nil, errors.New("ecrauth: nil JetStream context")
	}
	if len(masterKey) == 0 {
		return nil, nil, errors.New("ecrauth: empty master key")
	}

	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:   SigningBucket,
		History:  signingKeyHistory,
		Replicas: replicas,
	})
	if err != nil {
		if kv, err = js.KeyValue(SigningBucket); err != nil {
			return nil, nil, fmt.Errorf("open %s bucket: %w", SigningBucket, err)
		}
	}

	keys, err := kv.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, nil, fmt.Errorf("list signing keys: %w", err)
	}

	verify := make(map[string]*ecdsa.PublicKey)
	var active *SigningKey
	for _, name := range keys {
		if !strings.HasPrefix(name, signingKeyPrefix) {
			continue
		}
		sk, err := loadSigningKey(kv, name, masterKey)
		if err != nil {
			return nil, nil, err
		}
		verify[sk.Kid] = &sk.priv.PublicKey
		if active == nil || sk.Kid > active.Kid {
			active = sk
		}
	}

	if active == nil {
		active, err = generateSigningKey(kv, masterKey)
		if err != nil {
			return nil, nil, err
		}
		verify[active.Kid] = &active.priv.PublicKey
	}
	return active, verify, nil
}

// loadSigningKey decrypts and parses one stored signing key. The kid is the
// KV key suffix; the stored bytes are the AES-256-GCM-encrypted PKCS#8 PEM.
func loadSigningKey(kv nats.KeyValue, name string, masterKey []byte) (*SigningKey, error) {
	entry, err := kv.Get(name)
	if err != nil {
		return nil, fmt.Errorf("kv get %s: %w", name, err)
	}
	pemStr, err := handlers_iam.DecryptSecret(string(entry.Value()), masterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt signing key %s: %w", name, err)
	}
	priv, err := parseECPrivateKeyPEM(pemStr)
	if err != nil {
		return nil, err
	}
	return &SigningKey{Kid: strings.TrimPrefix(name, signingKeyPrefix), priv: priv}, nil
}

// generateSigningKey creates a fresh ES256 key, persists the encrypted PEM under
// jwt-signing/{kid}, and returns it.
func generateSigningKey(kv nats.KeyValue, masterKey []byte) (*SigningKey, error) {
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
	if _, err := kv.Put(signingKeyName(kid), []byte(ciphertext)); err != nil {
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
