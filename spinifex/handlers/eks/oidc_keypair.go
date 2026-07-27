package handlers_eks

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go/jetstream"
)

// PublicKeyPEMFromPrivate parses a PKCS#8 private-key PEM and returns the PKIX public-key PEM.
// kube-apiserver --service-account-key-file requires a public key; signing uses the private key.
func PublicKeyPEMFromPrivate(privPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return "", errors.New("eks: PublicKeyPEMFromPrivate: no PEM block in private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse pkcs8 private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return "", errors.New("eks: PublicKeyPEMFromPrivate: private key is not a crypto.Signer")
	}
	pubDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return "", fmt.Errorf("marshal pkix public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})), nil
}

// oidcJWK and oidcJWKS are the RFC 7517 wire shapes for the per-cluster JWKS document.
type oidcJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

type oidcJWKS struct {
	Keys []oidcJWK `json:"keys"`
}

// p256CoordLen is the P-256 coordinate width; JWK requires fixed-length base64url.
const p256CoordLen = 32

// GenerateClusterOIDCKeypair creates an ECDSA-P256 keypair, encrypts the private key PEM
// into KV, and writes the JWKS document. Returns plaintext PEM + JWKS for immediate use.
func GenerateClusterOIDCKeypair(ctx context.Context, kv jetstream.KeyValue, clusterName string, masterKey []byte) (privPEM string, jwksBytes []byte, err error) {
	if clusterName == "" {
		return "", nil, errors.New("eks: GenerateClusterOIDCKeypair empty cluster name")
	}
	if len(masterKey) == 0 {
		return "", nil, errors.New("eks: GenerateClusterOIDCKeypair empty master key")
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate ECDSA-P256 key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, fmt.Errorf("marshal pkcs8 private key: %w", err)
	}
	pemBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	ciphertext, err := handlers_iam.EncryptSecret(string(pemBlock), masterKey)
	if err != nil {
		return "", nil, fmt.Errorf("encrypt OIDC private key: %w", err)
	}
	if _, err := kv.Put(ctx, OIDCSigningKeyKey(clusterName), []byte(ciphertext)); err != nil {
		return "", nil, fmt.Errorf("kv put %s: %w", OIDCSigningKeyKey(clusterName), err)
	}

	jwksBytes, err = marshalJWKS(&priv.PublicKey)
	if err != nil {
		return "", nil, err
	}
	if _, err := kv.Put(ctx, OIDCJWKSKey(clusterName), jwksBytes); err != nil {
		return "", nil, fmt.Errorf("kv put %s: %w", OIDCJWKSKey(clusterName), err)
	}
	return string(pemBlock), jwksBytes, nil
}

// LoadClusterOIDCPrivateKey decrypts and parses the cluster OIDC private key from KV.
// Returns ErrClusterNotFound if absent (supports idempotent DeleteCluster).
func LoadClusterOIDCPrivateKey(ctx context.Context, kv jetstream.KeyValue, clusterName string, masterKey []byte) (*ecdsa.PrivateKey, error) {
	if clusterName == "" {
		return nil, errors.New("eks: LoadClusterOIDCPrivateKey empty cluster name")
	}
	if len(masterKey) == 0 {
		return nil, errors.New("eks: LoadClusterOIDCPrivateKey empty master key")
	}
	entry, err := kv.Get(ctx, OIDCSigningKeyKey(clusterName))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrClusterNotFound
		}
		return nil, fmt.Errorf("kv get %s: %w", OIDCSigningKeyKey(clusterName), err)
	}
	pemStr, err := handlers_iam.DecryptSecret(string(entry.Value()), masterKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt OIDC private key: %w", err)
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("eks: PEM decode produced no block")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse pkcs8 private key: %w", err)
	}
	priv, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("eks: unexpected key type %T (want *ecdsa.PrivateKey)", keyAny)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("eks: unexpected curve %s (want P-256)", priv.Curve.Params().Name)
	}
	return priv, nil
}

// ZeroizeClusterOIDCKey overwrites the encrypted key blob with zeros then purges it.
// Purge (not Delete) drops all revision history, leaving no recoverable key material.
// Absent key is a no-op for idempotent DeleteCluster retries.
func ZeroizeClusterOIDCKey(ctx context.Context, kv jetstream.KeyValue, clusterName string) error {
	if clusterName == "" {
		return errors.New("eks: ZeroizeClusterOIDCKey empty cluster name")
	}
	key := OIDCSigningKeyKey(clusterName)
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	zero := make([]byte, len(entry.Value()))
	if _, err := kv.Put(ctx, key, zero); err != nil {
		return fmt.Errorf("kv put zero %s: %w", key, err)
	}
	if err := kv.Purge(ctx, key); err != nil {
		return fmt.Errorf("kv purge %s: %w", key, err)
	}
	return nil
}

// marshalJWKS builds the single-key JWKS document for an ECDSA P-256
// public key. kid is the base64url SHA-256 of the SPKI DER.
func marshalJWKS(pub *ecdsa.PublicKey) ([]byte, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key DER: %w", err)
	}
	kidHash := sha256.Sum256(pubDER)
	kid := base64.RawURLEncoding.EncodeToString(kidHash[:])

	// The document below hardcodes P-256, and the point is sliced at P-256 widths.
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("eks: unexpected curve %s (want P-256)", pub.Curve.Params().Name)
	}

	// SEC1 uncompressed point: 0x04 || X || Y, both coordinates fixed-width.
	// That is exactly the left-padding RFC 7518 requires of the JWK x/y members,
	// which big.Int.Bytes() would strip from a coordinate with a leading zero.
	point, err := pub.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encode public key point: %w", err)
	}
	xb := point[1 : 1+p256CoordLen]
	yb := point[1+p256CoordLen:]

	doc := oidcJWKS{Keys: []oidcJWK{{
		Kty: "EC",
		Kid: kid,
		Use: "sig",
		Alg: "ES256",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(xb),
		Y:   base64.RawURLEncoding.EncodeToString(yb),
	}}}
	out, err := json.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshal JWKS: %w", err)
	}
	return out, nil
}
