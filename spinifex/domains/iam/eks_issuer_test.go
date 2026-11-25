package iam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEKSIssuerURL_Valid(t *testing.T) {
	t.Parallel()

	accountID, clusterName, err := ParseEKSIssuerURL("https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/my-cluster")
	require.NoError(t, err)
	assert.Equal(t, "123456789012", accountID)
	assert.Equal(t, "my-cluster", clusterName)
}

func TestParseEKSIssuerURL_Invalid(t *testing.T) {
	cases := []struct {
		name   string
		issuer string
	}{
		{"http scheme", "http://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/c1"},
		{"missing host", "https:///oidc/eks/au-mel-1/123456789012/c1"},
		{"wrong path prefix", "https://10.0.0.1:9999/foo/eks/au-mel-1/123456789012/c1"},
		{"too few segments", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012"},
		{"too many segments", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/c1/extra"},
		{"empty account segment", "https://10.0.0.1:9999/oidc/eks/au-mel-1//c1"},
		{"empty cluster segment", "https://10.0.0.1:9999/oidc/eks/au-mel-1/123456789012/"},
		{"empty path", "https://10.0.0.1:9999"},
		{"legacy aws two-segment shape", "https://oidc.eks.au-mel-1.mulga.local/123456789012/c1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseEKSIssuerURL(tc.issuer)
			require.Error(t, err)
		})
	}
}
