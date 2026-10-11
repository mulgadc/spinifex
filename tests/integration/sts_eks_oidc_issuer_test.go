//go:build integration

package integration

import (
	"testing"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEKSClusterOIDCIssuer_RoundTripsThroughSTSParseEKSIssuerURL is a
// composition test: handlers/eks mints an issuer URL via ClusterOIDCIssuer and
// handlers/sts must parse it back via ParseEKSIssuerURL to the same
// (accountID, clusterName) — the collision the JWKS lookup must avoid.
// Importing both packages together is intentional here; neither package's own
// tests may depend on the other.
func TestEKSClusterOIDCIssuer_RoundTripsThroughSTSParseEKSIssuerURL(t *testing.T) {
	issuer, err := handlers_eks.ClusterOIDCIssuer("https://10.0.0.1:9999", "au-mel-1", "123456789012", "my-cluster")
	require.NoError(t, err)

	accountID, clusterName, err := handlers_sts.ParseEKSIssuerURL(issuer)
	require.NoError(t, err)
	assert.Equal(t, "123456789012", accountID)
	assert.Equal(t, "my-cluster", clusterName)
}
