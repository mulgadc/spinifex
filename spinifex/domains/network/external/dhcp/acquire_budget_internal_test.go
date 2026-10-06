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

// newBackoffTestManager builds a Manager for schedule selection only; it never
// DORAs, so a bare fake client and store suffice.
func newBackoffTestManager(t *testing.T, cfg ManagerConfig) *Manager {
	t.Helper()
	cfg.Client = &Fake{}
	cfg.Store = &Store{}
	m, err := NewManager(cfg)
	assert.NoError(t, err)
	return m
}

// A failed gw-lrp acquire costs the whole VPC its external gateway until a
// reconcile pass returns, and nothing external times that DORA — the allocator
// calls the manager in-process. A failed ENI or EIP acquire blocks an AWS client
// that must hear back inside its read timeout. The purposes must not share a
// budget.
func TestAcquireBackoffIsPerPurpose(t *testing.T) {
	m := newBackoffTestManager(t, ManagerConfig{})

	gwSchedule, gwBudget := m.acquireBackoffFor(PurposeGatewayLRP)
	assert.Equal(t, gwLRPAcquireSchedule, gwSchedule)
	assert.Equal(t, gwLRPAcquireBudget, gwBudget)

	for _, purpose := range []string{PurposeEIP, PurposeENIPublic, PurposeNATGWExternal, ""} {
		schedule, budget := m.acquireBackoffFor(purpose)
		assert.Equal(t, defaultAcquireSchedule, schedule,
			"purpose %q must keep the default ladder: its caller is an AWS client that will retry untokened", purpose)
		assert.Equal(t, defaultAcquireBudget, budget,
			"purpose %q must keep the default budget", purpose)
	}
}

// The gw-lrp window has to be wider than the default, or the split buys nothing:
// the ladder was shortened for the EIP path alone and took gw-lrp down with it.
func TestGatewayLRPAcquireWindowExceedsDefault(t *testing.T) {
	ladder := func(schedule []time.Duration) time.Duration {
		total := time.Duration(0)
		for _, d := range schedule {
			total += d + acquireAttemptJitter
		}
		return total
	}
	assert.Greater(t, ladder(gwLRPAcquireSchedule), ladder(defaultAcquireSchedule),
		"the gw-lrp ladder must outlast the default one")
	assert.Greater(t, gwLRPAcquireBudget, defaultAcquireBudget,
		"the gw-lrp budget must outlast the default one")
	assert.GreaterOrEqual(t, gwLRPAcquireBudget, ladder(gwLRPAcquireSchedule),
		"the budget must leave room for the full ladder plus jitter, or later attempts never run")
}

// Both pairs stay configurable, so an operator on a slow upstream can widen
// either without a rebuild.
func TestAcquireBackoffConfigOverrides(t *testing.T) {
	m := newBackoffTestManager(t, ManagerConfig{
		AcquireSchedule:           []time.Duration{time.Second},
		AcquireBudget:             2 * time.Second,
		GatewayLRPAcquireSchedule: []time.Duration{3 * time.Second},
		GatewayLRPAcquireBudget:   4 * time.Second,
	})

	gwSchedule, gwBudget := m.acquireBackoffFor(PurposeGatewayLRP)
	assert.Equal(t, []time.Duration{3 * time.Second}, gwSchedule)
	assert.Equal(t, 4*time.Second, gwBudget)

	schedule, budget := m.acquireBackoffFor(PurposeEIP)
	assert.Equal(t, []time.Duration{time.Second}, schedule)
	assert.Equal(t, 2*time.Second, budget)
}
