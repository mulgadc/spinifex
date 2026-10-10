// Package admin holds what non-operator code still needs from the former admin
// package: default account identifiers and AWS credential generators (awaiting
// IAM owners) and system-image promotion (awaiting domains/ec2/image).
package admin

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
)

// DefaultAccountID returns the default admin account ID (000000000001).
// This is the first human-facing account created during bootstrap.
func DefaultAccountID() string {
	return "000000000001"
}

// DefaultAccountName returns the default admin account name ("spinifex").
func DefaultAccountName() string {
	return "spinifex"
}

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
