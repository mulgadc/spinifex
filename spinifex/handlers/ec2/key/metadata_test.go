package handlers_ec2_key

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two ED25519 fingerprints AWS itself returned on 2026-07-22 (account
// 744090693897, us-east-1), one per write path. They are recorded observations,
// not derivations, and they pin the rendering without needing the key material
// that produced them.
const (
	awsImportedED25519Fingerprint = "8ZVMcTKqdR5hxMjZI3t6b09EPIOE5QA6G9/Z63ejNBM="
	awsCreatedED25519Fingerprint  = "odzmoB4gyxi1jn9vGD4c+bOpt1XUX1JUyaHaWHHCO2w="
)

// A legacy ED25519 record is typed from the prefix and then re-rendered as the
// AWS values above spell it. Each stored value is the OpenSSH rendering of the
// very digest AWS reported for that key pair.
func TestDecodeKeyPairMetadata_LegacyED25519(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{
			name:   "imported",
			stored: "SHA256:8ZVMcTKqdR5hxMjZI3t6b09EPIOE5QA6G9/Z63ejNBM",
			want:   awsImportedED25519Fingerprint,
		},
		{
			name:   "created",
			stored: "SHA256:odzmoB4gyxi1jn9vGD4c+bOpt1XUX1JUyaHaWHHCO2w",
			want:   awsCreatedED25519Fingerprint,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := decodeKeyPairMetadata([]byte(`{"KeyFingerprint":"` + tt.stored + `","KeyName":"k","KeyPairId":"key-1"}`))
			require.NoError(t, err)
			assert.Equal(t, "ed25519", metadata.KeyType)
			assert.Equal(t, tt.want, *metadata.KeyFingerprint)
		})
	}
}

// A record that names its own type is left exactly as stored, whatever its
// fingerprint looks like: it was written by code that already renders as EC2
// does, so re-deriving either field could only lose information.
func TestDecodeKeyPairMetadata_KeyTypeWins(t *testing.T) {
	tests := []struct {
		name        string
		keyType     string
		fingerprint string
	}{
		{name: "ED25519", keyType: "ed25519", fingerprint: awsImportedED25519Fingerprint},
		{name: "RSA", keyType: "rsa", fingerprint: testRSAImportedFingerprint},
		{name: "PrefixedNotRewritten", keyType: "ed25519", fingerprint: testLegacyED25519Fingerprint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := decodeKeyPairMetadata([]byte(
				`{"KeyFingerprint":"` + tt.fingerprint + `","KeyName":"k","KeyPairId":"key-1","KeyType":"` + tt.keyType + `"}`))
			require.NoError(t, err)
			assert.Equal(t, tt.keyType, metadata.KeyType)
			assert.Equal(t, tt.fingerprint, *metadata.KeyFingerprint)
		})
	}
}

// Legacy RSA records carry no prefix, so they type as rsa and their colon hex is
// left untouched -- at both digest widths, 16 bytes imported and 20 created.
func TestDecodeKeyPairMetadata_LegacyRSA(t *testing.T) {
	for name, fingerprint := range map[string]string{
		"imported": testRSAImportedFingerprint,
		"created":  testRSACreatedFingerprint,
	} {
		t.Run(name, func(t *testing.T) {
			metadata, err := decodeKeyPairMetadata([]byte(`{"KeyFingerprint":"` + fingerprint + `","KeyName":"k","KeyPairId":"key-1"}`))
			require.NoError(t, err)
			assert.Equal(t, "rsa", metadata.KeyType)
			assert.Equal(t, fingerprint, *metadata.KeyFingerprint)
		})
	}
}

// A prefixed value whose body is not base64 is corruption, not a legacy
// rendering. It is reported back verbatim so the corruption stays visible.
func TestDecodeKeyPairMetadata_UndecodableFingerprint(t *testing.T) {
	metadata, err := decodeKeyPairMetadata([]byte(`{"KeyFingerprint":"SHA256:not valid base64!","KeyName":"k","KeyPairId":"key-1"}`))
	require.NoError(t, err)
	assert.Equal(t, "ed25519", metadata.KeyType)
	assert.Equal(t, "SHA256:not valid base64!", *metadata.KeyFingerprint)
}

// A record with no fingerprint at all must not be dereferenced on the way to a
// key type.
func TestDecodeKeyPairMetadata_NoFingerprint(t *testing.T) {
	metadata, err := decodeKeyPairMetadata([]byte(`{"KeyName":"k","KeyPairId":"key-1"}`))
	require.NoError(t, err)
	assert.Equal(t, "rsa", metadata.KeyType)
	assert.Nil(t, metadata.KeyFingerprint)
}

func TestDecodeKeyPairMetadata_InvalidJSON(t *testing.T) {
	_, err := decodeKeyPairMetadata([]byte("not json{{{"))
	require.Error(t, err)
}
