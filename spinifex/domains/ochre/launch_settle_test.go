package ochre

//test:in-package: builds real catalog bundle records with the package's
// unexported port assignment and key resolution.

import (
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var launchOwner = LaunchOwner{WorkerNonce: "nonce-1", TokenID: "launch-1", Epoch: 1}

// bareClaimRecord is a STARTING claim as admission writes it for launchOwner.
func bareClaimRecord() EndpointRecord {
	return EndpointRecord{
		AccountID: recoveryTenant, ModelID: testModelID, State: StateStarting, NodeID: testNodeID,
		CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Generation: 1,
		LaunchOwner: Owned(launchOwner),
	}
}

// realizedOf is claim with the complete bundle a launch records.
func realizedOf(t *testing.T, claim EndpointRecord) EndpointRecord {
	t.Helper()
	ref := catalogBundleRecord(t, testModelID, recoveryTenant, recoveryIP)
	claim.InstanceID, claim.ENIID, claim.PrivateIP = ref.InstanceID, ref.ENIID, ref.PrivateIP
	claim.WeightsVolumeID, claim.Members = ref.WeightsVolumeID, ref.Members
	claim.Generation++
	return claim
}

func parkedOf(rec EndpointRecord, reason ParkReason) EndpointRecord {
	rec.ParkedReason = reason
	rec.ParkedAt = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	rec.Generation++
	return rec
}

func readyOf(t *testing.T, rec EndpointRecord) EndpointRecord {
	t.Helper()
	rec.State = StateReady
	rec.BaseURL = catalogBundleRecord(t, testModelID, recoveryTenant, recoveryIP).BaseURL
	rec.ReadyAt = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	rec.Generation++
	return rec
}

// launchSettlement is what launch-1 presents for its own write of old.
func launchSettlement(old, next []byte) readiness.Settlement {
	return readiness.Settlement{
		TokenID: "launch-1", Kind: readiness.TokenLaunch, Epoch: 1, Node: testNodeID,
		Target: drainKey(), Key: drainKey(), Revision: 7, Owned: true, Old: old, Next: next,
	}
}

func checkLaunch(t *testing.T, old, next EndpointRecord) error {
	t.Helper()
	return CheckLaunchSettlement(launchSettlement(encode(t, old), encode(t, next)))
}

func TestLaunchSettlement_AllowsTheWorkersThreeSteps(t *testing.T) {
	claim := bareClaimRecord()
	realized := realizedOf(t, claim)
	pinned := bareClaimRecord()
	pinned.Pinned, pinned.OperatorReserved = true, true
	pinnedRealized := realizedOf(t, pinned)

	for name, pair := range map[string][2]EndpointRecord{
		"record the realized bundle":     {claim, realized},
		"park after a redirect":          {realized, parkedOf(realized, ParkReadinessRedirected)},
		"park after a timeout":           {realized, parkedOf(realized, ParkReadinessTimedOut)},
		"publish READY":                  {realized, readyOf(t, realized)},
		"record a pinned claim's bundle": {pinned, pinnedRealized},
		"publish a pinned claim READY":   {pinnedRealized, readyOf(t, pinnedRealized)},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, checkLaunch(t, pair[0], pair[1]))
		})
	}
}

func TestLaunchSettlement_RefusesOtherWorkersAndKeys(t *testing.T) {
	claim := bareClaimRecord()
	ok := launchSettlement(encode(t, claim), encode(t, realizedOf(t, claim)))
	require.NoError(t, CheckLaunchSettlement(ok))

	legacy := bareClaimRecord()
	legacy.LaunchOwner = OwnerField{}
	other := bareClaimRecord()
	other.AccountID = "000000000002"

	for name, edit := range map[string]func(*readiness.Settlement){
		"drain token":                func(s *readiness.Settlement) { s.Kind = readiness.TokenDrain },
		"sweep token":                func(s *readiness.Settlement) { s.Kind = readiness.TokenSweep },
		"no token ID":                func(s *readiness.Settlement) { s.TokenID = "" },
		"zero epoch":                 func(s *readiness.Settlement) { s.Epoch = 0 },
		"no token node":              func(s *readiness.Settlement) { s.Node = "" },
		"revision not this worker's": func(s *readiness.Settlement) { s.Owned = false },
		"delete":                     func(s *readiness.Settlement) { s.Next = nil },
		"no target":                  func(s *readiness.Settlement) { s.Target = "" },
		"target is another key":      func(s *readiness.Settlement) { s.Target = resolveKey("000000000002", testModelID) },
		"another token ID":           func(s *readiness.Settlement) { s.TokenID = "launch-2" },
		"another epoch":              func(s *readiness.Settlement) { s.Epoch = 2 },
		"token on another node":      func(s *readiness.Settlement) { s.Node = "node-b" },
		"claim with no owner": func(s *readiness.Settlement) {
			s.Old, s.Next = encode(t, legacy), encode(t, realizedOf(t, legacy))
		},
		"another account's claim": func(s *readiness.Settlement) {
			s.Old, s.Next = encode(t, other), encode(t, realizedOf(t, other))
		},
		"ambiguous stored record": func(s *readiness.Settlement) {
			s.Old = []byte(strings.Replace(string(s.Old), `"state":"STARTING",`, `"state":"STARTING","State":"READY",`, 1))
		},
		"unknown field in the new record": func(s *readiness.Settlement) {
			s.Next = []byte(strings.Replace(string(s.Next), `"state":"STARTING",`, `"state":"STARTING","future_field":1,`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			edit(&s)
			require.ErrorIs(t, CheckLaunchSettlement(s), errLaunchSettlement)
		})
	}
}

func TestLaunchSettlement_IdentityStepRecordsOnlyTheBundle(t *testing.T) {
	base := bareClaimRecord()
	require.NoError(t, checkLaunch(t, base, realizedOf(t, base)))

	for name, edit := range map[string]func(old, next *EndpointRecord){
		"no ENI":             func(old, next *EndpointRecord) { next.ENIID = "" },
		"no instance":        func(old, next *EndpointRecord) { next.InstanceID = "" },
		"no address":         func(old, next *EndpointRecord) { next.PrivateIP = "" },
		"primary volume off": func(old, next *EndpointRecord) { next.WeightsVolumeID = "vol-other" },
		"member missing": func(old, next *EndpointRecord) {
			next.Members = cloneMembers(next.Members)
			for id := range next.Members {
				delete(next.Members, id)
				break
			}
		},
		"member port changed": func(old, next *EndpointRecord) {
			next.Members = cloneMembers(next.Members)
			for id, m := range next.Members {
				m.Port++
				next.Members[id] = m
				break
			}
		},
		"base URL set early": func(old, next *EndpointRecord) { next.BaseURL = "http://" + recoveryIP + ":8000" },
		"READY at once":      func(old, next *EndpointRecord) { *next = readyOf(t, *next) },
		"parked at once":     func(old, next *EndpointRecord) { *next = parkedOf(*next, ParkReadinessTimedOut) },
		"a new hold": func(old, next *EndpointRecord) {
			next.HealthHold = &HealthHold{Token: "launch-1", Epoch: 1, ReservedAt: time.Now().UTC()}
		},
		"owner changed": func(old, next *EndpointRecord) {
			next.LaunchOwner = Owned(LaunchOwner{WorkerNonce: "nonce-2", TokenID: "launch-1", Epoch: 1})
		},
		"owner removed":        func(old, next *EndpointRecord) { next.LaunchOwner = OwnerField{} },
		"VM node changed":      func(old, next *EndpointRecord) { next.NodeID = "node-b" },
		"created time changed": func(old, next *EndpointRecord) { next.CreatedAt = time.Now().UTC() },
		"pinned":               func(old, next *EndpointRecord) { next.Pinned = true },
		"activity added":       func(old, next *EndpointRecord) { next.LastActiveAt = time.Now().UTC() },
		"generation kept":      func(old, next *EndpointRecord) { next.Generation = old.Generation },
		"generation skipped":   func(old, next *EndpointRecord) { next.Generation = old.Generation + 2 },
		"to DRAINING":          func(old, next *EndpointRecord) { next.State = StateDraining },
		"partly realized claim": func(old, next *EndpointRecord) {
			old.InstanceID = next.InstanceID
		},
		"claim with activity": func(old, next *EndpointRecord) {
			old.LastActiveAt, next.LastActiveAt = old.CreatedAt, old.CreatedAt
		},
		"claim with no creation time": func(old, next *EndpointRecord) { old.CreatedAt, next.CreatedAt = time.Time{}, time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			old := bareClaimRecord()
			next := realizedOf(t, old)
			edit(&old, &next)
			require.ErrorIs(t, checkLaunch(t, old, next), errLaunchSettlement)
		})
	}
}

func TestLaunchSettlement_FinalStepParksOrPublishesTheSameBundle(t *testing.T) {
	realized := realizedOf(t, bareClaimRecord())
	require.NoError(t, checkLaunch(t, realized, readyOf(t, realized)))
	require.NoError(t, checkLaunch(t, realized, parkedOf(realized, ParkReadinessRedirected)))

	for name, next := range map[string]func(old EndpointRecord) EndpointRecord{
		"READY with no ready time": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.ReadyAt = time.Time{}
			return n
		},
		"READY on another address": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.BaseURL = "http://10.0.0.10:8000"
			return n
		},
		"READY with a new instance": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.InstanceID = "i-other"
			return n
		},
		"READY and parked": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.ParkedReason, n.ParkedAt = ParkReadinessTimedOut, n.ReadyAt
			return n
		},
		"READY with a hold": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.HealthHold = &HealthHold{Token: "launch-1", Epoch: 1, ReservedAt: n.ReadyAt}
			return n
		},
		"READY seeds activity": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.LastActiveAt = n.ReadyAt
			return n
		},
		"READY keeps generation": func(old EndpointRecord) EndpointRecord {
			n := readyOf(t, old)
			n.Generation = old.Generation
			return n
		},
		"park with an unknown reason": func(old EndpointRecord) EndpointRecord { return parkedOf(old, "operator") },
		"park with no time": func(old EndpointRecord) EndpointRecord {
			n := parkedOf(old, ParkReadinessTimedOut)
			n.ParkedAt = time.Time{}
			return n
		},
		"park with a new ENI": func(old EndpointRecord) EndpointRecord {
			n := parkedOf(old, ParkReadinessTimedOut)
			n.ENIID = "eni-other"
			return n
		},
		"park with a base URL": func(old EndpointRecord) EndpointRecord {
			n := parkedOf(old, ParkReadinessTimedOut)
			n.BaseURL = readyOf(t, old).BaseURL
			return n
		},
		"identity rewritten": func(old EndpointRecord) EndpointRecord {
			old.InstanceID = "i-other"
			old.Generation++
			return old
		},
		"back to a bare claim": func(old EndpointRecord) EndpointRecord {
			n := bareClaimRecord()
			n.Generation = old.Generation + 1
			return n
		},
		"to DRAINING": func(old EndpointRecord) EndpointRecord {
			old.State = StateDraining
			old.Generation++
			return old
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkLaunch(t, realized, next(realized)), errLaunchSettlement)
		})
	}
}

func TestLaunchSettlement_RefusesWritesAfterTheFinalStep(t *testing.T) {
	realized := realizedOf(t, bareClaimRecord())
	parked := parkedOf(realized, ParkReadinessTimedOut)
	ready := readyOf(t, realized)

	cleared := parked
	cleared.ParkedReason, cleared.ParkedAt = "", time.Time{}
	cleared.Generation++
	for name, pair := range map[string][2]EndpointRecord{
		"READY after a park":     {parked, readyOf(t, cleared)},
		"park again":             {parked, parkedOf(parked, ParkReadinessRedirected)},
		"clear the park":         {parked, cleared},
		"park a READY record":    {ready, parkedOf(ready, ParkReadinessTimedOut)},
		"rewrite a READY record": {ready, readyOf(t, ready)},
		"hold a READY record":    {ready, heldOf(ready)},
		"identity on a park":     {parked, realizedOf(t, parked)},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkLaunch(t, pair[0], pair[1]), errLaunchSettlement)
		})
	}
}

// A STARTING record with a stray marker field or a reaper observation is not
// an open claim, so no launch step can carry it forward.
func TestLaunchSettlement_RefusesInconsistentOpenClaims(t *testing.T) {
	strays := map[string]func(*EndpointRecord){
		"marker time with no reason": func(r *EndpointRecord) { r.ParkedAt = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC) },
		"activity time":              func(r *EndpointRecord) { r.LastActiveAt = r.CreatedAt },
		"queue depth":                func(r *EndpointRecord) { r.InFlight = 1 },
		"success count":              func(r *EndpointRecord) { r.SuccessTotal = 1 },
		"failure count":              func(r *EndpointRecord) { r.ScrapeFailures = 1 },
	}
	steps := map[string]struct {
		old  func() EndpointRecord
		next func(EndpointRecord) EndpointRecord
	}{
		"identity": {old: bareClaimRecord, next: func(r EndpointRecord) EndpointRecord { return realizedOf(t, r) }},
		"park": {
			old:  func() EndpointRecord { return realizedOf(t, bareClaimRecord()) },
			next: func(r EndpointRecord) EndpointRecord { return parkedOf(r, ParkReadinessTimedOut) },
		},
		"READY": {
			old:  func() EndpointRecord { return realizedOf(t, bareClaimRecord()) },
			next: func(r EndpointRecord) EndpointRecord { return readyOf(t, r) },
		},
	}
	for stepName, step := range steps {
		base := step.old()
		require.NoError(t, checkLaunch(t, base, step.next(base)), "the clean %s step is allowed", stepName)
		for strayName, stray := range strays {
			t.Run(stepName+"/"+strayName, func(t *testing.T) {
				old := step.old()
				stray(&old)
				err := checkLaunch(t, old, step.next(old))
				require.ErrorIs(t, err, errLaunchSettlement)
				require.ErrorContains(t, err, "not an open launch claim")
			})
		}
	}
}

func heldOf(rec EndpointRecord) EndpointRecord {
	rec.HealthHold = &HealthHold{Token: "launch-1", Epoch: 1, ReservedAt: rec.ReadyAt}
	rec.Generation++
	return rec
}

// newLaunchEnv is an active guarded control using CheckLaunchSettlement with
// launch-1 admitted for the claim key from testNodeID.
func newLaunchEnv(t *testing.T) (drainEnv, *readiness.Operation) {
	t.Helper()
	c, kv, _ := newGuardedControl(t, CheckLaunchSettlement, EndpointKey(recoveryTenant, "seed"), []byte(`{"state":"READY"}`))
	return drainEnv{c: c, kv: kv}, admitLaunch(t, c, "launch-1")
}

func admitLaunch(t *testing.T, c *readiness.Control, id string) *readiness.Operation {
	t.Helper()
	op, _, err := c.Admit(t.Context(), readiness.Token{ID: id, Kind: readiness.TokenLaunch, Epoch: 1, Node: testNodeID, Target: drainKey()})
	require.NoError(t, err)
	return op
}

func TestLaunchSettlement_GuardedOperationRunsTheWorkersSteps(t *testing.T) {
	ctx := t.Context()
	for name, final := range map[string]func(EndpointRecord) EndpointRecord{
		"to READY": func(r EndpointRecord) EndpointRecord { return readyOf(t, r) },
		"to park":  func(r EndpointRecord) EndpointRecord { return parkedOf(r, ParkReadinessRedirected) },
	} {
		t.Run(name, func(t *testing.T) {
			e, op := newLaunchEnv(t)
			claim := bareClaimRecord()
			rev, err := op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claim), 0)
			require.NoError(t, err)

			realized := realizedOf(t, claim)
			rev, err = op.SettleEndpoint(ctx, drainKey(), encode(t, realized), rev)
			require.NoError(t, err)
			_, err = op.DeleteEndpoint(ctx, drainKey(), rev)
			require.ErrorIs(t, err, readiness.ErrNotSettlement, "the worker never removes a realized claim")

			done := final(realized)
			rev, err = op.SettleEndpoint(ctx, drainKey(), encode(t, done), rev)
			require.NoError(t, err)
			got, gotRev := e.stored(t)
			assert.Equal(t, rev, gotRev)
			assert.Equal(t, string(encode(t, done)), got)

			_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, readyOf(t, done)), rev)
			require.ErrorIs(t, err, readiness.ErrNotSettlement, "nothing follows the final write")
			_, gotRev = e.stored(t)
			assert.Equal(t, rev, gotRev)
		})
	}
}

func TestLaunchSettlement_GuardedOperationYieldsToAChangedRevision(t *testing.T) {
	ctx := t.Context()
	e, op := newLaunchEnv(t)
	claim := bareClaimRecord()
	rev, err := op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claim), 0)
	require.NoError(t, err)

	realized := realizedOf(t, claim)
	changedRev, err := e.kv.Update(ctx, drainKey(), encode(t, realized), rev)
	require.NoError(t, err)

	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, realized), rev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the claim's revision is gone")
	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, readyOf(t, realized)), changedRev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the worker did not write the new revision")
	got, gotRev := e.stored(t)
	assert.Equal(t, changedRev, gotRev)
	assert.Equal(t, string(encode(t, realized)), got)
}

func TestLaunchSettlement_GuardedOperationRefusesAnotherWorkersClaim(t *testing.T) {
	ctx := t.Context()
	e, _ := newLaunchEnv(t)
	op := admitLaunch(t, e.c, "launch-2")
	claim := bareClaimRecord()
	rev, err := op.WriteEndpoint(ctx, drainKey(), readiness.IntentClaim, encode(t, claim), 0)
	require.NoError(t, err)

	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, realizedOf(t, claim)), rev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "launch-2 wrote a claim that names launch-1")
	got, gotRev := e.stored(t)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, string(encode(t, claim)), got)
}
