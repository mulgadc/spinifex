package function

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

func envAAD(accountID, functionID string) []byte {
	return []byte("lambda-env/v1|" + accountID + "|" + functionID)
}

// SealEnv encrypts the variables as base64(nonce||ciphertext), bound to the
// account and function. No variables yield "".
func SealEnv(aead cipher.AEAD, accountID, functionID string, vars map[string]string) (string, error) {
	if len(vars) == 0 {
		return "", nil
	}
	plain, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("encode environment: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("environment nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, plain, envAAD(accountID, functionID))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenEnv reverses SealEnv. A blob sealed for another account or function fails.
func OpenEnv(aead cipher.AEAD, accountID, functionID, sealed string) (map[string]string, error) {
	if sealed == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	n := aead.NonceSize()
	if len(raw) < n+aead.Overhead() {
		return nil, errors.New("environment blob too short")
	}
	plain, err := aead.Open(nil, raw[:n], raw[n:], envAAD(accountID, functionID))
	if err != nil {
		return nil, fmt.Errorf("open environment: %w", err)
	}
	var vars map[string]string
	if err := json.Unmarshal(plain, &vars); err != nil {
		return nil, fmt.Errorf("decode environment: %w", err)
	}
	return vars, nil
}
