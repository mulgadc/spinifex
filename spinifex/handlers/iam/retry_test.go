package handlers_iam_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/clustersize"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

// TestNewIAMServiceWithRetry_StopsOnAConfigFault covers the difference between a
// cluster that has yet to form and one that was misconfigured.
//
// The retry loop exists for the first: on a cold multi-node boot the KV store
// needs a quorum that does not exist yet, so waiting is right. An undeclared
// cluster size is the second, and waiting five minutes for it produces a timeout
// that names quorum as the cause when the cause was a config the process never
// loaded. This test bounds that: it must come back fast, and it must come back
// saying what is actually wrong.
func TestNewIAMServiceWithRetry_StopsOnAConfigFault(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	clustersize.RedeclareForTest(t, 0)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	start := time.Now()
	_, err = handlers_iam.NewIAMServiceWithRetry(t.Context(), nc, masterKey)
	elapsed := time.Since(start)

	require.Error(t, err, "an undeclared cluster size must not look like a working IAM service")
	require.ErrorIs(t, err, clustersize.ErrUndeclared,
		"the error has to name the config fault, not the quorum the loop was waiting for")
	require.Lessf(t, elapsed, 10*time.Second,
		"returned after %s; a permanent fault must not consume the retry budget", elapsed)
}
