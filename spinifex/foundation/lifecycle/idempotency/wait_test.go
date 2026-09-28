//test:in-package — the poll deadline is unexported on purpose, so shrinking it
//to keep the test fast needs to happen from inside the package.

package idempotency

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A duplicate polling an owner that never finishes has to give up rather than
// block the request forever.
func TestStore_InFlightWaitTimesOut(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	store, err := OpenStore[string](t.Context(), testutil.NewJetStream(t, nc), "wait-tokens", time.Minute)
	require.NoError(t, err)
	const account, tok, hash = "111122223333", "wt-1", "h"

	_, owned, err := store.Claim(t.Context(), account, tok, hash)
	require.NoError(t, err)
	require.True(t, owned, "owner holds the in-flight record and never finalizes")

	origTimeout, origStep := waitTimeout, pollStep
	waitTimeout, pollStep = 30*time.Millisecond, 10*time.Millisecond
	defer func() { waitTimeout, pollStep = origTimeout, origStep }()

	_, _, err = store.Claim(t.Context(), account, tok, hash)
	assert.ErrorIs(t, err, ErrWaitTimeout)
}
