package handlers_eks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusterOIDCIssuer_HappyPath(t *testing.T) {
	got, err := ClusterOIDCIssuer("https://gw.example:9999", "us-east-1", "111122223333", "alpha")
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example:9999/oidc/eks/us-east-1/111122223333/alpha", got)
}

func TestClusterOIDCIssuer_TrimsTrailingSlash(t *testing.T) {
	got, err := ClusterOIDCIssuer("https://gw.example:9999/", "us-east-1", "111122223333", "alpha")
	require.NoError(t, err)
	assert.Equal(t, "https://gw.example:9999/oidc/eks/us-east-1/111122223333/alpha", got)
}

func TestClusterOIDCIssuer_EmptyInputsRejected(t *testing.T) {
	cases := []struct {
		name                                       string
		base, region, accountID, clusterName, want string
	}{
		{"empty base", "", "us-east-1", "111122223333", "alpha", "gatewayBaseURL"},
		{"empty region", "https://gw", "", "111122223333", "alpha", "region"},
		{"empty accountID", "https://gw", "us-east-1", "", "alpha", "accountID"},
		{"empty clusterName", "https://gw", "us-east-1", "111122223333", "", "clusterName"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ClusterOIDCIssuer(tc.base, tc.region, tc.accountID, tc.clusterName)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestClusterOIDCIssuer_MalformedBaseRejected(t *testing.T) {
	_, err := ClusterOIDCIssuer("not-a-url", "us-east-1", "111122223333", "alpha")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing scheme or host")
}

func TestClusterOIDCIssuer_URLUnsafeInputsRejected(t *testing.T) {
	cases := []struct {
		field, region, accountID, clusterName string
	}{
		{"region", "us/east", "111122223333", "alpha"},
		{"accountID", "us-east-1", "111?ab", "alpha"},
		{"clusterName", "us-east-1", "111122223333", "alpha#frag"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			_, err := ClusterOIDCIssuer("https://gw", tc.region, tc.accountID, tc.clusterName)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "URL-unsafe")
		})
	}
}
