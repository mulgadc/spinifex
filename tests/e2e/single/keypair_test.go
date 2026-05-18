//go:build e2e

package single

import (
	"crypto/rand"
	"crypto/rsa"
	"os"
	"path/filepath"
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
// DeleteKeyPair. test-key-1 is kept around for Phase 5 (instance launch);
// test-key-2 is created via import then deleted within this phase. Maps to
// run-e2e.sh ~204–231.
func phase3_KeyPairs(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 3 — SSH Key Management")

	const key1 = "test-key-1"
	const key2 = "test-key-2"

	// Best-effort clean-up of leftovers from a prior failed run so this
	// phase is idempotent — bash relies on `set -e` failing loudly, but
	// in Go we'd rather take the deterministic path and recreate.
	_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key1)})
	_, _ = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key2)})

	harness.Step(t, "create-key-pair %q", key1)
	created, err := fix.AWS.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{
		KeyName: aws.String(key1),
	})
	require.NoError(t, err, "create-key-pair %s", key1)
	material := aws.StringValue(created.KeyMaterial)
	require.NotEmpty(t, material, "create-key-pair returned empty KeyMaterial")
	require.True(t, strings.HasPrefix(material, "-----BEGIN"),
		"KeyMaterial must be a PEM block (got prefix %q)", firstN(material, 32))

	pemPath := filepath.Join(t.TempDir(), key1+".pem")
	require.NoError(t, os.WriteFile(pemPath, []byte(material), 0o600), "write PEM")
	fix.KeyName = key1
	fix.KeyPath = pemPath
	harness.Detail(t, "key", key1, "pem", pemPath)

	// TODO(stage-G): register a t.Cleanup that deletes key1 once the whole
	// TestSingleNode test finishes. Phase 5+ still need this key, so we
	// can't bind it to the Phase 3 subtest. Stage G's teardown_test.go
	// will own the final cleanup.

	harness.Step(t, "import-key-pair %q (local-generated RSA)", key2)
	pubMaterial := generateImportPubKey(t)
	_, err = fix.AWS.EC2.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(key2),
		PublicKeyMaterial: pubMaterial,
	})
	require.NoError(t, err, "import-key-pair %s", key2)
	harness.Detail(t, "imported", key2)

	harness.Step(t, "describe-key-pairs (both present)")
	listed := describeKeyNames(t, fix)
	assert.Contains(t, listed, key1, "describe-key-pairs missing %s (got %v)", key1, listed)
	assert.Contains(t, listed, key2, "describe-key-pairs missing %s (got %v)", key2, listed)

	harness.Step(t, "delete %q", key2)
	_, err = fix.AWS.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key2)})
	require.NoError(t, err, "delete-key-pair %s", key2)

	harness.Step(t, "describe-key-pairs (only %s remains)", key1)
	remaining := describeKeyNames(t, fix)
	assert.Contains(t, remaining, key1, "describe-key-pairs lost %s after deleting %s", key1, key2)
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
