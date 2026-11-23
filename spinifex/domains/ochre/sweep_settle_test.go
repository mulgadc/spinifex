package ochre

//test:in-package: builds real catalog bundle records with the package's
// unexported port assignment and key resolution.

import (
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func globalKey() string { return resolveKey(awsidentifiers.GlobalAccountID, testModelID) }

// globalHeld is a held READY bundle on the shared global account, its hold
// reserved by an earlier sweep.
func globalHeld(t *testing.T) EndpointRecord {
	t.Helper()
	rec := heldReadyBundle(t)
	rec.AccountID = awsidentifiers.GlobalAccountID
	return rec
}

// sweepSettlement is what sweep-2, led from another node than the VM's,
// presents for old to next at key, a revision it did not write.
func sweepSettlement(key string, old, next []byte) readiness.Settlement {
	return readiness.Settlement{
		TokenID: "sweep-2", Kind: readiness.TokenSweep, Epoch: 1, Node: "node-leader",
		Target: SweepTarget, Key: key, Revision: 7, Old: old, Next: next,
	}
}

func checkSweep(t *testing.T, key string, old, next EndpointRecord) error {
	t.Helper()
	return CheckSweepSettlement(sweepSettlement(key, encode(t, old), encode(t, next)))
}

func TestSweepSettlement_AllowsRecoveryOfAnotherSweepsRecords(t *testing.T) {
	held := globalHeld(t)
	require.NotEqual(t, "sweep-2", held.HealthHold.Token, "the hold was reserved by an earlier sweep")
	tenant := markedOwnedBundle(t)
	require.Equal(t, recoveryTenant, tenant.AccountID)
	global := markedOwnedBundle(t)
	global.AccountID = awsidentifiers.GlobalAccountID

	require.NoError(t, checkSweep(t, globalKey(), held, clearedOf(held)), "clear an earlier sweep's hold")
	require.NoError(t, checkSweep(t, drainKey(), tenant, promotedOf(t, tenant)), "promote a tenant's parked launch")
	require.NoError(t, checkSweep(t, globalKey(), global, promotedOf(t, global)), "promote a global parked launch")
}

func TestSweepSettlement_RefusesOutsideItsScope(t *testing.T) {
	held := globalHeld(t)
	ok := sweepSettlement(globalKey(), encode(t, held), encode(t, clearedOf(held)))
	require.NoError(t, CheckSweepSettlement(ok))

	tenantHeld := heldReadyBundle(t)
	for name, edit := range map[string]func(*readiness.Settlement){
		"drain token":           func(s *readiness.Settlement) { s.Kind = readiness.TokenDrain },
		"launch token":          func(s *readiness.Settlement) { s.Kind = readiness.TokenLaunch },
		"no token ID":           func(s *readiness.Settlement) { s.TokenID = "" },
		"zero epoch":            func(s *readiness.Settlement) { s.Epoch = 0 },
		"no token node":         func(s *readiness.Settlement) { s.Node = "" },
		"delete":                func(s *readiness.Settlement) { s.Next = nil },
		"no target":             func(s *readiness.Settlement) { s.Target = "" },
		"key as target":         func(s *readiness.Settlement) { s.Target = s.Key },
		"another scope":         func(s *readiness.Settlement) { s.Target = SweepTarget + "/tenants" },
		"record at another key": func(s *readiness.Settlement) { s.Key = drainKey() },
		"a tenant's held READY record": func(s *readiness.Settlement) {
			s.Key, s.Old, s.Next = drainKey(), encode(t, tenantHeld), encode(t, clearedOf(tenantHeld))
		},
		"ambiguous stored record": func(s *readiness.Settlement) {
			s.Old = []byte(strings.Replace(string(s.Old), `"state":"READY",`, `"state":"READY","State":"DRAINING",`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			edit(&s)
			require.ErrorIs(t, CheckSweepSettlement(s), errSweepSettlement)
		})
	}
}

func TestSweepSettlement_NeverStartsWorkOrChangesIdentity(t *testing.T) {
	held := globalHeld(t)
	unheld := clearedOf(held)
	claim := bareClaimRecord()
	realized := realizedOf(t, claim)
	marked := markedOwnedBundle(t)
	staleMarked := marked
	staleMarked.LastActiveAt = staleMarked.CreatedAt

	newHold := unheld
	newHold.HealthHold = &HealthHold{Token: "sweep-2", Epoch: 1, ReservedAt: time.Now().UTC()}
	newHold.Generation++
	moved := clearedOf(held)
	moved.InstanceID = "i-other"
	stale := clearedOf(held)
	stale.InFlight, stale.LastActiveAt = 5, held.LastActiveAt
	draining := clearedOf(held)
	draining.State = StateDraining

	for name, c := range map[string]struct {
		key       string
		old, next EndpointRecord
	}{
		"a new hold":                     {globalKey(), unheld, newHold},
		"realize a bare claim":           {drainKey(), claim, realized},
		"publish an unparked claim":      {drainKey(), realized, readyOf(t, realized)},
		"park an open claim":             {drainKey(), realized, parkedOf(realized, ParkReadinessTimedOut)},
		"a moved instance":               {globalKey(), held, moved},
		"a stale busy observation":       {globalKey(), held, stale},
		"a clear to DRAINING":            {globalKey(), held, draining},
		"promote with stale observation": {drainKey(), staleMarked, promotedOf(t, staleMarked)},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, checkSweep(t, c.key, c.old, c.next), errSweepSettlement)
		})
	}
	require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, staleMarked), encode(t, promotedOf(t, staleMarked)))),
		errDrainSettlement, "a drain cannot carry a stale observation into READY either")
}

type sweepEnv struct {
	c  *readiness.Control
	kv jetstream.KeyValue
}

func newSweepEnv(t *testing.T, settles readiness.SettleCheck, key string, value []byte) (sweepEnv, uint64) {
	t.Helper()
	c, kv, rev := newGuardedControl(t, settles, key, value)
	return sweepEnv{c: c, kv: kv}, rev
}

func (e sweepEnv) admit(t *testing.T, id, target string) *readiness.Operation {
	t.Helper()
	op, _, err := e.c.Admit(t.Context(), readiness.Token{ID: id, Kind: readiness.TokenSweep, Epoch: 1, Node: "node-leader", Target: target})
	require.NoError(t, err)
	return op
}

func (e sweepEnv) stored(t *testing.T, key string) (string, uint64) {
	t.Helper()
	entry, err := e.kv.Get(t.Context(), key)
	require.NoError(t, err)
	return string(entry.Value()), entry.Revision()
}

func TestSweepSettlement_NewSweepClearsAnEarlierSweepsHold(t *testing.T) {
	ctx := t.Context()
	held := globalHeld(t)
	unheld := held
	unheld.HealthHold, unheld.Generation = nil, held.Generation-1
	e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, unheld))

	first := e.admit(t, "sweep-1", SweepTarget)
	heldRev, err := first.WriteEndpoint(ctx, globalKey(), readiness.IntentHold, encode(t, held), rev)
	require.NoError(t, err)
	_, err = first.Release(ctx)
	require.NoError(t, err, "the probe failed and the first sweep ended with the hold in place")

	second := e.admit(t, "sweep-2", SweepTarget)
	next := encode(t, clearedOf(held))
	written, err := second.SettleEndpoint(ctx, globalKey(), next, heldRev)
	require.NoError(t, err)
	got, gotRev := e.stored(t, globalKey())
	assert.Equal(t, written, gotRev)
	assert.Equal(t, string(next), got)
}

func TestSweepSettlement_OneTokenCoversGlobalAndTenantKeys(t *testing.T) {
	ctx := t.Context()
	held := globalHeld(t)
	e, heldRev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
	tenant := markedOwnedBundle(t)
	tenantRev, err := e.kv.Put(ctx, drainKey(), encode(t, tenant))
	require.NoError(t, err)
	tenantHeld := heldReadyBundle(t)
	tenantHeldKey := resolveKey("000000000002", testModelID)
	tenantHeld.AccountID = "000000000002"
	tenantHeldRev, err := e.kv.Put(ctx, tenantHeldKey, encode(t, tenantHeld))
	require.NoError(t, err)

	sweep := e.admit(t, "sweep-2", SweepTarget)
	_, err = sweep.SettleEndpoint(ctx, globalKey(), encode(t, clearedOf(held)), heldRev)
	require.NoError(t, err)
	promoted := encode(t, promotedOf(t, tenant))
	_, err = sweep.SettleEndpoint(ctx, drainKey(), promoted, tenantRev)
	require.NoError(t, err, "a tenant's parked launch recovers")
	got, _ := e.stored(t, drainKey())
	assert.Equal(t, string(promoted), got)

	_, err = sweep.SettleEndpoint(ctx, tenantHeldKey, encode(t, clearedOf(tenantHeld)), tenantHeldRev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "a tenant's hold is outside the sweep's scope")
	_, gotRev := e.stored(t, tenantHeldKey)
	assert.Equal(t, tenantHeldRev, gotRev)

	rec, _, err := e.c.Read(ctx)
	require.NoError(t, err)
	var keys []string
	for _, j := range rec.Tokens["sweep-2"].Journal {
		keys = append(keys, j.Key)
	}
	assert.Equal(t, []string{globalKey(), drainKey()}, keys, "each settled key is journaled on the one token")
}

func TestSweepSettlement_GuardedRefusalsLeaveTheRecord(t *testing.T) {
	ctx := t.Context()
	held := globalHeld(t)
	next := encode(t, clearedOf(held))

	t.Run("token scoped to one key", func(t *testing.T) {
		e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
		_, err := e.admit(t, "sweep-2", globalKey()).SettleEndpoint(ctx, globalKey(), next, rev)
		require.ErrorIs(t, err, readiness.ErrNotSettlement)
		_, gotRev := e.stored(t, globalKey())
		assert.Equal(t, rev, gotRev)
	})
	t.Run("launch token on an unowned revision", func(t *testing.T) {
		e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
		op, _, err := e.c.Admit(ctx, readiness.Token{ID: "launch-1", Kind: readiness.TokenLaunch, Epoch: 1, Node: "node-leader", Target: SweepTarget})
		require.NoError(t, err)
		_, err = op.SettleEndpoint(ctx, globalKey(), next, rev)
		require.ErrorIs(t, err, readiness.ErrNotSettlement)
		_, gotRev := e.stored(t, globalKey())
		assert.Equal(t, rev, gotRev)
	})
	t.Run("sweep delete", func(t *testing.T) {
		e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
		_, err := e.admit(t, "sweep-2", SweepTarget).DeleteEndpoint(ctx, globalKey(), rev)
		require.ErrorIs(t, err, readiness.ErrNotSettlement)
		_, gotRev := e.stored(t, globalKey())
		assert.Equal(t, rev, gotRev)
	})
	t.Run("no rule", func(t *testing.T) {
		e, rev := newSweepEnv(t, nil, globalKey(), encode(t, held))
		_, err := e.admit(t, "sweep-2", SweepTarget).SettleEndpoint(ctx, globalKey(), next, rev)
		require.ErrorIs(t, err, readiness.ErrNotSettlement, "a control with no rule refuses every settle")
		_, gotRev := e.stored(t, globalKey())
		assert.Equal(t, rev, gotRev)
	})
}

func TestSweepSettlement_CompetingRevisionVoidsTheProbe(t *testing.T) {
	ctx := t.Context()
	held := globalHeld(t)
	e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
	sweep := e.admit(t, "sweep-2", SweepTarget)

	competing := clearedOf(held)
	competingRev, err := e.kv.Update(ctx, globalKey(), encode(t, competing), rev)
	require.NoError(t, err)

	_, err = sweep.SettleEndpoint(ctx, globalKey(), encode(t, clearedOf(held)), rev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the probed revision is gone")
	_, err = sweep.SettleEndpoint(ctx, globalKey(), encode(t, clearedOf(competing)), competingRev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the competing record has no hold to clear")
	got, gotRev := e.stored(t, globalKey())
	assert.Equal(t, competingRev, gotRev)
	assert.Equal(t, string(encode(t, competing)), got)
}

func TestSweepSettlement_FreezeLetsAnAdmittedSweepOnlySettle(t *testing.T) {
	ctx := t.Context()
	held := globalHeld(t)
	e, rev := newSweepEnv(t, CheckSweepSettlement, globalKey(), encode(t, held))
	unheld := clearedOf(globalHeld(t))
	unheldKey := resolveKey(awsidentifiers.GlobalAccountID, "other-model")
	unheld.ModelID = "other-model"
	unheldRev, err := e.kv.Put(ctx, unheldKey, encode(t, unheld))
	require.NoError(t, err)

	sweep := e.admit(t, "sweep-2", SweepTarget)
	_, controlRev, err := e.c.Read(ctx)
	require.NoError(t, err)
	_, err = e.c.Freeze(ctx, "rb-1", controlRev)
	require.NoError(t, err)

	_, _, err = e.c.Admit(ctx, readiness.Token{ID: "sweep-3", Kind: readiness.TokenSweep, Epoch: 1, Node: "node-leader", Target: SweepTarget})
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "no new sweep after the freeze")
	hold := unheld
	hold.HealthHold, hold.Generation = &HealthHold{Token: "sweep-2", Epoch: 1, ReservedAt: time.Now().UTC()}, unheld.Generation+1
	_, err = sweep.WriteEndpoint(ctx, unheldKey, readiness.IntentHold, encode(t, hold), unheldRev)
	require.ErrorIs(t, err, readiness.ErrNotAdmitted, "the admitted sweep adds no new hold after the freeze")

	next := encode(t, clearedOf(held))
	written, err := sweep.SettleEndpoint(ctx, globalKey(), next, rev)
	require.NoError(t, err, "the admitted sweep may still remove a hold")
	got, gotRev := e.stored(t, globalKey())
	assert.Equal(t, written, gotRev)
	assert.Equal(t, string(next), got)
}
