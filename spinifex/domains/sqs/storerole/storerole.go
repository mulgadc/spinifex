// Package storerole names the SQS store guest's identity contract: the
// instance role it runs under, the one internal action that role is granted,
// and the pin that names the store's TLS server key. The gateway gate, the
// role grant, the store guest and the worker all read it.
package storerole

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"regexp"
)

// RoleName is the system-account instance role the SQS store VM runs under.
// Its trust policy must admit only EC2, so STS names every session after the
// instance and no caller can choose the session name the gate reads.
const RoleName = "sqsStoreRole"

const (
	// ActionDescribeStoreServerKey reports the claim, volume and any pin
	// recorded for the calling store, so it decides before creating a key.
	ActionDescribeStoreServerKey = "DescribeStoreServerKey"
	// ActionRecordStoreServerKey records the pin of the store's TLS server key.
	ActionRecordStoreServerKey = "RecordStoreServerKey"
)

// Actions lists every action the store role is granted, sorted.
func Actions() []string {
	return []string{ActionDescribeStoreServerKey, ActionRecordStoreServerKey}
}

var pinPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Pin is the lowercase hex SHA-256 of cert's SubjectPublicKeyInfo. It names
// the server key, so a reissued certificate for the same key keeps its pin.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ValidPin reports whether s has the shape Pin produces.
func ValidPin(s string) bool { return pinPattern.MatchString(s) }
