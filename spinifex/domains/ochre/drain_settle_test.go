package ochre

//test:in-package: builds real catalog bundle records with the package's
// unexported port assignment and key resolution.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var drainOwner = LaunchOwner{WorkerNonce: "nonce-1", TokenID: "launch-1", Epoch: 1}

// heldReadyBundle is a READY bundle with a sweep's health hold and no launch
// owner, as a record written before the owner field has.
func heldReadyBundle(t *testing.T) EndpointRecord {
	t.Helper()
	rec := catalogBundleRecord(t, testModelID, recoveryTenant, recoveryIP)
	rec.State = StateReady
	rec.CreatedAt = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	rec.ReadyAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rec.LastActiveAt = rec.ReadyAt.Add(time.Hour)
	rec.InFlight, rec.SuccessTotal = 2, 40
	rec.HealthHold = &HealthHold{Token: "sweep-1", Epoch: 1, ReservedAt: rec.ReadyAt.Add(2 * time.Hour)}
	rec.Generation = 5
	return rec
}

// clearedOf is rec with its hold cleared by a probe that saw it serving.
func clearedOf(rec EndpointRecord) EndpointRecord {
	rec.LastActiveAt = rec.LastActive().Add(time.Minute)
	rec.HealthHold = nil
	rec.Generation++
	return rec
}

// markedOwnedBundle is a parked STARTING bundle claimed by drainOwner.
func markedOwnedBundle(t *testing.T) EndpointRecord {
	t.Helper()
	rec := parkedBundle(t, recoveryTenant)
	rec.LaunchOwner = Owned(drainOwner)
	return rec
}

func promotedOf(t *testing.T, rec EndpointRecord) EndpointRecord {
	t.Helper()
	ref := catalogBundleRecord(t, testModelID, recoveryTenant, recoveryIP)
	rec.State = StateReady
	rec.ParkedReason, rec.ParkedAt = "", time.Time{}
	rec.BaseURL = ref.BaseURL
	rec.ReadyAt = time.Now().UTC()
	rec.Generation++
	return rec
}

func drainKey() string { return resolveKey(recoveryTenant, testModelID) }

func encode(t *testing.T, rec EndpointRecord) []byte {
	t.Helper()
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	return data
}

// drainSettlement is what a drain token led from another node than the VM's
// presents for old to next.
func drainSettlement(old, next []byte) readiness.Settlement {
	return readiness.Settlement{
		TokenID: "drain-1", Kind: readiness.TokenDrain, Epoch: 1, Rollback: "rb-1",
		Node: "node-leader", Target: drainKey(), Key: drainKey(), Revision: 7, Old: old, Next: next,
	}
}

func TestDrainSettlement_AllowsOnlyTheTwoTransitions(t *testing.T) {
	owned := heldReadyBundle(t)
	owned.LaunchOwner = Owned(drainOwner)
	marked := markedOwnedBundle(t)
	require.NotEqual(t, "node-leader", marked.NodeID, "the drain leader is not on the VM's node")
	idle := heldReadyBundle(t)
	idle.InFlight = 0
	busy := clearedOf(idle)
	busy.InFlight, busy.SuccessTotal, busy.LastActiveAt = 3, 41, time.Now().UTC()
	reset := clearedOf(idle)
	reset.ScrapeFailures, idle.ScrapeFailures = 0, 2
	counterReset := clearedOf(heldReadyBundle(t))
	counterReset.SuccessTotal, counterReset.LastActiveAt = 0, time.Now().UTC()
	completed := clearedOf(idle)
	completed.SuccessTotal, completed.LastActiveAt = 41, time.Now().UTC()
	idleCleared := clearedOf(idle)
	idleCleared.LastActiveAt = idle.LastActiveAt
	maxCounts := clearedOf(heldReadyBundle(t))
	maxCounts.InFlight, maxCounts.SuccessTotal, maxCounts.LastActiveAt = maxExactCount, maxExactCount, time.Now().UTC()

	for name, pair := range map[string][2]EndpointRecord{
		"clear the hold of a legacy READY record": {heldReadyBundle(t), clearedOf(heldReadyBundle(t))},
		"clear the hold of an owned READY record": {owned, clearedOf(owned)},
		"record a busy probe with the clear":      {idle, busy},
		"reset the failure count with the clear":  {idle, reset},
		"record a restarted success count":        {heldReadyBundle(t), counterReset},
		"record completed requests with no queue":  {idle, completed},
		"keep an idle sample's activity time":      {idle, idleCleared},
		"record counts at the metrics maximum":     {heldReadyBundle(t), maxCounts},
		"promote a marked complete bundle":        {marked, promotedOf(t, marked)},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, CheckDrainSettlement(drainSettlement(encode(t, pair[0]), encode(t, pair[1]))))
		})
	}
}

func TestDrainSettlement_RefusesOtherTokensAndTargets(t *testing.T) {
	old := heldReadyBundle(t)
	ok := drainSettlement(encode(t, old), encode(t, clearedOf(old)))
	require.NoError(t, CheckDrainSettlement(ok))
	other := catalogBundleRecord(t, testModelID, "000000000002", recoveryIP)
	other.State, other.ReadyAt, other.HealthHold, other.Generation = StateReady, old.ReadyAt, old.HealthHold, 5

	for name, edit := range map[string]func(*readiness.Settlement){
		"launch token":             func(s *readiness.Settlement) { s.Kind = readiness.TokenLaunch },
		"sweep token":              func(s *readiness.Settlement) { s.Kind = readiness.TokenSweep },
		"no token ID":              func(s *readiness.Settlement) { s.TokenID = "" },
		"zero epoch":               func(s *readiness.Settlement) { s.Epoch = 0 },
		"no rollback":              func(s *readiness.Settlement) { s.Rollback = "" },
		"no token node":            func(s *readiness.Settlement) { s.Node = "" },
		"delete":                   func(s *readiness.Settlement) { s.Next = nil },
		"no target":                func(s *readiness.Settlement) { s.Target = "" },
		"target is another key":    func(s *readiness.Settlement) { s.Target = resolveKey("000000000002", testModelID) },
		"target in another case":   func(s *readiness.Settlement) { s.Target = strings.ToUpper(s.Key) },
		"another account's record": func(s *readiness.Settlement) { s.Old, s.Next = encode(t, other), encode(t, clearedOf(other)) },
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			edit(&s)
			require.ErrorIs(t, CheckDrainSettlement(s), errDrainSettlement)
		})
	}
}

func TestDrainSettlement_ClearKeepsEveryOtherField(t *testing.T) {
	base := heldReadyBundle(t)
	require.NoError(t, CheckDrainSettlement(drainSettlement(encode(t, base), encode(t, clearedOf(base)))), "the unedited clear is allowed")
	for name, edit := range map[string]func(old, next *EndpointRecord){
		"no hold":            func(old, next *EndpointRecord) { old.HealthHold = nil },
		"hold with no token": func(old, next *EndpointRecord) { old.HealthHold.Token = "" },
		"hold with no epoch": func(old, next *EndpointRecord) { old.HealthHold.Epoch = 0 },
		"hold with no time":  func(old, next *EndpointRecord) { old.HealthHold.ReservedAt = time.Time{} },
		"READY with a marker": func(old, next *EndpointRecord) {
			old.ParkedReason, next.ParkedReason = ParkReadinessTimedOut, ParkReadinessTimedOut
		},
		"a new hold": func(old, next *EndpointRecord) {
			next.HealthHold = &HealthHold{Token: "drain-1", Epoch: 1, ReservedAt: time.Now().UTC()}
		},
		"hold kept":        func(old, next *EndpointRecord) { next.HealthHold = old.HealthHold },
		"to DRAINING":      func(old, next *EndpointRecord) { next.State = StateDraining },
		"to STARTING":      func(old, next *EndpointRecord) { next.State = StateStarting },
		"instance changed": func(old, next *EndpointRecord) { next.InstanceID = "i-other" },
		"VM node changed":  func(old, next *EndpointRecord) { next.NodeID = "node-leader" },
		"ENI changed":      func(old, next *EndpointRecord) { next.ENIID = "eni-other" },
		"volume changed":   func(old, next *EndpointRecord) { next.WeightsVolumeID = "vol-other" },
		"address changed":  func(old, next *EndpointRecord) { next.PrivateIP = "10.0.0.10" },
		"base URL changed": func(old, next *EndpointRecord) { next.BaseURL = "http://10.0.0.10:8000" },
		"member volume changed": func(old, next *EndpointRecord) {
			next.Members = cloneMembers(old.Members)
			for id, m := range next.Members {
				m.WeightsVolumeID = "vol-other"
				next.Members[id] = m
				break
			}
		},
		"member removed": func(old, next *EndpointRecord) {
			next.Members = cloneMembers(old.Members)
			for id := range next.Members {
				delete(next.Members, id)
				break
			}
		},
		"owner added":          func(old, next *EndpointRecord) { next.LaunchOwner = Owned(drainOwner) },
		"negative queue depth": func(old, next *EndpointRecord) { next.InFlight = -1 },
		"queue depth past the maximum": func(old, next *EndpointRecord) {
			next.InFlight, next.LastActiveAt = maxExactCount+1, time.Now().UTC()
		},
		"successes past the maximum": func(old, next *EndpointRecord) {
			next.SuccessTotal, next.LastActiveAt = maxExactCount+1, time.Now().UTC()
		},
		"completed requests, stale activity": func(old, next *EndpointRecord) {
			old.InFlight, next.InFlight, next.SuccessTotal, next.LastActiveAt = 0, 0, old.SuccessTotal+1, old.LastActiveAt
		},
		"queued requests, stale activity": func(old, next *EndpointRecord) { next.LastActiveAt = old.LastActiveAt },
		"successes reset, stale activity": func(old, next *EndpointRecord) {
			next.SuccessTotal, next.LastActiveAt = 0, old.LastActiveAt
		},
		"work shown at the old activity time": func(old, next *EndpointRecord) {
			next.SuccessTotal, next.LastActiveAt = old.SuccessTotal+1, old.LastActiveAt
		},
		"fractional successes": func(old, next *EndpointRecord) { next.SuccessTotal, next.LastActiveAt = 40.5, time.Now().UTC() },
		"negative successes":   func(old, next *EndpointRecord) { next.SuccessTotal, next.LastActiveAt = -1, time.Now().UTC() },
		"failures raised":      func(old, next *EndpointRecord) { next.ScrapeFailures = old.ScrapeFailures + 1 },
		"activity moved back":  func(old, next *EndpointRecord) { next.LastActiveAt = old.LastActiveAt.Add(-time.Second) },
		"activity before ready": func(old, next *EndpointRecord) {
			old.LastActiveAt = time.Time{}
			next.LastActiveAt = old.ReadyAt.Add(-time.Second)
		},
		"activity cleared": func(old, next *EndpointRecord) { next.LastActiveAt = time.Time{} },
		"no ready time":    func(old, next *EndpointRecord) { old.ReadyAt, next.ReadyAt = time.Time{}, time.Time{} },
		"no ENI":           func(old, next *EndpointRecord) { old.ENIID, next.ENIID = "", "" },
		"no VM node":       func(old, next *EndpointRecord) { old.NodeID, next.NodeID = "", "" },
		"not the primary's address": func(old, next *EndpointRecord) {
			old.BaseURL, next.BaseURL = "http://10.0.0.10:8000", "http://10.0.0.10:8000"
		},
		"incomplete member set": func(old, next *EndpointRecord) {
			old.Members = cloneMembers(old.Members)
			for id := range old.Members {
				if id != testModelID {
					delete(old.Members, id)
					break
				}
			}
			next.Members = old.Members
		},
		"ready time changed":   func(old, next *EndpointRecord) { next.ReadyAt = time.Now().UTC() },
		"pinned":               func(old, next *EndpointRecord) { next.Pinned = true },
		"generation kept":      func(old, next *EndpointRecord) { next.Generation = old.Generation },
		"generation skipped":   func(old, next *EndpointRecord) { next.Generation = old.Generation + 2 },
		"account changed":      func(old, next *EndpointRecord) { next.AccountID = "000000000002" },
		"model changed":        func(old, next *EndpointRecord) { next.ModelID = "other-model" },
		"from a DRAINING copy": func(old, next *EndpointRecord) { old.State, next.State = StateDraining, StateDraining },
	} {
		t.Run(name, func(t *testing.T) {
			old := heldReadyBundle(t)
			hold := *old.HealthHold
			old.HealthHold = &hold
			next := clearedOf(old)
			edit(&old, &next)
			require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, old), encode(t, next))), errDrainSettlement)
		})
	}

	owned := heldReadyBundle(t)
	owned.LaunchOwner = Owned(drainOwner)
	require.NoError(t, CheckDrainSettlement(drainSettlement(encode(t, owned), encode(t, clearedOf(owned)))))
	for name, owner := range map[string]OwnerField{
		"owner removed": {},
		"owner changed": Owned(LaunchOwner{WorkerNonce: "nonce-2", TokenID: "launch-2", Epoch: 2}),
	} {
		next := clearedOf(owned)
		next.LaunchOwner = owner
		require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, owned), encode(t, next))), errDrainSettlement, name)
	}
}

func TestDrainSettlement_PromotionNeedsAMarkedCompleteOwnedBundle(t *testing.T) {
	base := markedOwnedBundle(t)
	require.NoError(t, CheckDrainSettlement(drainSettlement(encode(t, base), encode(t, promotedOf(t, base)))), "the unedited promotion is allowed")
	for name, edit := range map[string]func(old, next *EndpointRecord){
		"no marker":              func(old, next *EndpointRecord) { old.ParkedReason, old.ParkedAt = "", time.Time{} },
		"no marker time":         func(old, next *EndpointRecord) { old.ParkedAt = time.Time{} },
		"unknown park reason":    func(old, next *EndpointRecord) { old.ParkedReason = "operator" },
		"legacy claim, no owner": func(old, next *EndpointRecord) { old.LaunchOwner, next.LaunchOwner = OwnerField{}, OwnerField{} },
		"no ENI":                 func(old, next *EndpointRecord) { old.ENIID, next.ENIID = "", "" },
		"no instance":            func(old, next *EndpointRecord) { old.InstanceID, next.InstanceID = "", "" },
		"held while starting": func(old, next *EndpointRecord) {
			old.HealthHold = &HealthHold{Token: "sweep-1", Epoch: 1, ReservedAt: time.Now().UTC()}
		},
		"member set incomplete": func(old, next *EndpointRecord) {
			old.Members = cloneMembers(old.Members)
			for id := range old.Members {
				if id != testModelID {
					delete(old.Members, id)
					break
				}
			}
			next.Members = old.Members
		},
		"marker kept":    func(old, next *EndpointRecord) { next.ParkedReason, next.ParkedAt = old.ParkedReason, old.ParkedAt },
		"no ready time":  func(old, next *EndpointRecord) { next.ReadyAt = time.Time{} },
		"wrong base URL": func(old, next *EndpointRecord) { next.BaseURL = "http://10.0.0.10:8000" },
		"a new hold": func(old, next *EndpointRecord) {
			next.HealthHold = &HealthHold{Token: "drain-1", Epoch: 1, ReservedAt: time.Now().UTC()}
		},
		"stays STARTING":          func(old, next *EndpointRecord) { next.State = StateStarting },
		"to DRAINING":             func(old, next *EndpointRecord) { next.State = StateDraining },
		"instance changed":        func(old, next *EndpointRecord) { next.InstanceID = "i-other" },
		"VM node is the leader's": func(old, next *EndpointRecord) { next.NodeID = "node-leader" },
		"owner changed": func(old, next *EndpointRecord) {
			next.LaunchOwner = Owned(LaunchOwner{WorkerNonce: "nonce-2", TokenID: "launch-1", Epoch: 1})
		},
		"owner removed":        func(old, next *EndpointRecord) { next.LaunchOwner = OwnerField{} },
		"created time changed": func(old, next *EndpointRecord) { next.CreatedAt = time.Now().UTC() },
		"generation kept":      func(old, next *EndpointRecord) { next.Generation = old.Generation },
		"activity seeded":      func(old, next *EndpointRecord) { next.LastActiveAt = next.ReadyAt },
	} {
		t.Run(name, func(t *testing.T) {
			old := markedOwnedBundle(t)
			next := promotedOf(t, old)
			edit(&old, &next)
			require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, old), encode(t, next))), errDrainSettlement)
		})
	}

	claim := markedOwnedBundle(t)
	claim.ParkedReason, claim.ParkedAt = "", time.Time{}
	ready := clearedOf(heldReadyBundle(t))
	require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, claim), encode(t, promotedOf(t, claim)))), errDrainSettlement,
		"an unparked claim may still have a running launch worker")
	require.ErrorIs(t, CheckDrainSettlement(drainSettlement(encode(t, ready), encode(t, clearedOf(ready)))), errDrainSettlement,
		"a READY record with no hold has nothing to settle")
}

func TestDrainSettlement_RefusesAmbiguousOrUnknownData(t *testing.T) {
	old := heldReadyBundle(t)
	good, next := string(encode(t, old)), encode(t, clearedOf(old))
	require.NoError(t, CheckDrainSettlement(drainSettlement([]byte(good), next)))

	splice := func(at, add string) string {
		require.Contains(t, good, at)
		return strings.Replace(good, at, at+add, 1)
	}
	for name, stored := range map[string]string{
		"key repeated":         splice(`"instance_id":"i-bundle",`, `"instance_id":"i-other",`),
		"key case alias":       strings.Replace(good, `"instance_id"`, `"Instance_ID"`, 1),
		"alias beside the key": splice(`"instance_id":"i-bundle",`, `"INSTANCE_ID":"i-bundle",`),
		"unknown field":        splice(`"instance_id":"i-bundle",`, `"future_field":1,`),
		"defaulted field":      splice(`"instance_id":"i-bundle",`, `"parked_reason":"",`),
		"hold key repeated":    splice(`"health_hold":{`, `"token":"sweep-2",`),
		"hold key alias":       strings.Replace(good, `"reserved_at"`, `"Reserved_At"`, 1),
		"member key repeated":  splice(`"members":{`, `"`+testModelID+`":{"port":1},`),
		"number rewritten":     strings.Replace(good, `"generation":5`, `"generation":5.0`, 1),
		"partial owner":        strings.TrimSuffix(good, "}") + `,"launch_owner":{"token_id":"launch-1"}}`,
		"ambiguous owner": strings.TrimSuffix(good, "}") +
			`,"launch_owner":{"worker_nonce":"n","token_id":"t","epoch":1},"Launch_Owner":{"worker_nonce":"n","token_id":"t","epoch":1}}`,
		"trailing data": good + `{}`,
		"not an object": `"READY"`,
		"null":          `null`,
		"truncated":     good[:len(good)-1],
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, CheckDrainSettlement(drainSettlement([]byte(stored), next)), errDrainSettlement)
			require.ErrorIs(t, CheckDrainSettlement(drainSettlement([]byte(good), []byte(stored))), errDrainSettlement,
				"the same data is refused as the new record")
		})
	}
}

func cloneMembers(m map[string]MemberEndpoint) map[string]MemberEndpoint {
	out := make(map[string]MemberEndpoint, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type drainEnv struct {
	c     *readiness.Control
	kv    jetstream.KeyValue
	drain func(target string) *readiness.Operation
}

// newGuardedControl stores value at key, then binds a deployment whose
// guarded control uses settles and activates epoch 1.
func newGuardedControl(t *testing.T, settles readiness.SettleCheck, key string, value []byte) (*readiness.Control, jetstream.KeyValue, uint64) {
	t.Helper()
	ctx := t.Context()
	_, _, js := testutil.StartTestJetStream(t)
	for _, b := range []string{"spinifex-cluster-config", KVBucketEndpoints} {
		_, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: b, History: 1})
		require.NoError(t, err)
	}
	kv, err := js.KeyValue(ctx, KVBucketEndpoints)
	require.NoError(t, err)
	rev, err := kv.Put(ctx, key, value)
	require.NoError(t, err)

	in := readiness.NewInitializer(js, readiness.InitConfig{ConfigBucket: "spinifex-cluster-config", EndpointBucket: KVBucketEndpoints})
	_, err = in.Initialize(ctx, readiness.FirstRollout{Confirmed: true})
	require.NoError(t, err)
	b, err := in.Load(ctx)
	require.NoError(t, err)
	c, err := readiness.NewGuarded(js, b, settles)
	require.NoError(t, err)
	initRev, err := c.Initialize(ctx)
	require.NoError(t, err)
	_, err = c.Activate(ctx, 1, initRev)
	require.NoError(t, err)
	return c, kv, rev
}

// newDrainEnv stores value at its key under a guarded control that uses
// CheckDrainSettlement, frozen for rollback rb-1.
func newDrainEnv(t *testing.T, value []byte) (drainEnv, uint64) {
	t.Helper()
	ctx := t.Context()
	c, kv, rev := newGuardedControl(t, CheckDrainSettlement, drainKey(), value)
	_, controlRev, err := c.Read(ctx)
	require.NoError(t, err)
	_, err = c.Freeze(ctx, "rb-1", controlRev)
	require.NoError(t, err)

	n := 0
	env := drainEnv{c: c, kv: kv, drain: func(target string) *readiness.Operation {
		n++
		tok := readiness.Token{ID: "drain-" + string(rune('0'+n)), Kind: readiness.TokenDrain, Epoch: 1, Node: "node-leader", Target: target}
		op, _, err := c.AdmitDrain(ctx, "rb-1", tok)
		require.NoError(t, err)
		return op
	}}
	return env, rev
}

func (e drainEnv) stored(t *testing.T) (string, uint64) {
	t.Helper()
	entry, err := e.kv.Get(t.Context(), drainKey())
	require.NoError(t, err)
	return string(entry.Value()), entry.Revision()
}

func TestDrainSettlement_GuardedOperationAppliesTheRule(t *testing.T) {
	ctx := t.Context()
	marked := markedOwnedBundle(t)
	held := heldReadyBundle(t)

	for name, pair := range map[string][2]EndpointRecord{
		"clear a hold":           {held, clearedOf(held)},
		"promote a marked claim": {marked, promotedOf(t, marked)},
	} {
		t.Run(name, func(t *testing.T) {
			e, rev := newDrainEnv(t, encode(t, pair[0]))
			next := encode(t, pair[1])

			_, err := e.drain(resolveKey("000000000002", testModelID)).SettleEndpoint(ctx, drainKey(), next, rev)
			require.ErrorIs(t, err, readiness.ErrNotSettlement, "a token for another key cannot settle this one")
			_, err = e.drain(drainKey()).DeleteEndpoint(ctx, drainKey(), rev)
			require.ErrorIs(t, err, readiness.ErrNotSettlement)
			got, gotRev := e.stored(t)
			require.Equal(t, rev, gotRev, "nothing was sent")
			require.Equal(t, string(encode(t, pair[0])), got)

			written, err := e.drain(drainKey()).SettleEndpoint(ctx, drainKey(), next, rev)
			require.NoError(t, err)
			got, gotRev = e.stored(t)
			assert.Equal(t, written, gotRev)
			assert.Equal(t, string(next), got)
		})
	}
}

func TestDrainSettlement_GuardedOperationYieldsToACompetingRevision(t *testing.T) {
	ctx := t.Context()
	held := heldReadyBundle(t)
	e, rev := newDrainEnv(t, encode(t, held))

	competing := clearedOf(held)
	competing.LastActiveAt = time.Now().UTC()
	competingRev, err := e.kv.Update(ctx, drainKey(), encode(t, competing), rev)
	require.NoError(t, err)

	op := e.drain(drainKey())
	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, clearedOf(held)), rev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the observed revision is gone")
	_, err = op.SettleEndpoint(ctx, drainKey(), encode(t, clearedOf(competing)), competingRev)
	require.ErrorIs(t, err, readiness.ErrNotSettlement, "the competing write left no hold to clear")
	got, gotRev := e.stored(t)
	assert.Equal(t, competingRev, gotRev)
	assert.Equal(t, string(encode(t, competing)), got)
}

func TestDrainSettlement_GuardedOperationRefusesMalformedRecords(t *testing.T) {
	ctx := t.Context()
	held := heldReadyBundle(t)
	good := string(encode(t, held))
	for name, stored := range map[string]string{
		"repeated key":  strings.Replace(good, `"state":"READY",`, `"state":"READY","state":"READY",`, 1),
		"unknown field": strings.Replace(good, `"state":"READY",`, `"state":"READY","future_field":1,`, 1),
		"not JSON":      "READY",
	} {
		t.Run(name, func(t *testing.T) {
			e, rev := newDrainEnv(t, []byte(stored))
			_, err := e.drain(drainKey()).SettleEndpoint(ctx, drainKey(), encode(t, clearedOf(held)), rev)
			require.ErrorIs(t, err, readiness.ErrNotSettlement)
			got, gotRev := e.stored(t)
			assert.Equal(t, rev, gotRev)
			assert.Equal(t, stored, got)
		})
	}
}
