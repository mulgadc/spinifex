package handlers_eks

import (
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
	"github.com/nats-io/nats.go"
)

// PublicKeyPEMFromPrivate parses a PKCS#8 private-key PEM and returns the PKIX
// public-key PEM. kube-apiserver's --service-account-key-file requires a PUBLIC
// key (ParsePublicKeysPEM rejects private-key blocks), while the matching
// --service-account-signing-key-file takes the private key; the K3s server VM
// needs both, seeded to separate files.
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

// oidcJWK and oidcJWKS are the RFC 7517 wire shapes the per-cluster JWKS
// document uses. handlers_sts decodes the same JSON via its own JWK / JWKS
// types — wire compatibility (not Go type identity) is the contract.
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

// P-256 coordinate width per SEC1 — JWK requires fixed-length base64url, not
// the variable-length big.Int byte representation.
const p256CoordLen = 32

// GenerateClusterOIDCKeypair creates a fresh ECDSA-P256 keypair for the
// cluster, PEM-marshals the private key as PKCS8, envelope-encrypts the PEM
// with masterKey, writes the ciphertext to OIDCSigningKeyKey and the JWKS
// document to OIDCJWKSKey. Returns the plaintext private-key PEM and the JWKS
// bytes so the caller (CreateCluster) can pass them straight to user-data
// without a follow-up KV read + decrypt.
func GenerateClusterOIDCKeypair(kv nats.KeyValue, clusterName string, masterKey []byte) (privPEM string, jwksBytes []byte, err error) {
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
	if _, err := kv.Put(OIDCSigningKeyKey(clusterName), []byte(ciphertext)); err != nil {
		return "", nil, fmt.Errorf("kv put %s: %w", OIDCSigningKeyKey(clusterName), err)
	}

	jwksBytes, err = marshalJWKS(&priv.PublicKey)
	if err != nil {
		return "", nil, err
	}
	if _, err := kv.Put(OIDCJWKSKey(clusterName), jwksBytes); err != nil {
		return "", nil, fmt.Errorf("kv put %s: %w", OIDCJWKSKey(clusterName), err)
	}
	return string(pemBlock), jwksBytes, nil
}

// LoadClusterOIDCPrivateKey reads the encrypted PEM from KV, decrypts with
// masterKey, parses it to *ecdsa.PrivateKey. Returns ErrClusterNotFound when
// the key is absent so DeleteCluster idempotency works.
func LoadClusterOIDCPrivateKey(kv nats.KeyValue, clusterName string, masterKey []byte) (*ecdsa.PrivateKey, error) {
	if clusterName == "" {
		return nil, errors.New("eks: LoadClusterOIDCPrivateKey empty cluster name")
	}
	if len(masterKey) == 0 {
		return nil, errors.New("eks: LoadClusterOIDCPrivateKey empty master key")
	}
	entry, err := kv.Get(OIDCSigningKeyKey(clusterName))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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

// ZeroizeClusterOIDCKey overwrites the encrypted private-key blob with zeros
// of the same length, then purges the key. DeleteCluster calls this first so
// a crash after this point leaves no recoverable key material. Purge (not
// Delete) drops the key's entire revision history — a Delete leaves a tombstone
// over prior revisions, so the original encrypted PEM would survive in history
// until age/limit rolls it off. The account bucket is History=1 regardless
// (see store.go), so this is defense-in-depth against a widened history.
// Absent key is a no-op — supports idempotent DeleteCluster retries.
func ZeroizeClusterOIDCKey(kv nats.KeyValue, clusterName string) error {
	if clusterName == "" {
		return errors.New("eks: ZeroizeClusterOIDCKey empty cluster name")
	}
	key := OIDCSigningKeyKey(clusterName)
	entry, err := kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("kv get %s: %w", key, err)
	}
	zero := make([]byte, len(entry.Value()))
	if _, err := kv.Put(key, zero); err != nil {
		return fmt.Errorf("kv put zero %s: %w", key, err)
	}
	if err := kv.Purge(key); err != nil {
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

	xb := padCoord(pub.X.Bytes())
	yb := padCoord(pub.Y.Bytes())

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

func padCoord(b []byte) []byte {
	if len(b) >= p256CoordLen {
		return b
	}
	out := make([]byte, p256CoordLen)
	copy(out[p256CoordLen-len(b):], b)
	return out
}
