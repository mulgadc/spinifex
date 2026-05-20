//go:build e2e

package single

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// phase3_KeyPairs exercises CreateKeyPair / ImportKeyPair / DescribeKeyPairs /
// DeleteKeyPair. The primary key pair is materialised via harness.EnsureKeyPair
// — it owns the create path's assertions, memoizes for downstream Phase 5+
// callers, and registers TestSingleNode-scoped cleanup. test-key-2 is created
// via import then deleted within this phase. Maps to run-e2e.sh ~204–231.
func phase3_KeyPairs(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 3 — SSH Key Management")

	const key2 = "test-key-2"

	// Clean up leftover import-test key from a prior failed run.
	_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key2)})

	harness.Step(t, "ensure primary key pair (Phase 5+ prereq)")
	keyName, pemPath := harness.EnsureKeyPair(t, fix.Harness, fix.TmpDir)
	material, rerr := os.ReadFile(pemPath)
	require.NoError(t, rerr, "read PEM %s", pemPath)
	require.NotEmpty(t, material, "EnsureKeyPair PEM empty")
	require.True(t, strings.HasPrefix(string(material), "-----BEGIN"),
		"PEM must start with -----BEGIN (got prefix %q)", firstN(string(material), 32))
	fix.KeyName = keyName
	fix.KeyPath = pemPath
	harness.Detail(t, "key", keyName, "pem", pemPath)

	harness.Step(t, "import-key-pair %q (local-generated RSA)", key2)
	pubMaterial := generateImportPubKey(t)
	_, err := fix.AWS.EC2.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(key2),
		PublicKeyMaterial: pubMaterial,
	})
	require.NoError(t, err, "import-key-pair %s", key2)
	harness.Detail(t, "imported", key2)

	harness.Step(t, "describe-key-pairs (both present)")
	listed := describeKeyNames(t, fix)
	assert.Contains(t, listed, keyName, "describe-key-pairs missing %s (got %v)", keyName, listed)
	assert.Contains(t, listed, key2, "describe-key-pairs missing %s (got %v)", key2, listed)

	harness.Step(t, "delete %q", key2)
	_, err = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key2)})
	require.NoError(t, err, "delete-key-pair %s", key2)

	harness.Step(t, "describe-key-pairs (only %s remains)", keyName)
	remaining := describeKeyNames(t, fix)
	assert.Contains(t, remaining, keyName, "describe-key-pairs lost %s after deleting %s", keyName, key2)
	assert.NotContains(t, remaining, key2, "describe-key-pairs still lists %s after delete", key2)
}

// generateImportPubKey returns an OpenSSH-formatted public key (matching the
// `ssh-keygen -t rsa` output the bash script feeds into import-key-pair).
// 2048-bit to match the bash key length.
func generateImportPubKey(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate RSA key")
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	require.NoError(t, err, "ssh.NewPublicKey")
	return ssh.MarshalAuthorizedKey(pub)
}

func describeKeyNames(t *testing.T, fix *Fixture) []string {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
	require.NoError(t, err, "describe-key-pairs")
	names := make([]string, 0, len(out.KeyPairs))
	for _, kp := range out.KeyPairs {
		names = append(names, aws.StringValue(kp.KeyName))
	}
	return names
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
