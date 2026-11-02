//test:in-package — reads the unexported ec2Actions route table.

package awsapi

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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

// A well-prefixed ID of the wrong shape is refused before any service is
// asked, so it can never come back as NotFound.
func TestDescribe_MalformedIDRejectedBeforeLookup(t *testing.T) {
	tests := []struct {
		action, param, id, code, msg string
	}{
		{"DescribeInstances", "InstanceId.1", "i-zzz", awserrors.ErrorInvalidInstanceIDMalformed, `Invalid id: "i-zzz"`},
		{"DescribeImages", "ImageId.1", "ami-zzz", awserrors.ErrorInvalidAMIIDMalformed, `Invalid id: "ami-zzz"`},
		{"DescribeVolumes", "VolumeId.1", "vol-zzz", awserrors.ErrorInvalidParameterValue, "Value (vol-zzz) for parameter volumes is invalid. Expected: 'vol-...'."},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			h := ec2Actions[tt.action]
			input, err := h.parse(map[string]string{tt.param: tt.id})
			require.NoError(t, err)
			_, err = h.dispatch(t.Context(), tt.action, input, &call{Handler: NewHandler(Deps{})}, "123456789012")
			code, msg, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, tt.code, code)
			assert.Equal(t, tt.msg, msg)
		})
	}
}
