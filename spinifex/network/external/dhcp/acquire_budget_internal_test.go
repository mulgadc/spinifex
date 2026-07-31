package dhcp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// botocore sends no retry token and defaults to a 60s read timeout, so a DORA
// that outlasts it turns one AllocateAddress into two leases with no way to tell
// the retry from a second request. The ladder has to answer first.
func TestAcquireLadderFitsInsideClientReadTimeout(t *testing.T) {
	const clientReadTimeout = 60 * time.Second

	total := time.Duration(0)
	for _, d := range defaultAcquireSchedule {
		// Each attempt can run up to a second over its nominal timeout from the
		// per-attempt jitter.
		total += d + acquireAttemptJitter
	}
	assert.Less(t, total, clientReadTimeout,
		"acquire ladder totals %s, which must stay inside a %s client read timeout", total, clientReadTimeout)
	assert.Less(t, defaultAcquireBudget, clientReadTimeout,
		"the acquire budget must not outlast the client either")
	assert.Less(t, defaultRPCTimeout, clientReadTimeout,
		"the daemon must hear the outcome before its caller gives up")
	assert.Greater(t, defaultRPCTimeout, defaultAcquireBudget,
		"the RPC must outlast the acquire, or a slow DORA reports a timeout instead of its own error")
}
