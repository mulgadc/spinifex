package handlers_iam

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mulgadc/predastore/pkg/masterkey"
)

const masterKeySize = 32 // AES-256

// BootstrapVersion is the current bootstrap.json config version.
const BootstrapVersion = "1.0"

// GenerateMasterKey returns 32 cryptographically random bytes suitable
// for use as an AES-256-GCM key.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, masterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	return key, nil
}

// LoadMasterKey reads a master key from disk and validates it is exactly 32 bytes.
func LoadMasterKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", masterKeySize, len(key))
	}
	return key, nil
}

// SaveMasterKey writes a master key to disk with 0600 permissions.
func SaveMasterKey(path string, key []byte) error {
	if len(key) != masterKeySize {
		return fmt.Errorf("master key must be %d bytes, got %d", masterKeySize, len(key))
	}
	if err := os.WriteFile(path, key, 0600); err != nil {
		return fmt.Errorf("write master key: %w", err)
	}
	return nil
}

// EncryptSecret encrypts a plaintext secret with AES-256-GCM, returning
// base64(nonce + ciphertext + tag). It delegates to predastore's masterkey so
// the wire format stays byte-for-byte identical across services.
func EncryptSecret(plaintext string, key []byte) (string, error) {
	k, err := masterkey.New(key)
	if err != nil {
		return "", err
	}
	return k.Encrypt(plaintext)
}

// Decrypter holds a pre-computed AES-256-GCM cipher for repeated decryption.
// Create once at startup with NewDecrypter and reuse across requests.
type Decrypter struct {
	key *masterkey.Key
}

// NewDecrypter creates a Decrypter with a pre-computed AES-GCM cipher from the given key.
func NewDecrypter(key []byte) (*Decrypter, error) {
	k, err := masterkey.New(key)
	if err != nil {
		return nil, err
	}
	return &Decrypter{key: k}, nil
}

// Decrypt decrypts a base64-encoded AES-256-GCM ciphertext using the pre-computed cipher.
func (d *Decrypter) Decrypt(ciphertext string) (string, error) {
	return d.key.Decrypt(ciphertext)
}

// DecryptSecret decrypts a base64-encoded AES-256-GCM ciphertext.
func DecryptSecret(ciphertext string, key []byte) (string, error) {
	k, err := masterkey.New(key)
	if err != nil {
		return "", err
	}
	return k.Decrypt(ciphertext)
}

// SaveBootstrapData writes bootstrap data as JSON to disk with 0600 permissions.
// It always sets the version to BootstrapVersion.
func SaveBootstrapData(path string, data *BootstrapData) error {
	data.Version = BootstrapVersion
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal bootstrap data: %w", err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		return fmt.Errorf("write bootstrap data: %w", err)
	}
	return nil
}

// LoadBootstrapData reads and parses a bootstrap JSON file from disk.
func LoadBootstrapData(path string) (*BootstrapData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bd BootstrapData
	if err := json.Unmarshal(data, &bd); err != nil {
		return nil, fmt.Errorf("parse bootstrap data: %w", err)
	}
	if bd.Version == "" {
		return nil, fmt.Errorf("bootstrap data missing version field")
	}
	return &bd, nil
}
