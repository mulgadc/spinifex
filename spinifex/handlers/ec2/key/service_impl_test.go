package handlers_ec2_key

import (
	"context"
	"crypto/sha1"
	"crypto/x509"
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
	"golang.org/x/crypto/ssh"
)

const (
	testBucket    = "test-bucket"
	testAccountID = "123456789"
)

// Valid ed25519 public key for import tests.
const testED25519PubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// Valid RSA public key for import tests (generated locally, 2048-bit).
const testRSAPubKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDP9LrByKWpgbX+prxBwnlf7lrmI5AfDwuiCofuvOAzt9/PwIDMMIAhlvlpm09jjOuuH/MRQApJB5A714Auv+hBKVK0lCq9KcTrnKZOpRj2aGgIZgaoO6P/POoZc+kBf9Y/GP18DCKc4y/XyBsp69dPP6XRdYBlEdeIeVZdgCPYrM7s5FjT7aML2ba2Y2EvH116hPxv+nJZGwM6yqWxWRyTOoNMMTAfNYmoKkNy2zC1FARUyqDwumJ2z5Fvo4ZdN1qoFPOsfPc3iB0NUtSZbLU1awScvHb0rwR5PRnelTZ3Nbkw8I8A0IAhBTE5veW9D38hDIJhRd4nW73BUhgmzDL7"

// Well-formed ECDSA (nistp256) public key: parseable by x/crypto/ssh, but EC2
// key pairs are RSA or ED25519 only, so import must reject it.
const testECDSAPubKey = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBMZBEqs7mTlkOKGPSYp+tc5lZadVp9C2vCzIWZFTdnO1e3a8X59SdBWXiccQXjK1jxj+KLuQAEJY38kKqIUe/no="

// Well-formed DSA public key: parseable by x/crypto/ssh but not a key type EC2 accepts.
const testDSAPubKey = "ssh-dss AAAAB3NzaC1kc3MAAACBAMfooAGeGkAp6syFsxNueaASpwMr5t+BiLTxAmf9grlH/7MPhXHxxtrrzEdq4bK0mheLl4irAtardgR5ghOVxKXqz4dLlcEyv3H3tOY8Hq+JT/6w9j4FLjen4obXcilh7vfRPdL/A7Dk3Th9NSkrp43FmXUL4EyuMbqi7LcQYpMpAAAAFQDpwLJZvztWN3pEeT/MMLsVSmJedwAAAIBZjhEyHHk1iomvueP6GkmdrXt4V9+6BHjG/rHzQRlO79muU5ImX/BFALCc0RjaPNAoo0lF6ptaPf2HPeu3dtEAWM9iXH8SLqcAVX7B5FUYKFb7zsyQmlT3pKo21V3mCakKHDma8kbHSC2sysl1NOD4IkGTQalP4MuzIvCXNKbCdAAAAIEAl7OdP7hBngX0CuM0+cJXonZvnvIo1NOWGVu+dCn93mvoGjKFyZmLSEMIfFbmckQF2J4F9cM9aU6ht76k73DFnnA/F7WiJ+hIKLhL7Y8F0eDtWkawswvwxvHB+C7drrqezD6t5INX4CYlNQD4zqhgWKBSKVn3sQzQI95gFE51rKE="

const (
	// The OpenSSH SHA256 rendering, as reported by `ssh-keygen -lf`. This is not
	// the value AWS returns: AWS drops the "SHA256:" prefix and pads the base64.
	testED25519Fingerprint = "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU"

	// Recorded from real AWS: the value ImportKeyPair returned for testRSAPubKey
	// above. Reproducible with
	// `ssh-keygen -f k.pub -e -m PKCS8 | openssl pkey -pubin -outform DER | openssl md5 -c`.
	testRSAImportedFingerprint = "ad:57:77:5e:13:5f:87:5d:e8:95:46:cb:3c:92:a1:be"
)

// A fixed RSA private key, so one created-path assertion is a golden value that
// does not depend on the generator. Its fingerprint below comes from
// `openssl pkcs8 -topk8 -nocrypt -in k -outform DER | openssl sha1 -c`, the
// command AWS documents for verifying a key pair EC2 generated.
const testRSAPrivKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEApFkPvNAcoePF248Sb6x3k+IEW4WC0n7J95J59J8qYziNTgsn
6NxKAqOeDRaUc5pzPcJA28iJaW7FjH3Z9o6VSt0jJPqp4la1XqpBL7hQnta1adWl
VLAQry/Va9K8suoqcX0jbJ+wQzz3gv+HzfCPVGJMuqG6Mms0HL6G5m0AYRhVsQhU
QGc3rPZ2wlO2XJMWDop1BpY+oN+K/wcYTevwgD49TG1PkiIGU4txqcDxUdljw+/8
ihRHb8YzVeQEtTt6FEDH0n/0P+3WTZRX1JCJc567eg167sGEhJ8J2RL5osIoqJoy
y5hT5er/A07UVaaZ0NtsY36ln5fusWEA98aoJQIDAQABAoIBABe3Jlc3rLoLtTRu
m9lziLnVRo2yYWNgmmJYR7Lt+N7ifTmC0JqAl0l0NM1ssbVQ10pVKqjMck+9hVI1
ous6Pf7UlEq0xSj9HCTx6oApV7DkCL+h7b6fvXiaLXDmswYaVk+UIDV/gZ7iQFEt
8HneOcCSgsH3rneyEo4HTE4Z8pEQB6ZY3xNceYbiM5/GWOfuuhF7YbxrkApHKvVS
Q8zr/fXXNQUbaqYKQX+KppdlsZkXwUjR2h0xhbp8/B7Dv42cS3E6oeeng0jTB4ix
Co9juenwTQf1YYWiF0sRv4cg6CwiALKYuUiAByC5a2ZHXOZdnlYjbRLhIEBZnVBz
yVuZ5cECgYEA1aNG9elohFw6ysAbSSWE4J13iD9iDK/MX9hzxojDpT8hOuv08QDc
H6N9I/tlsqNzKNzGGfXYxCPAG/Sv3d+bI3F8r/z8E7ILdI/GpzIfWeNKc31YGLWR
YNU/19WlpYMujh93t7m3U+xcIRtwa9MHdhklik+urWwpmPCMIa1toDUCgYEAxO+4
gVW8HI2svh57bumzHlhtmmjpTqrLuYZshmOTd4Sl7r4exlBjm/r0SSHNQ2ycJKLO
9YbXvEByFW/8yQAhUzX4VZmLRNbNVrtFOUx8ymK17FJK2cN/pUqW6oZu/7D22LLy
PtVQrFzV7ARvF40xIrIPLlM0ZTLP4HwSPqyaxjECgYAvffmjZzzl1772HZizPRT5
/ed5sWVxno8Xa33pT7P2gz824wdzoBZPLj/+hL+J484Q8mtTkBSdHblyPYXvE+tg
CLWIRfwfwL/NLL0jo//WMrH1VJMGAy8LULy9lXAaiDwMOjCZ9j4r+OpOLdRjE+mf
tl1jDu2s/dONfUQZpH0vVQKBgGuxI1Ymig2bM8FrbdhDF94aQSVVBXAtWeaEKch7
n2KWOR8K/E06HJ5pZziusU6Tj/dAyKffKw4Yt8odSUCpP4//TWOR6WSlifhJxBsH
Rp5tyEoI3kGi9KRw24I4LW7JWNM7V9kgUVNQGPNNoWphnWL5t+9/NIG6fY6miluX
i7OhAoGBAMvVHgBWR9vcgC3Ii1u0r+sBJyiQ7v1Nrog89/6ufA5HR0UQ29lHJM+K
YpmMAcq6NBl+N1yo0zq59Rs2ueoCwhiNSYzJ7L6qB8jru3mIXKug6kxlEmmq9lmq
YCgXE9CeVzlPlMA2IBLqfA8/DZx+CiyVJAqfpRvbwgSMAV8zlxFL
-----END RSA PRIVATE KEY-----
`

const (
	testRSACreatedFingerprint = "2b:c7:d0:bc:45:a8:4b:dc:2a:8f:0e:9b:70:db:66:a6:b8:67:84:28"

	// The same fixture down the import path instead, from
	// `ssh-keygen -y -f k | ssh-keygen -f /dev/stdin -e -m PKCS8 | openssl pkey -pubin -outform DER | openssl md5 -c`.
	// One key, two unrelated digests, because the two paths hash different things.
	testRSAPrivKeyImportedFingerprint = "66:81:50:28:0a:74:9e:48:9f:d8:79:da:27:86:22:79"
)

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
	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
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

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
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
	getOut, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, getOut)

	// Verify metadata stored in S3
	metaPath := "keys/" + testAccountID + "/" + *out.KeyPairId + ".json"
	metaOut, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, metaOut)
}

func TestCreateKeyPair_RSA(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("my-rsa-key"),
		KeyType: aws.String("rsa"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "my-rsa-key", *out.KeyName)

	// The key is generated per-run, so the digest is not knowable in advance.
	// Recompute it from the private key the caller was handed: that is what pins
	// the fingerprint to the private key rather than to the public one.
	rawKey, err := ssh.ParseRawPrivateKey([]byte(*out.KeyMaterial))
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(rawKey)
	require.NoError(t, err)
	sum := sha1.Sum(der)

	assert.Equal(t, colonHex(sum[:]), *out.KeyFingerprint)
	// 20 hex pairs -- a SHA-1, not the 16-pair MD5 this used to return.
	assert.Regexp(t, `^([0-9a-f]{2}:){19}[0-9a-f]{2}$`, *out.KeyFingerprint)
}

func TestCreateKeyPair_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), nil, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreateKeyPair_MissingKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreateKeyPair_InvalidKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("invalid key name!@#"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairFormat, err.Error())
}

func TestCreateKeyPair_Duplicate(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	_, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("dup-key"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("dup-key"),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairDuplicate, err.Error())
}

func TestCreateKeyPair_InvalidKeyType(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
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

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported-ed25519"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "imported-ed25519", *out.KeyName)
	assert.NotEmpty(t, *out.KeyPairId)
	assert.True(t, strings.HasPrefix(*out.KeyPairId, "key-"))
	assert.Equal(t, testED25519Fingerprint, *out.KeyFingerprint)

	// Verify public key stored in S3
	keyPath := "keys/" + testAccountID + "/imported-ed25519"
	getOut, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, getOut)

	// Verify metadata stored in S3
	metaPath := "keys/" + testAccountID + "/" + *out.KeyPairId + ".json"
	metaOut, err := store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	require.NoError(t, err)
	assert.NotNil(t, metaOut)
}

func TestImportKeyPair_Success_RSA(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported-rsa"),
		PublicKeyMaterial: []byte(testRSAPubKey),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.Equal(t, "imported-rsa", *out.KeyName)
	assert.Equal(t, testRSAImportedFingerprint, *out.KeyFingerprint)
}

// A trailing comment is accepted and preserved, and surrounding whitespace is
// stripped, so what the guest is served is exactly the key the fingerprint
// covers.
func TestImportKeyPair_KeyWithComment(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("imported-commented"),
		PublicKeyMaterial: []byte(testED25519PubKey + " user@laptop\n"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, testED25519Fingerprint, *out.KeyFingerprint)

	material, err := svc.GetPublicKeyMaterial(testAccountID, "imported-commented")
	require.NoError(t, err)
	assert.Equal(t, testED25519PubKey+" user@laptop", material)
}

func TestImportKeyPair_Duplicate(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("dup-import"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("dup-import"),
		PublicKeyMaterial: []byte(testED25519PubKey),
	}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairDuplicate, err.Error())
}

func TestImportKeyPair_InvalidKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
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
			name:           "TruncatedAlgorithmBody",
			publicKey:      "ssh-dss AAAAB3NzaC1kc3MAAACB",
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			// Parses cleanly, but DSA is not a key type EC2 accepts.
			name:           "UnsupportedAlgorithmDSA",
			publicKey:      testDSAPubKey,
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			// Likewise ECDSA: EC2 key pairs are RSA or ED25519 only.
			name:           "UnsupportedAlgorithmECDSA",
			publicKey:      testECDSAPubKey,
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
		{
			// An option prefix parses, but sshd would honour it on every login
			// while the fingerprint reports only the bare key.
			name:           "AuthorizedKeysOptions",
			publicKey:      `command="/bin/false",no-pty ` + testED25519PubKey,
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			// Only the first key would be validated and fingerprinted, yet both
			// would be installed on the guest.
			name:           "MultipleKeys",
			publicKey:      testED25519PubKey + "\n" + testRSAPubKey,
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
		{
			// ParseAuthorizedKey skips leading junk, so the blob must be rejected
			// before it reaches the parser.
			name:           "LeadingCommentLine",
			publicKey:      "# my key\n" + testED25519PubKey,
			expectedErrMsg: awserrors.ErrorInvalidKeyFormat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A name per case: a case that regressed into acceptance would
			// otherwise store an object and fail every later case with
			// Duplicate, pointing the failure at the wrong input.
			keyName := "test-key-" + tt.name
			_, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
				KeyName:           aws.String(keyName),
				PublicKeyMaterial: []byte(tt.publicKey),
			}, testAccountID)
			require.Error(t, err)
			assert.Equal(t, tt.expectedErrMsg, err.Error())

			// Rejection must precede the upload, or the guest is served material
			// the API refused.
			_, err = svc.GetPublicKeyMaterial(testAccountID, keyName)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
		})
	}
}

// ============================================================
// DeleteKeyPair Tests
// ============================================================

func TestDeleteKeyPair_ByKeyName(t *testing.T) {
	svc, store := newTestKeyService()

	imported := importTestKey(t, svc, "to-delete-by-name")

	result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
		KeyName: aws.String("to-delete-by-name"),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify public key removed from S3
	keyPath := "keys/" + testAccountID + "/to-delete-by-name"
	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	assert.Error(t, err, "public key should be deleted")

	// Verify metadata removed from S3
	metaPath := "keys/" + testAccountID + "/" + *imported.KeyPairId + ".json"
	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	assert.Error(t, err, "metadata should be deleted")
}

func TestDeleteKeyPair_ByKeyPairId(t *testing.T) {
	svc, store := newTestKeyService()

	imported := importTestKey(t, svc, "to-delete-by-id")

	result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
		KeyPairId: imported.KeyPairId,
	}, testAccountID)
	require.NoError(t, err)
	assert.NotNil(t, result)

	// Verify public key removed from S3
	keyPath := "keys/" + testAccountID + "/to-delete-by-id"
	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(keyPath),
	})
	assert.Error(t, err, "public key should be deleted")

	// Verify metadata removed from S3
	metaPath := "keys/" + testAccountID + "/" + *imported.KeyPairId + ".json"
	_, err = store.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metaPath),
	})
	assert.Error(t, err, "metadata should be deleted")
}

func TestDeleteKeyPairIdempotent(t *testing.T) {
	svc, _ := newTestKeyService()

	t.Run("NonExistentKeyName", func(t *testing.T) {
		result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
			KeyName: aws.String("no-such-key"),
		}, testAccountID)
		require.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("NonExistentKeyPairId", func(t *testing.T) {
		result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
			KeyPairId: aws.String("key-0123456789abcdef0"),
		}, testAccountID)
		require.NoError(t, err)
		assert.NotNil(t, result)
	})
}

func TestDeleteKeyPair_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(context.Background(), nil, testAccountID)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDeleteKeyPair_EmptyNameAndId(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{}, testAccountID)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDeleteKeyPair_InvalidKeyPairIdFormat(t *testing.T) {
	svc, _ := newTestKeyService()

	result, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.KeyPairs)
}

func TestDescribeKeyPairs_AllKeys(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "key-alpha")
	importTestKey(t, svc, "key-beta")

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, testAccountID)
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

// KeyType is inferred from the stored fingerprint's shape, so it has to survive
// RSA reporting two different digest widths: 16 bytes when imported, 20 when EC2
// generated the key. Only ED25519 carries the "SHA256:" prefix that marks it.
func TestDescribeKeyPairs_KeyTypeInference(t *testing.T) {
	requireSSHKeygen(t)
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "inferred-ed25519")

	_, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName:           aws.String("inferred-rsa-imported"),
		PublicKeyMaterial: []byte(testRSAPubKey),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{
		KeyName: aws.String("inferred-rsa-created"),
		KeyType: aws.String("rsa"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)

	keyTypes := make(map[string]string, len(out.KeyPairs))
	for _, kp := range out.KeyPairs {
		keyTypes[*kp.KeyName] = *kp.KeyType
	}
	assert.Equal(t, map[string]string{
		"inferred-ed25519":      "ed25519",
		"inferred-rsa-imported": "rsa",
		"inferred-rsa-created":  "rsa",
	}, keyTypes)
}

func TestDescribeKeyPairs_FilterByKeyName(t *testing.T) {
	svc, _ := newTestKeyService()

	importTestKey(t, svc, "find-me")
	importTestKey(t, svc, "ignore-me")

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
		KeyNames: []*string{aws.String("does-not-exist")},
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.KeyPairs)
}

func TestDescribeKeyPairs_NilInput(t *testing.T) {
	svc, _ := newTestKeyService()

	out, err := svc.DescribeKeyPairs(context.Background(), nil, testAccountID)
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

	err := svc.ValidateKeyPairExists(context.Background(), testAccountID, "existing-key")
	assert.NoError(t, err)
}

func TestValidateKeyPairExists_NotFound(t *testing.T) {
	svc, _ := newTestKeyService()

	err := svc.ValidateKeyPairExists(context.Background(), testAccountID, "ghost-key")
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
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
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
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
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
// getKeyNameFromKeyPairId Tests
// ============================================================

func TestGetKeyNameFromKeyPairId_NotFound(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.getKeyNameFromKeyPairId(context.Background(), testAccountID, "key-nonexistent")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidKeyPairNotFound, err.Error())
}

func TestGetKeyNameFromKeyPairId_InvalidJSON(t *testing.T) {
	svc, store := newTestKeyService()

	// Seed store with garbage data at the metadata path
	metadataPath := "keys/" + testAccountID + "/key-badjson.json"
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metadataPath),
		Body:   strings.NewReader("this is not valid json{{{"),
	})
	require.NoError(t, err)

	_, err = svc.getKeyNameFromKeyPairId(context.Background(), testAccountID, "key-badjson")
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

	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testBucket),
		Key:    aws.String(metadataPath),
		Body:   strings.NewReader(string(jsonData)),
	})
	require.NoError(t, err)

	_, err = svc.getKeyNameFromKeyPairId(context.Background(), testAccountID, "key-noname")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid metadata: missing KeyName")
}

func TestGetKeyNameFromKeyPairId_Success(t *testing.T) {
	svc, _ := newTestKeyService()

	// Import a key, then look it up by keyPairId
	imported := importTestKey(t, svc, "lookup-test")

	keyName, err := svc.getKeyNameFromKeyPairId(context.Background(), testAccountID, *imported.KeyPairId)
	require.NoError(t, err)
	assert.Equal(t, "lookup-test", keyName)
}

// ============================================================
// Fingerprint / keyPairType Tests
// ============================================================

// parseTestPubKey parses an authorized-key line that the test asserts is valid.
func parseTestPubKey(t *testing.T, material string) ssh.PublicKey {
	t.Helper()
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(material))
	require.NoError(t, err)
	return pubKey
}

func TestImportedKeyFingerprint(t *testing.T) {
	tests := []struct {
		name     string
		pubKey   string
		expected string
	}{
		// The OpenSSH rendering of the SHA256 digest, which is not what AWS
		// returns -- AWS drops the prefix and pads the base64.
		{name: "ED25519", pubKey: testED25519PubKey, expected: testED25519Fingerprint},
		// The MD5 of the DER SubjectPublicKeyInfo, matching AWS.
		{name: "RSA", pubKey: testRSAPubKey, expected: testRSAImportedFingerprint},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fingerprint, err := importedKeyFingerprint(parseTestPubKey(t, tt.pubKey))
			require.NoError(t, err)
			assert.Equal(t, tt.expected, fingerprint)
		})
	}
}

// keyPairType rejects these before import reaches the fingerprint, but the two
// guards must not drift: an algorithm that slips past must error rather than
// yield a digest AWS would never produce. ECDSA is the case that matters --
// x509.MarshalPKIXPublicKey marshals it happily, so nothing else would stop it.
func TestImportedKeyFingerprint_UnsupportedAlgorithm(t *testing.T) {
	for name, pubKey := range map[string]string{"ECDSA": testECDSAPubKey, "DSA": testDSAPubKey} {
		t.Run(name, func(t *testing.T) {
			_, err := importedKeyFingerprint(parseTestPubKey(t, pubKey))
			require.Error(t, err)
		})
	}
}

// A comment or surrounding whitespace is not part of the key, so it must not
// move the fingerprint.
func TestImportedKeyFingerprint_IgnoresComment(t *testing.T) {
	fingerprint, err := importedKeyFingerprint(parseTestPubKey(t, testED25519PubKey+" user@host\n"))
	require.NoError(t, err)
	assert.Equal(t, testED25519Fingerprint, fingerprint)
}

// One RSA key, fingerprinted down both paths. The digests share no bytes because
// the paths hash different things -- the private key on create, the public key on
// import -- which is the divergence that made this two fixes rather than one. A
// created fingerprint that ever equals the imported one has regressed to hashing
// the public key.
func TestCreatedKeyFingerprint_RSA(t *testing.T) {
	signer, err := ssh.ParsePrivateKey([]byte(testRSAPrivKey))
	require.NoError(t, err)

	created, err := createdKeyFingerprint([]byte(testRSAPrivKey), signer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, testRSACreatedFingerprint, created)

	imported, err := importedKeyFingerprint(signer.PublicKey())
	require.NoError(t, err)
	assert.Equal(t, testRSAPrivKeyImportedFingerprint, imported)
}

// ED25519 hashes the public key on both paths, so the created value matches the
// imported one -- and diverges from AWS the same way. The private key is passed
// deliberately mismatched: this branch must not read it at all.
func TestCreatedKeyFingerprint_ED25519(t *testing.T) {
	fingerprint, err := createdKeyFingerprint([]byte(testRSAPrivKey), parseTestPubKey(t, testED25519PubKey))
	require.NoError(t, err)
	assert.Equal(t, testED25519Fingerprint, fingerprint)
}

func TestColonHex(t *testing.T) {
	assert.Equal(t, "00:0f:a0:ff", colonHex([]byte{0x00, 0x0f, 0xa0, 0xff}))
	assert.Empty(t, colonHex(nil))
}

func TestKeyPairType(t *testing.T) {
	tests := []struct {
		name      string
		pubKey    string
		expected  string
		expectErr bool
	}{
		{name: "ED25519", pubKey: testED25519PubKey, expected: "ed25519"},
		{name: "RSA", pubKey: testRSAPubKey, expected: "rsa"},
		{name: "ECDSAUnsupported", pubKey: testECDSAPubKey, expectErr: true},
		{name: "DSAUnsupported", pubKey: testDSAPubKey, expectErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyType, err := keyPairType(parseTestPubKey(t, tt.pubKey))
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, keyType)
		})
	}
}

// --- DescribeKeyPairs AWS filter tests ---

func TestDescribeKeyPairs_AWSFilterByKeyName(t *testing.T) {
	svc, _ := newTestKeyService()
	importTestKey(t, svc, "key-alpha")
	importTestKey(t, svc, "key-beta")

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("key-name"), Values: []*string{aws.String("key-a"), aws.String("key-c")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 2)
}

func TestDescribeKeyPairs_AWSFilterUnknownName_Error(t *testing.T) {
	svc, _ := newTestKeyService()

	_, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{
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

	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.KeyPairs, 2)
}
