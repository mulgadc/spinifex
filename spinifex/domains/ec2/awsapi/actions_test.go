//test:in-package — reads the unexported ec2Actions route table.

package awsapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportKeyPair_Base64PaddingWorkaround(t *testing.T) {
	// The ImportKeyPair handler decodes URL-encoded Base64 padding (%3D%3D → ==)
	// before passing to the generic handler.
	handler := ec2Actions["ImportKeyPair"]
	require.NotNil(t, handler)

	q := map[string]string{
		"Action":            "ImportKeyPair",
		"KeyName":           "test-key",
		"PublicKeyMaterial": "c3NoLXJzYSBBQUFBQjNOemFDMXljMkVBQUFBREFRQUJBQUFCZ1FD%3D%3D",
	}

	_, err := handler.parse(q)
	require.NoError(t, err)

	assert.True(t, strings.HasSuffix(q["PublicKeyMaterial"], "=="),
		"Expected PublicKeyMaterial to end with == but got: %s", q["PublicKeyMaterial"])
	assert.NotContains(t, q["PublicKeyMaterial"], "%3D",
		"Expected no URL-encoded padding remaining")
}
