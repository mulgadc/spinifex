//go:build integration

package integration

import (
	"testing"

	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEKSClusterOIDCIssuer_RoundTripsThroughSTSParseEKSIssuerURL is a
// composition test: domains/eks/cluster mints an issuer URL via ClusterOIDCIssuer and
// domains/sts must parse it back via ParseEKSIssuerURL to the same
// (accountID, clusterName) — the collision the JWKS lookup must avoid.
// Importing both packages together is intentional here; neither package's own
// tests may depend on the other.
func TestEKSClusterOIDCIssuer_RoundTripsThroughSTSParseEKSIssuerURL(t *testing.T) {
	issuer, err := ekscluster.ClusterOIDCIssuer("https://10.0.0.1:9999", "au-mel-1", "123456789012", "my-cluster")
	require.NoError(t, err)

	accountID, clusterName, err := stsdomain.ParseEKSIssuerURL(issuer)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", accountID)
	assert.Equal(t, "my-cluster", clusterName)
}
