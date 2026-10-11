// Package awscredentials generates AWS-format access key IDs and secret keys.
// It is a stateless format primitive: it issues, stores and authorises nothing.
package awscredentials

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
)

// GenerateAWSAccessKey generates an AWS-style access key (AKIA + 16 random alphanumeric chars).
func GenerateAWSAccessKey() (string, error) {
	const prefix = "AKIA"
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 16

	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("crypto/rand failure: %w", err)
		}
		result[i] = charset[num.Int64()]
	}

	return prefix + string(result), nil
}

// GenerateAWSSecretKey generates a 40-character base64-encoded AWS-style secret key.
func GenerateAWSSecretKey() (string, error) {
	bytes := make([]byte, 30) // 30 bytes = 40 chars in base64
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bytes), nil
}
