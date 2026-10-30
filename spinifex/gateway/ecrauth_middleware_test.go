package gateway

import (
	"crypto/rand"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/stretchr/testify/require"
)

const (
	ecrTestRegion   = "ap-southeast-2"
	ecrTestSuffix   = "spinifex.internal"
	ecrTestAudience = "ecr." + ecrTestRegion + "." + ecrTestSuffix
	ecrTestAccount  = "000000000001"
)

func ecrTestMasterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

// newECRAuth builds a wired issuer+verifier over a freshly generated signing key.
func newECRAuth(t *testing.T) (*ecrauth.Issuer, *ecrauth.Verifier) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	key, verify, err := ecrauth.LoadOrCreateSigningKey(t.Context(), js, ecrTestMasterKey(t))
	require.NoError(t, err)
	return ecrauth.NewIssuer(key, ecrTestAudience), ecrauth.NewVerifier(verify, ecrTestAudience)
}
