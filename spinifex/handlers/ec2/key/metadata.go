package handlers_ec2_key

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// openSSHSHA256Prefix is what ssh.FingerprintSHA256 puts in front of the digest.
// EC2 never emits it, so a stored fingerprint carrying it was written by the
// earlier code that rendered ED25519 keys the OpenSSH way.
const openSSHSHA256Prefix = "SHA256:"

// keyPairMetadata is the stored form of a key pair, written to
// keys/<accountID>/<keyPairID>.json. The JSON keys match the field names
// ec2.CreateKeyPairOutput marshalled under, which this record was serialised as
// before it had a type of its own, so records already on disk still decode. An
// empty KeyType is exactly that case, and is what triggers the legacy upgrade in
// decodeKeyPairMetadata.
type keyPairMetadata struct {
	KeyFingerprint *string    `json:"KeyFingerprint"`
	KeyName        *string    `json:"KeyName"`
	KeyPairId      *string    `json:"KeyPairId"`
	KeyType        string     `json:"KeyType,omitempty"`
	Tags           []*ec2.Tag `json:"Tags"`
}

// decodeKeyPairMetadata unmarshals a stored record and upgrades it to current
// form. Records written before KeyType was persisted are typed by the "SHA256:"
// prefix their ED25519 fingerprints still carry, and that fingerprint is then
// re-rendered in the AWS form. Resolving the type before rewriting the value it
// was inferred from is what keeps the upgrade lossless.
func decodeKeyPairMetadata(body []byte) (*keyPairMetadata, error) {
	var metadata keyPairMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, err
	}

	// A record that names its own type was written by code that also renders
	// fingerprints as EC2 does, so there is nothing to upgrade.
	if metadata.KeyType != "" {
		return &metadata, nil
	}

	// Only ED25519 was ever stored prefixed; every other legacy fingerprint is
	// the colon hex of an RSA digest and is left exactly as it was found.
	if metadata.KeyFingerprint != nil && strings.HasPrefix(*metadata.KeyFingerprint, openSSHSHA256Prefix) {
		metadata.KeyType = "ed25519"
		metadata.KeyFingerprint = aws.String(awsFingerprint(*metadata.KeyFingerprint))
	} else {
		metadata.KeyType = "rsa"
	}

	return &metadata, nil
}

// awsFingerprint re-renders an OpenSSH SHA-256 fingerprint the way EC2 spells
// it: the "SHA256:" prefix dropped and the base64 padding OpenSSH omits
// restored. Anything that is not a raw base64 SHA-256 digest is corruption
// rather than a legacy rendering, and is returned verbatim instead of guessed
// at. The length check is what makes that true of a truncated value: a short
// body still decodes cleanly, so without it a record holding "SHA256:" alone
// would re-render as the empty string and the next write would persist that
// over the stored value.
func awsFingerprint(fingerprint string) string {
	digest, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(fingerprint, openSSHSHA256Prefix))
	if err != nil || len(digest) != sha256.Size {
		return fingerprint
	}
	return base64.StdEncoding.EncodeToString(digest)
}
