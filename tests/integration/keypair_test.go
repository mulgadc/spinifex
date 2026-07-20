//go:build integration

package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// TestKeyPairs is ported from tests/e2e/single/keypair_test.go runKeyPairs,
// driving the REAL key/service_impl.go over the harness's DaemonLite instead
// of a live spinifex daemon. Exercises CreateKeyPair / ImportKeyPair /
// DescribeKeyPairs / DeleteKeyPair with the same assertions as the E2E
// source: both keys visible after import, only the primary key remains after
// delete.
func TestKeyPairs(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)
	ec2Cli := gw.EC2Client(t)

	const key1 = "test-key-1"
	const key2 = "test-key-2"

	createOut, err := ec2Cli.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(key1)})
	require.NoError(t, err, "create-key-pair %s", key1)
	require.NotEmpty(t, aws.StringValue(createOut.KeyMaterial), "CreateKeyPair PEM empty")
	require.Contains(t, aws.StringValue(createOut.KeyMaterial), "-----BEGIN",
		"PEM must contain -----BEGIN (got prefix %q)", firstN(aws.StringValue(createOut.KeyMaterial), 32))

	pubMaterial := generateImportPubKey(t)
	_, err = ec2Cli.ImportKeyPair(&ec2.ImportKeyPairInput{
		KeyName:           aws.String(key2),
		PublicKeyMaterial: pubMaterial,
	})
	require.NoError(t, err, "import-key-pair %s", key2)

	listed := describeKeyNames(t, ec2Cli)
	assert.Contains(t, listed, key1, "describe-key-pairs missing %s (got %v)", key1, listed)
	assert.Contains(t, listed, key2, "describe-key-pairs missing %s (got %v)", key2, listed)

	_, err = ec2Cli.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(key2)})
	require.NoError(t, err, "delete-key-pair %s", key2)

	remaining := describeKeyNames(t, ec2Cli)
	assert.Contains(t, remaining, key1, "describe-key-pairs lost %s after deleting %s", key1, key2)
	assert.NotContains(t, remaining, key2, "describe-key-pairs still lists %s after delete", key2)
}

// generateImportPubKey returns an OpenSSH-formatted public key (matching the
// `ssh-keygen -t rsa` output the E2E source's import-key-pair step feeds in).
// 2048-bit to match the E2E key length.
func generateImportPubKey(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "generate RSA key")
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	require.NoError(t, err, "ssh.NewPublicKey")
	return ssh.MarshalAuthorizedKey(pub)
}

func describeKeyNames(t *testing.T, ec2Cli *ec2.EC2) []string {
	t.Helper()
	out, err := ec2Cli.DescribeKeyPairs(&ec2.DescribeKeyPairsInput{})
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
