package handlers_ec2_key

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBucket    = "test-bucket"
	testAccountID = "123456789"
)

// Valid ed25519 public key for import tests
const testED25519PubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// Valid RSA public key for import tests (generated locally, 2048-bit)
const testRSAPubKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDP9LrByKWpgbX+prxBwnlf7lrmI5AfDwuiCofuvOAzt9/PwIDMMIAhlvlpm09jjOuuH/MRQApJB5A714Auv+hBKVK0lCq9KcTrnKZOpRj2aGgIZgaoO6P/POoZc+kBf9Y/GP18DCKc4y/XyBsp69dPP6XRdYBlEdeIeVZdgCPYrM7s5FjT7aML2ba2Y2EvH116hPxv+nJZGwM6yqWxWRyTOoNMMTAfNYmoKkNy2zC1FARUyqDwumJ2z5Fvo4ZdN1qoFPOsfPc3iB0NUtSZbLU1awScvHb0rwR5PRnelTZ3Nbkw8I8A0IAhBTE5veW9D38hDIJhRd4nW73BUhgmzDL7"

func newTestKeyService() (*KeyServiceImpl, *objectstore.MemoryObjectStore) {
	store := objectstore.NewMemoryObjectStore()
	svc := NewKeyServiceImplWithStore(store, testBucket)
	return svc, store
}

// requireSSHKeygen skips the test if ssh-keygen is not available.
func requireSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available, skipping")
	}
}

// importTestKey is a helper that imports an ed25519 key and returns the output.
func importTestKey(t *testing.T, svc *KeyServiceImpl, keyName string) *ec2.ImportKeyPairOutput {
	t.Helper()
	out, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(keyName),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	return out
}

// ============================================================
// CreateKeyPair Tests
// ============================================================

func TestCreateKeyPair_ED25519(t *testing.T) {
	requireSSHKeygen(t)
	svc, store := newTestKeyService()

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("my-ed25519-key"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "my-ed25519-key", *out.KeyName)
	assert.NotEmpty(t, *out.KeyPairId)
	assert.True(t, strings.HasPrefix(*out.KeyPairId, "key-"))
	assert.NotEmpty(t, *out.KeyFingerprint)
	assert.True(t, strings.HasPrefix(*out.KeyFingerprint, "SHA256:"), "ed25519 fingerprint should be SHA256 format")
	assert.NotEmpty(t, *out.KeyMaterial)
	assert.Contains(t, *out.KeyMaterial, "PRIVATE KEY")

	// Verify public key stored in S3
	keyPath := "keys/" + testAccountID + "/my-ed25519-key"
	getOut, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, getOut)

	// Verify metadata stored in S3
	metaPath := "keys/" + testAccountID + "/" + *out.KeyPairId + ".json"
	metaOut, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, metaOut)
}

func TestCreateKeyPair_RSA(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("my-rsa-key"),
		KeyType: aws.String("rsa"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "my-rsa-key", *out.KeyName)
	assert.NotEmpty(t, *out.KeyFingerprint)
	// RSA fingerprint is MD5 hex (no "SHA256:" prefix, no colons in our format)
	assert.False(t, strings.HasPrefix(*out.KeyFingerprint, "SHA256:"), "RSA fingerprint should not be SHA256 format")
}

func TestCreateKeyPair_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(nil, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreateKeyPair_MissingKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreateKeyPair_InvalidKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("invalid key name!@#"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairFormat, err.Error())
}

func TestCreateKeyPair_Duplicate(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	_, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("dup-key"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("dup-key"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairDuplicate, err.Error())
}

func TestCreateKeyPair_InvalidKeyType(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String("bad-type-key"),
		KeyType: aws.String("dsa"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

// ============================================================
// ImportKeyPair Tests
// ============================================================

func TestImportKeyPair_Success_ED25519(t *testing.T) {
	svc, store := newTestKeyService()

	out, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported-ed25519"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "imported-ed25519", *out.KeyName)
	assert.NotEmpty(t, *out.KeyPairId)
	assert.True(t, strings.HasPrefix(*out.KeyPairId, "key-"))
	assert.NotEmpty(t, *out.KeyFingerprint)
	assert.True(t, strings.HasPrefix(*out.KeyFingerprint, "SHA256:"))

	// Verify public key stored in S3
	keyPath := "keys/" + testAccountID + "/imported-ed25519"
	getOut, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, getOut)

	// Verify metadata stored in S3
	metaPath := "keys/" + testAccountID + "/" + *out.KeyPairId + ".json"
	metaOut, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, metaOut)
}

func TestImportKeyPair_Success_RSA(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported-rsa"),
		PublicKeyMaterial: []byte(testRSAPubKey),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "imported-rsa", *out.KeyName)
	assert.NotEmpty(t, *out.KeyFingerprint)
	assert.False(t, strings.HasPrefix(*out.KeyFingerprint, "SHA256:"), "RSA fingerprint should be MD5 format")
}

func TestImportKeyPair_Duplicate(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String("dup-import"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String("dup-import"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairDuplicate, err.Error())
}

func TestImportKeyPair_InvalidKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String("bad name with spaces!"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairFormat, err.Error())
}

func TestImportKeyPairInvalidKeyFormat(t *testing.T) {
	svc, _ := newTestKeyService()

	tests := []struct {
		name           string
		publicKey      string
		expectedErrMsg string
	}{
		{
			name:           "SingleFieldNoKeyData",
			publicKey:      "ssh-rsa",
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			name:           "UnsupportedAlgorithm",
			publicKey:      "ssh-dss AAAAB3NzaC1kc3MAAACB",
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			name:           "InvalidBase64",
			publicKey:      "ssh-rsa not-valid-base64!!!",
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			name:           "EmptyKeyData",
			publicKey:      "ssh-ed25519 ",
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.ImportKeyPair(&ec2.ImportKeyPairInput{
				KeyName:           aws.String("test-key"),
				PublicKeyMaterial: []byte(tt.publicKey),
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, tt.expectedErrMsg, err.Error())
		})
	}
}

// ============================================================
// DeleteKeyPair Tests
// ============================================================

func TestDeleteKeyPair_ByKeyName(t *testing.T) {
	svc, store := newTestKeyService()

	imported := importTestKey(t, svc, "to-delete-by-name")

	result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyName: aws.String("to-delete-by-name"),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify public key removed from S3
	keyPath := "keys/" + testAccountID + "/to-delete-by-name"
	_, err = store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	assert.Error(t, err, "public key should be deleted")

	// Verify metadata removed from S3
	metaPath := "keys/" + testAccountID + "/" + *imported.KeyPairId + ".json"
	_, err = store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	assert.Error(t, err, "metadata should be deleted")
}

func TestDeleteKeyPair_ByKeyPairId(t *testing.T) {
	svc, store := newTestKeyService()

	imported := importTestKey(t, svc, "to-delete-by-id")

	result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyPairId: imported.KeyPairId,
	}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify public key removed from S3
	keyPath := "keys/" + testAccountID + "/to-delete-by-id"
	_, err = store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	assert.Error(t, err, "public key should be deleted")

	// Verify metadata removed from S3
	metaPath := "keys/" + testAccountID + "/" + *imported.KeyPairId + ".json"
	_, err = store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	assert.Error(t, err, "metadata should be deleted")
}

func TestDeleteKeyPairIdempotent(t *testing.T) {
	svc, _ := newTestKeyService()

	t.Run("NonExistentKeyName", func(t *testing.T) {
		result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
			KeyName: aws.String("no-such-key"),
		}, testAccountID)
		require.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("NonExistentKeyPairId", func(t *testing.T) {
		result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
			KeyPairId: aws.String("key-0123456789abcdef0"),
		}, testAccountID)
		require.NoError(t, err)
		assert.NotNil(t, result)
	})
}

func TestDeleteKeyPair_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(nil, testAccountID)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDeleteKeyPair_EmptyNameAndId(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDeleteKeyPair_InvalidKeyPairIdFormat(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(&ec2.DeleteKeyPairInput{
		KeyPairId: aws.String("bad id format!!!"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairFormat, err.Error())
}

// ============================================================
// DescribeKeyPairs Tests
// ============================================================

func TestDescribeKeyPairs_Empty(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.KeyPairs)
}

func TestDescribeKeyPairs_AllKeys(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "key-alpha")
	importTestKey(t, svc, "key-beta")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Len(t, out.KeyPairs, 2)

	names := make(map[string]bool)
	for _, kp := range out.KeyPairs {
		names[*kp.KeyName] = true
		assert.NotEmpty(t, *kp.KeyPairId)
		assert.NotEmpty(t, *kp.KeyFingerprint)
		assert.NotEmpty(t, *kp.KeyType)
	}
	assert.True(t, names["key-alpha"])
	assert.True(t, names["key-beta"])
}

func TestDescribeKeyPairs_FilterByKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "find-me")
	importTestKey(t, svc, "ignore-me")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String("find-me")},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.KeyPairs, 1)
	assert.Equal(t, "find-me", *out.KeyPairs[0].KeyName)
}

func TestDescribeKeyPairs_FilterByKeyPairId(t *testing.T) {
	svc, _ := newTestKeyService()

	imported := importTestKey(t, svc, "find-by-id")
	importTestKey(t, svc, "other-key")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyPairIds: []*string{imported.KeyPairId},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out.KeyPairs, 1)
	assert.Equal(t, "find-by-id", *out.KeyPairs[0].KeyName)
}

func TestDescribeKeyPairs_FilterNoMatch(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "exists")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String("does-not-exist")},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.KeyPairs)
}

func TestDescribeKeyPairs_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.DescribeKeyPairs(nil, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.KeyPairs)
}

// ============================================================
// ValidateKeyPairExists Tests
// ============================================================

func TestValidateKeyPairExists_Found(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "existing-key")

	err := svc.ValidateKeyPairExists(testAccountID, "existing-key")
	assert.NoError(t, err)
}

func TestValidateKeyPairExists_NotFound(t *testing.T) {
	svc, _ := newTestKeyService()

	err := svc.ValidateKeyPairExists(testAccountID, "ghost-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

// ============================================================
// GetPublicKeyMaterial Tests
// ============================================================

func TestGetPublicKeyMaterial_Success(t *testing.T) {
	svc, store := newTestKeyService()

	// Stored exactly as ssh-keygen writes it: the key line plus a trailing newline.
	keyPath := "keys/" + testAccountID + "/imds-key"
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
		Body:   strings.NewReader(testED25519PubKey + "\n"),
	})
	require.NoError(t, err)

	material, err := svc.GetPublicKeyMaterial(testAccountID, "imds-key")
	require.NoError(t, err)
	// Normalised to a single clean line — no trailing newline (the IMDS handler
	// adds exactly one).
	assert.Equal(t, testED25519PubKey, material)
}

func TestGetPublicKeyMaterial_NotFound(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.GetPublicKeyMaterial(testAccountID, "ghost-key")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

func TestGetPublicKeyMaterial_RoundTripsImport(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "imported-imds")

	material, err := svc.GetPublicKeyMaterial(testAccountID, "imported-imds")
	require.NoError(t, err)
	assert.Equal(t, testED25519PubKey, material)
}

func TestGetPublicKeyMaterial_EmptyObject(t *testing.T) {
	svc, store := newTestKeyService()

	keyPath := "keys/" + testAccountID + "/blank-key"
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
		Body:   strings.NewReader("   \n"),
	})
	require.NoError(t, err)

	// An empty stored object is corruption, not absence: it must not map to the
	// NotFound code (which the IMDS handler would render as a keyless 404).
	_, err = svc.GetPublicKeyMaterial(testAccountID, "blank-key")
	require.Error(t, err)
	assert.NotEqual(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

// ============================================================
// formatFingerprint Tests
// ============================================================

// ============================================================
// getKeyNameFromKeyPairId Tests
// ============================================================

func TestGetKeyNameFromKeyPairId_NotFound(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.getKeyNameFromKeyPairId(testAccountID, "key-nonexistent")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

func TestGetKeyNameFromKeyPairId_InvalidJSON(t *testing.T) {
	svc, store := newTestKeyService()

	// Seed store with garbage data at the metadata path
	metadataPath := "keys/" + testAccountID + "/key-badjson.json"
	_, err := store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metadataPath),
		Body:   strings.NewReader("this is not valid json{{{"),
	})
	require.NoError(t, err)

	_, err = svc.getKeyNameFromKeyPairId(testAccountID, "key-badjson")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal metadata")
}

func TestGetKeyNameFromKeyPairId_MissingKeyName(t *testing.T) {
	svc, store := newTestKeyService()

	// Seed store with valid JSON but missing KeyName field
	metadataPath := "keys/" + testAccountID + "/key-noname.json"
	jsonData, err := json.Marshal(map[string]string{
		"KeyPairId":      "key-noname",
		"KeyFingerprint": "SHA256:abc123",
	})
	require.NoError(t, err)

	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metadataPath),
		Body:   strings.NewReader(string(jsonData)),
	})
	require.NoError(t, err)

	_, err = svc.getKeyNameFromKeyPairId(testAccountID, "key-noname")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid metadata: missing KeyName")
}

func TestGetKeyNameFromKeyPairId_Success(t *testing.T) {
	svc, _ := newTestKeyService()

	// Import a key, then look it up by keyPairId
	imported := importTestKey(t, svc, "lookup-test")

	keyName, err := svc.getKeyNameFromKeyPairId(testAccountID, *imported.KeyPairId)
	require.NoError(t, err)
	assert.Equal(t, "lookup-test", keyName)
}

// ============================================================
// formatFingerprint Tests
// ============================================================

func TestFormatFingerprint_MD5(t *testing.T) {
	hash := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	result := formatFingerprint(hash, "MD5")
	assert.Equal(t, "aabbccddeeff00112233445566778899", result)
	// Should be lowercase hex, no colons
	assert.False(t, strings.Contains(result, ":"))
	assert.Equal(t, strings.ToLower(result), result)
}

func TestFormatFingerprint_SHA256(t *testing.T) {
	hash := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	result := formatFingerprint(hash, "SHA256")
	assert.True(t, strings.HasPrefix(result, "SHA256:"))
	// Should be raw base64 (no padding)
	assert.False(t, strings.HasSuffix(result, "="))
}

func TestFormatFingerprint_EmptyHash(t *testing.T) {
	result := formatFingerprint([]byte{}, "MD5")
	assert.Equal(t, "", result)

	result = formatFingerprint([]byte{}, "SHA256")
	assert.Equal(t, "SHA256:", result)
}

// ============================================================
// calculateFingerprint Tests
// ============================================================

func TestCalculateFingerprint_ED25519(t *testing.T) {
	svc, _ := newTestKeyService()

	fp, err := svc.calculateFingerprint([]byte(testED25519PubKey), "ed25519")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(fp, "SHA256:"), "ed25519 should use SHA256 fingerprint")
	assert.NotEqual(t, "SHA256:", fp, "fingerprint should have content after prefix")
}

func TestCalculateFingerprint_RSA(t *testing.T) {
	svc, _ := newTestKeyService()

	fp, err := svc.calculateFingerprint([]byte(testRSAPubKey), "rsa")
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(fp, "SHA256:"), "RSA should use MD5 fingerprint")
	assert.NotEmpty(t, fp)
	// MD5 of RSA key data = 32 hex chars
	assert.Len(t, fp, 32)
}

func TestCalculateFingerprint_Deterministic(t *testing.T) {
	svc, _ := newTestKeyService()

	fp1, err := svc.calculateFingerprint([]byte(testED25519PubKey), "ed25519")
	require.NoError(t, err)
	fp2, err := svc.calculateFingerprint([]byte(testED25519PubKey), "ed25519")
	require.NoError(t, err)
	assert.Equal(t, fp1, fp2, "same key should produce same fingerprint")
}

func TestCalculateFingerprint_InvalidFormat(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.calculateFingerprint([]byte("no-space-here"), "ed25519")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid public key format")
}

func TestCalculateFingerprint_InvalidBase64(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.calculateFingerprint([]byte("ssh-ed25519 !!!not-base64!!!"), "ed25519")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decode public key")
}

// --- DescribeKeyPairs AWS filter tests ---

func TestDescribeKeyPairs_AWSFilterByKeyName(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "key-alpha")
	importTestKey(t, svc, "key-beta")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-name"), Values: []*string{aws.String("key-alpha")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 1)
	assert.Equal(t, "key-alpha", *out.KeyPairs[0].KeyName)
}

func TestDescribeKeyPairs_AWSFilterByKeyPairId(t *testing.T) {
	svc, _ := newTestKeyService()
	imported := importTestKey(t, svc, "key-target")
	importTestKey(t, svc, "key-other")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-pair-id"), Values: []*string{imported.KeyPairId}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 1)
	assert.Equal(t, *imported.KeyPairId, *out.KeyPairs[0].KeyPairId)
}

func TestDescribeKeyPairs_AWSFilterByFingerprint(t *testing.T) {
	svc, _ := newTestKeyService()
	imported := importTestKey(t, svc, "key-fp")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("fingerprint"), Values: []*string{imported.KeyFingerprint}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 1)
	assert.Equal(t, *imported.KeyFingerprint, *out.KeyPairs[0].KeyFingerprint)
}

func TestDescribeKeyPairs_AWSFilterMultipleValues_OR(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "key-a")
	importTestKey(t, svc, "key-b")
	importTestKey(t, svc, "key-c")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-name"), Values: []*string{aws.String("key-a"), aws.String("key-c")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 2)
}

func TestDescribeKeyPairs_AWSFilterUnknownName_Error(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeKeyPairs_AWSFilterWildcard(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "prod-web")
	importTestKey(t, svc, "prod-api")
	importTestKey(t, svc, "staging-web")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-name"), Values: []*string{aws.String("prod-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 2)
}

func TestDescribeKeyPairs_AWSFilterNoResults(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "my-key")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-name"), Values: []*string{aws.String("nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.KeyPairs)
}

func TestDescribeKeyPairs_AWSFilterNoFilters(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "key-1")
	importTestKey(t, svc, "key-2")

	out, err := svc.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 2)
}
