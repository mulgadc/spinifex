package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureRegion = "ap-southeast-2"

// testClock is a settable clock shared by every store over one bucket.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ownerHookKV runs a test hook before or after a KV write and can replace the
// result, so a test fixes one interleaving or loses one server response.
type ownerHookKV struct {
	jetstream.KeyValue

	beforeUpdate func(key string)
	afterUpdate  func(key string, err error) error
	beforeDelete func(key string)
	afterDelete  func(key string, err error) error
	beforeGet    func(key string) error
}

func (kv *ownerHookKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if kv.beforeUpdate != nil {
		kv.beforeUpdate(key)
	}
	rev, err := kv.KeyValue.Update(ctx, key, value, last)
	if kv.afterUpdate != nil {
		err = kv.afterUpdate(key, err)
	}
	return rev, err
}

func (kv *ownerHookKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.beforeDelete != nil {
		kv.beforeDelete(key)
	}
	err := kv.KeyValue.Delete(ctx, key, opts...)
	if kv.afterDelete != nil {
		err = kv.afterDelete(key, err)
	}
	return err
}

func (kv *ownerHookKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if kv.beforeGet != nil {
		if err := kv.beforeGet(key); err != nil {
			return nil, err
		}
	}
	return kv.KeyValue.Get(ctx, key)
}

// leaderGet is the stream leader point read, which runs after the Get hook.
func (kv *ownerHookKV) leaderGet(js jetstream.JetStream) fixtureLeaderGet {
	get := leaderGetOf(js, kv.Bucket())
	return func(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
		if kv.beforeGet != nil {
			if err := kv.beforeGet(key); err != nil {
				return nil, err
			}
		}
		return get(ctx, key)
	}
}

type fixtureHarness struct {
	appliance *Store
	clock     *testClock
}

func newFixtureHarness(t *testing.T) *fixtureHarness {
	t.Helper()
	store := setupTestStore(t)
	clock := &testClock{t: time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)}
	store.now = clock.now
	return &fixtureHarness{appliance: store, clock: clock}
}

// owners returns an owner store over the shared bucket, as a separate daemon
// would see it, with KV calls passing through hooks.
func (h *fixtureHarness) owners() (*fixtureOwnerStore, *ownerHookKV) {
	owners := newFixtureOwnerStore(h.appliance, nil)
	kv := &ownerHookKV{KeyValue: owners.kv}
	owners.kv, owners.leaderGet = kv, kv.leaderGet(h.appliance.js)
	return owners, kv
}

func newFixtureRoot(id, name string) *LoadBalancerRecord {
	return &LoadBalancerRecord{
		LoadBalancerArn: resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, name, id, LoadBalancerTypeNetwork),
		LoadBalancerID:  id,
		Name:            name,
		Scheme:          SchemeInternal,
		Type:            LoadBalancerTypeNetwork,
		Realization:     realizationOVN,
		State:           StateProvisioning,
		VpcId:           "vpc-fixture",
		SecurityGroups:  []string{"sg-fixture"},
		Subnets:         []string{"subnet-fixture"},
		IPAddressType:   IPAddressTypeIPv4,
		AccountID:       testAccountID,
	}
}

func rawOwner(t *testing.T, s *Store, name string) fixtureOwner {
	t.Helper()
	entry, err := s.kv.Get(t.Context(), LBNameKey(name, testAccountID))
	require.NoError(t, err)
	owner, err := decodeFixtureOwner(entry.Value())
	require.NoError(t, err)
	return owner
}

// A pending root is invisible to the fixture readers and to every appliance
// read, and becomes readable by exact ARN only after its seal.
func TestFixtureOwner_PendingRootIsInvisibleUntilSealed(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, sealed, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	require.Nil(t, sealed)

	got, err := owners.GetSealed(ctx, root.Name, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "a pending root must not be readable by name")
	got, err = owners.GetSealedByArn(ctx, root.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "a pending root must not be readable by ARN")

	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err)
	assert.Equal(t, root.LoadBalancerArn, out.LoadBalancerArn)

	got, err = owners.GetSealedByArn(ctx, root.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, root.LoadBalancerID, got.LoadBalancerID)
	assert.Equal(t, realizationOVN, got.Realization)
	assert.Equal(t, StateProvisioning, got.State)
	stored := rawOwner(t, h.appliance, root.Name)
	assert.True(t, stored.Sealed)
	assert.Equal(t, uint64(fixtureOwnerRevision), stored.OwnerRevision)

	appARN := "arn:aws:elasticloadbalancing:" + fixtureRegion + ":" + testAccountID + ":loadbalancer/app/ovn-nlb/" + root.LoadBalancerID
	for name, arn := range map[string]string{
		"other account":    resourcearn.FormatELBv2LoadBalancer(fixtureRegion, "210987654321", root.Name, root.LoadBalancerID, LoadBalancerTypeNetwork),
		"other ID":         resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, root.Name, "bbbb000000000002", LoadBalancerTypeNetwork),
		"application path": appARN,
		"target group":     resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, root.Name, root.LoadBalancerID),
	} {
		got, err := owners.GetSealedByArn(ctx, arn, fixtureRegion, testAccountID)
		require.NoError(t, err, name)
		assert.Nil(t, got, name)
	}
	got, err = owners.GetSealedByArn(ctx, root.LoadBalancerArn, "us-east-1", testAccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "another region must not resolve the root")
	got, err = owners.GetSealedByArn(ctx, root.LoadBalancerArn, fixtureRegion, "210987654321")
	require.NoError(t, err)
	assert.Nil(t, got, "another caller account must not resolve the root")

	byArn, err := h.appliance.GetLoadBalancerByArn(ctx, root.LoadBalancerArn)
	require.NoError(t, err)
	assert.Nil(t, byArn, "the appliance ARN lookup must not resolve a fixture root")
	byName, err := h.appliance.GetLoadBalancerByName(ctx, root.Name, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, byName, "the appliance name lookup must not resolve a fixture root")
	list, err := h.appliance.ListLoadBalancersStrict(ctx)
	require.NoError(t, err)
	assert.Empty(t, list, "the appliance list must not include a fixture root")
}

// A seal whose server response is lost resolves to the same sealed root, and a
// create retry by the same owner reads that root back.
func TestFixtureOwner_LostSealResponseKeepsOriginalARN(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)

	kv.afterUpdate = func(_ string, err error) error {
		require.NoError(t, err, "the seal must reach the server")
		return nats.ErrTimeout
	}
	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err)
	assert.Equal(t, root.LoadBalancerArn, out.LoadBalancerArn)
	kv.afterUpdate = nil

	retry, _ := h.owners()
	again, sealed, err := retry.Begin(ctx, newFixtureRoot(root.LoadBalancerID, root.Name))
	require.NoError(t, err)
	assert.Nil(t, again, "a sealed root is not claimed again")
	require.NotNil(t, sealed)
	assert.Equal(t, root.LoadBalancerArn, sealed.LoadBalancerArn)

	other, sealed, err := retry.Begin(ctx, newFixtureRoot("bbbb000000000002", root.Name))
	require.NoError(t, err)
	assert.Nil(t, other)
	require.NotNil(t, sealed, "another create sees the sealed root to compare with")
	assert.Equal(t, root.LoadBalancerArn, sealed.LoadBalancerArn)
}

// A seal whose response is lost and whose re-read fails reports an error and
// leaves the row for a retry to resolve.
func TestFixtureOwner_UnreadableSealResultKeepsRow(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)

	readFail := errors.New("injected read failure")
	kv.afterUpdate = func(_ string, err error) error {
		require.NoError(t, err)
		return nats.ErrTimeout
	}
	kv.beforeGet = func(string) error { return readFail }
	_, err = owners.Seal(ctx, claim)
	require.ErrorIs(t, err, readFail)
	kv.afterUpdate, kv.beforeGet = nil, nil

	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err, "a retry resolves the earlier seal by re-reading")
	assert.Equal(t, root.LoadBalancerArn, out.LoadBalancerArn)
}

// An owner paused past its TTL before its seal loses the claim to a
// replacement, and can neither seal nor invalidate the replacement.
func TestFixtureOwner_ExpiredOwnerCannotSealOrReleaseReplacement(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	ctx := t.Context()
	first, firstKV := h.owners()
	second, _ := h.owners()
	oldRoot := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	newRoot := newFixtureRoot("bbbb000000000002", "ovn-nlb")

	oldClaim, _, err := first.Begin(ctx, oldRoot)
	require.NoError(t, err)

	_, _, err = second.Begin(ctx, newRoot)
	require.ErrorIs(t, err, errFixtureNameBusy, "an unexpired pending owner blocks a second create")

	var newClaim *fixtureClaim
	firstKV.beforeUpdate = func(string) {
		firstKV.beforeUpdate = nil
		h.clock.advance(h.appliance.claimTTL + time.Second)
		c, sealed, err := second.Begin(ctx, newRoot)
		require.NoError(t, err)
		require.Nil(t, sealed)
		newClaim = c
	}
	_, err = first.Seal(ctx, oldClaim)
	require.ErrorIs(t, err, errFixtureOwnerLost)
	require.NotNil(t, newClaim, "the replacement took the expired claim")

	require.ErrorIs(t, first.Invalidate(ctx, oldClaim), errFixtureOwnerLost)
	assert.Equal(t, newRoot.LoadBalancerID, rawOwner(t, h.appliance, newRoot.Name).OwnerID,
		"the expired owner must not remove the replacement")

	out, err := second.Seal(ctx, newClaim)
	require.NoError(t, err)
	assert.Equal(t, newRoot.LoadBalancerArn, out.LoadBalancerArn)
	require.ErrorIs(t, first.Invalidate(ctx, oldClaim), errFixtureOwnerLost)

	got, err := first.GetSealedByArn(ctx, oldRoot.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, got, "the expired owner's ARN never becomes readable")
	got, err = first.GetSealedByArn(ctx, newRoot.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, newRoot.LoadBalancerID, got.LoadBalancerID)
}

// A sealed root does not expire, and a sealed root cannot be invalidated.
func TestFixtureOwner_SealedRootDoesNotExpire(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	_, err = owners.Seal(ctx, claim)
	require.NoError(t, err)

	h.clock.advance(10 * h.appliance.claimTTL)
	other, sealed, err := owners.Begin(ctx, newFixtureRoot("bbbb000000000002", root.Name))
	require.NoError(t, err)
	assert.Nil(t, other, "a sealed root must not be taken over")
	require.NotNil(t, sealed)
	assert.Equal(t, root.LoadBalancerID, sealed.LoadBalancerID)

	require.ErrorIs(t, owners.Invalidate(ctx, claim), errFixtureSealed)
}

// Invalidation removes only the caller's exact pending claim, and a lost
// delete response still resolves to success.
func TestFixtureOwner_InvalidateExactPendingClaim(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	kv.afterDelete = func(_ string, err error) error {
		require.NoError(t, err)
		return nats.ErrTimeout
	}
	require.NoError(t, owners.Invalidate(ctx, claim))
	kv.afterDelete = nil

	_, err = h.appliance.kv.Get(ctx, claim.Key)
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	_, err = owners.Seal(ctx, claim)
	require.ErrorIs(t, err, errFixtureOwnerLost, "an invalidated claim cannot be sealed")

	next, _, err := owners.Begin(ctx, newFixtureRoot("bbbb000000000002", root.Name))
	require.NoError(t, err, "the name is free again without waiting for the TTL")
	require.NotNil(t, next)
}

// The fixture value keeps the appliance claim fields, and the appliance name
// and list operations behave as before in their own store.
func TestFixtureOwner_ApplianceStoreUnchanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fixture := newFixtureHarness(t)
	owners, _ := fixture.owners()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	entry, err := fixture.appliance.kv.Get(ctx, claim.Key)
	require.NoError(t, err)
	var legacy lbNameClaim
	require.NoError(t, json.Unmarshal(entry.Value(), &legacy), "the fixture value stays valid claim JSON")
	assert.Equal(t, root.LoadBalancerID, legacy.OwnerID)
	assert.Equal(t, fixture.clock.now(), legacy.ClaimedAt)

	appliance := setupTestStore(t)
	lb := newTestLB("cccc000000000003", "app-lb")
	ok, dup, err := appliance.ClaimLBName(ctx, lb.Name, testAccountID, lb.LoadBalancerID)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, dup)
	require.NoError(t, appliance.PutLoadBalancer(ctx, lb))
	ok, dup, err = appliance.ClaimLBName(ctx, lb.Name, testAccountID, "dddd000000000004")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.True(t, dup, "a live appliance LB keeps its name")

	list, err := appliance.ListLoadBalancersStrict(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, lb.LoadBalancerArn, list[0].LoadBalancerArn)
	byName, err := appliance.GetLoadBalancerByName(ctx, lb.Name, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, byName)
	assert.Equal(t, lb.LoadBalancerID, byName.LoadBalancerID)

	require.NoError(t, appliance.ReleaseLBName(ctx, lb.Name, testAccountID))
	require.NoError(t, appliance.ReleaseLBName(ctx, lb.Name, testAccountID), "release stays idempotent")
}

// Invalidation refuses a sealed root even at its current revision, and a seal
// that lands between the pending check and the delete wins.
func TestFixtureOwner_InvalidateRefusesSealedRevision(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	ctx := t.Context()
	owners, kv := h.owners()
	sealer, _ := h.owners()

	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	_, err = owners.Seal(ctx, claim)
	require.NoError(t, err)
	entry, err := h.appliance.kv.Get(ctx, claim.Key)
	require.NoError(t, err)
	current := *claim
	current.Revision = entry.Revision()
	require.ErrorIs(t, owners.Invalidate(ctx, &current), errFixtureSealed)
	got, err := owners.GetSealedByArn(ctx, root.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got, "the sealed root survives an invalidation at its own revision")

	raced := newFixtureRoot("bbbb000000000002", "ovn-nlb-raced")
	racedClaim, _, err := owners.Begin(ctx, raced)
	require.NoError(t, err)
	kv.beforeDelete = func(string) {
		kv.beforeDelete = nil
		_, err := sealer.Seal(ctx, racedClaim)
		require.NoError(t, err)
	}
	require.ErrorIs(t, owners.Invalidate(ctx, racedClaim), errFixtureSealed)
	got, err = owners.GetSealedByArn(ctx, raced.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got, "a seal that wins the race survives the invalidation")
}

// Seal writes the pending root that Begin persisted, not a root the caller
// changed after Begin.
func TestFixtureOwner_SealUsesPersistedPendingRoot(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)
	root.Subnets[0] = "subnet-changed"
	root.SecurityGroups = append(root.SecurityGroups, "sg-added")
	root.VpcId = "vpc-changed"

	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err)
	stored := rawOwner(t, h.appliance, "ovn-nlb")
	for name, got := range map[string]*LoadBalancerRecord{"returned": out, "stored": stored.Root} {
		assert.Equal(t, []string{"subnet-fixture"}, got.Subnets, name)
		assert.Equal(t, []string{"sg-fixture"}, got.SecurityGroups, name)
		assert.Equal(t, "vpc-fixture", got.VpcId, name)
	}
}

// claimOwner decodes the claim's persisted pending snapshot.
func claimOwner(t *testing.T, claim *fixtureClaim) fixtureOwner {
	t.Helper()
	owner, err := claim.snapshot()
	require.NoError(t, err)
	return owner
}

// A malformed or incomplete claim is a store error. It is never taken over or
// read as absent, and it stays untouched.
func TestFixtureOwner_MalformedClaimSurvives(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()

	cases := map[string][]byte{
		"not json":     []byte("aaaa000000000001"),
		"no root":      []byte(`{"ownerId":"aaaa000000000001","claimedAt":"2026-11-26T04:00:00Z"}`),
		"other ID":     []byte(`{"ownerId":"aaaa000000000001","claimedAt":"2026-11-26T04:00:00Z","ownerRevision":1,"root":{"load_balancer_id":"bbbb000000000002"}}`),
		"no claimedAt": []byte(`{"ownerId":"aaaa000000000001","ownerRevision":1,"sealed":true,"root":{"load_balancer_id":"aaaa000000000001"}}`),
	}
	i := 0
	for name, raw := range cases {
		i++
		lbName := "bad-" + strconv.Itoa(i)
		key := LBNameKey(lbName, testAccountID)
		rev, err := h.appliance.kv.Put(ctx, key, raw)
		require.NoError(t, err, name)
		h.clock.advance(h.appliance.claimTTL + time.Second)

		_, _, err = owners.Begin(ctx, newFixtureRoot("cccc000000000003", lbName))
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)
		_, err = owners.GetSealed(ctx, lbName, testAccountID)
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)
		arn := resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, lbName, "aaaa000000000001", LoadBalancerTypeNetwork)
		_, err = owners.GetSealedByArn(ctx, arn, fixtureRegion, testAccountID)
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)

		entry, err := h.appliance.kv.Get(ctx, key)
		require.NoError(t, err, name)
		assert.Equal(t, rev, entry.Revision(), name)
		assert.Equal(t, raw, entry.Value(), name)
	}
}

const (
	fixtureListenerID = "1111000000000001"
	fixtureGroupID    = "2222000000000002"
)

// fixtureListenerOf builds a valid listener of root with targets.
func fixtureListenerOf(root *LoadBalancerRecord, targets ...fixtureTarget) *fixtureListener {
	return &fixtureListener{
		ListenerArn:    resourcearn.FormatELBv2Listener(fixtureRegion, root.AccountID, root.Name, root.LoadBalancerID, fixtureListenerID, LoadBalancerTypeNetwork),
		Port:           80,
		TargetGroupArn: resourcearn.FormatELBv2TargetGroup(fixtureRegion, root.AccountID, "tcp-group", fixtureGroupID),
		TargetPort:     8080,
		Targets:        targets,
	}
}

// sealedFixture seals a fixture root and returns its claim key.
func sealedFixture(t *testing.T, owners *fixtureOwnerStore, root *LoadBalancerRecord) string {
	t.Helper()
	claim, _, err := owners.Begin(t.Context(), root)
	require.NoError(t, err)
	_, err = owners.Seal(t.Context(), claim)
	require.NoError(t, err)
	return claim.Key
}

// setListener is an edit that installs l and counts its calls.
func setListener(l *fixtureListener, calls *int) fixtureEdit {
	return func(fixtureOwner) (*fixtureListener, error) {
		*calls++
		return l, nil
	}
}

// addTargets is an edit that adds targets to the current listener in order.
func addTargets(calls *int, seen *[]fixtureOwner, targets ...fixtureTarget) fixtureEdit {
	return func(cur fixtureOwner) (*fixtureListener, error) {
		*calls++
		if seen != nil {
			*seen = append(*seen, cur)
		}
		next := *cur.Listener
		next.Targets = append(slices.Clone(next.Targets), targets...)
		slices.SortFunc(next.Targets, compareFixtureTargets)
		return &next, nil
	}
}

func kvEntry(t *testing.T, h *fixtureHarness, key string) jetstream.KeyValueEntry {
	t.Helper()
	entry, err := h.appliance.kv.Get(t.Context(), key)
	require.NoError(t, err)
	return entry
}

// An owner written in the revision-1 form, with no listener field, stays a
// readable sealed owner and is the base the first edit advances.
func TestFixtureOwner_RevisionOneOwnerStaysReadable(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")

	v1 := struct {
		OwnerID       string              `json:"ownerId"`
		ClaimedAt     time.Time           `json:"claimedAt"`
		OwnerRevision uint64              `json:"ownerRevision,omitempty"`
		Sealed        bool                `json:"sealed,omitempty"`
		Root          *LoadBalancerRecord `json:"root,omitempty"`
	}{root.LoadBalancerID, h.clock.now(), 1, true, root}
	raw, err := json.Marshal(v1)
	require.NoError(t, err)
	key := LBNameKey(root.Name, testAccountID)
	_, err = h.appliance.kv.Put(ctx, key, raw)
	require.NoError(t, err)

	got, err := owners.GetSealedOwner(ctx, root.Name, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(fixtureOwnerRevision), got.OwnerRevision)
	assert.Nil(t, got.Listener)
	byArn, err := owners.GetSealedByArn(ctx, root.LoadBalancerArn, fixtureRegion, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, byArn)
	assert.Equal(t, root.LoadBalancerArn, byArn.LoadBalancerArn)

	sealedKey := sealedFixture(t, owners, newFixtureRoot("bbbb000000000002", "ovn-new"))
	assert.NotContains(t, string(kvEntry(t, h, sealedKey).Value()), `"listener"`, "a new revision-1 owner keeps the revision-1 form")

	var calls int
	edited, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root), &calls))
	require.NoError(t, err)
	assert.Equal(t, uint64(2), edited.OwnerRevision)
	assert.Equal(t, 1, calls)
}

// Each changed edit advances the owner revision by exactly one and is read back
// whole; an unchanged edit writes nothing.
func TestFixtureOwner_EditAdvancesOneRevisionPerChange(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	key := sealedFixture(t, owners, root)

	var calls int
	listener := fixtureListenerOf(root)
	edited, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(listener, &calls))
	require.NoError(t, err)
	assert.Equal(t, uint64(2), edited.OwnerRevision)
	assert.Equal(t, listener, edited.Listener)

	before := kvEntry(t, h, key).Revision()
	same, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root), &calls))
	require.NoError(t, err)
	assert.Equal(t, uint64(2), same.OwnerRevision)
	assert.Equal(t, before, kvEntry(t, h, key).Revision(), "an unchanged edit must not write")

	targets := []fixtureTarget{{"i-0000000b", 8080}, {"i-0000000a", 9090}, {"i-0000000a", 8080}}
	edited, err = owners.Edit(ctx, key, root.LoadBalancerID, addTargets(&calls, nil, targets...))
	require.NoError(t, err)
	assert.Equal(t, uint64(3), edited.OwnerRevision)

	got, err := owners.GetSealedOwner(ctx, root.Name, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, uint64(3), got.OwnerRevision)
	assert.Equal(t, []fixtureTarget{{"i-0000000a", 8080}, {"i-0000000a", 9090}, {"i-0000000b", 8080}}, got.Listener.Targets)
	assert.Equal(t, listener.ListenerArn, got.Listener.ListenerArn)
	assert.Equal(t, root, got.Root, "an edit never changes the sealed root")

	retry, _ := h.owners()
	claim, sealed, err := retry.Begin(ctx, newFixtureRoot(root.LoadBalancerID, root.Name))
	require.NoError(t, err)
	assert.Nil(t, claim)
	require.NotNil(t, sealed, "a create retry reads the advanced root")
	assert.Equal(t, root.LoadBalancerArn, sealed.LoadBalancerArn)
	assert.Equal(t, uint64(3), rawOwner(t, h.appliance, root.Name).OwnerRevision)
}

// An edit of a missing, pending, replaced or malformed owner, a failing edit
// and an edit that breaks the snapshot invariants are refused with no write.
func TestFixtureOwner_EditRefusesWithoutWriting(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	key := sealedFixture(t, owners, root)
	var calls int
	_, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root, fixtureTarget{"i-0000000a", 80}), &calls))
	require.NoError(t, err)

	pendingRoot := newFixtureRoot("cccc000000000003", "ovn-pending")
	pending, _, err := owners.Begin(ctx, pendingRoot)
	require.NoError(t, err)
	badKey := LBNameKey("ovn-bad", testAccountID)
	_, err = h.appliance.kv.Put(ctx, badKey, []byte(`{"ownerId":"dddd000000000004"}`))
	require.NoError(t, err)

	calls = 0
	for name, c := range map[string]struct {
		key, owner string
		want       error
	}{
		"missing":   {LBNameKey("ovn-none", testAccountID), root.LoadBalancerID, errFixtureOwnerLost},
		"pending":   {pending.Key, pendingRoot.LoadBalancerID, errFixtureNotSealed},
		"replaced":  {key, "bbbb000000000002", errFixtureOwnerLost},
		"malformed": {badKey, "dddd000000000004", errFixtureClaimMalformed},
	} {
		_, err := owners.Edit(ctx, c.key, c.owner, setListener(fixtureListenerOf(root), &calls))
		require.ErrorIs(t, err, c.want, name)
	}
	assert.Zero(t, calls, "a refused owner never reaches the edit")

	before := kvEntry(t, h, key)
	editFail := errors.New("injected edit refusal")
	_, err = owners.Edit(ctx, key, root.LoadBalancerID, func(fixtureOwner) (*fixtureListener, error) { return nil, editFail })
	require.ErrorIs(t, err, editFail)

	tooMany := make([]fixtureTarget, fixtureMaxTargets+1)
	for i := range tooMany {
		tooMany[i] = fixtureTarget{fmt.Sprintf("i-%08x", 10+i), 80}
	}
	otherLB := newFixtureRoot("eeee000000000005", "ovn-other")
	zeroPort := fixtureListenerOf(root)
	zeroPort.Port = 0
	foreignGroup := fixtureListenerOf(root)
	foreignGroup.TargetGroupArn = resourcearn.FormatELBv2TargetGroup(fixtureRegion, "210987654321", "tcp-group", fixtureGroupID)
	_, err = owners.Edit(ctx, key, root.LoadBalancerID, setListener(nil, &calls))
	require.ErrorIs(t, err, errFixtureClaimMalformed, "an edit cannot remove the listener")
	broken := map[string]*fixtureListener{
		"listener port":     zeroPort,
		"target port":       fixtureListenerOf(root, fixtureTarget{"i-0000000a", 65536}),
		"too many":          fixtureListenerOf(root, tooMany...),
		"duplicate":         fixtureListenerOf(root, fixtureTarget{"i-0000000a", 80}, fixtureTarget{"i-0000000a", 80}),
		"unsorted":          fixtureListenerOf(root, fixtureTarget{"i-0000000b", 80}, fixtureTarget{"i-0000000a", 80}),
		"not an instance":   fixtureListenerOf(root, fixtureTarget{"10.0.0.1", 80}),
		"short instance ID": fixtureListenerOf(root, fixtureTarget{"i-0a", 80}),
		"other LB listener": fixtureListenerOf(otherLB),
		"foreign group":     foreignGroup,
	}
	for name, l := range broken {
		_, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(l, &calls))
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)
	}
	after := kvEntry(t, h, key)
	assert.Equal(t, before.Revision(), after.Revision(), "a refused edit must not write")
	assert.Equal(t, before.Value(), after.Value())
}

// The decoder refuses an owner whose revision, seal and listener disagree, so
// no reader or edit acts on it and the value stays as stored.
func TestFixtureOwner_SnapshotInvariantsOnRead(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()

	i := 0
	for name, c := range map[string]struct {
		revision uint64
		sealed   bool
		listener bool
	}{
		"pending with a listener":      {1, false, true},
		"pending past revision one":    {2, false, true},
		"sealed past one, no listener": {2, true, false},
		"sealed one with a listener":   {1, true, true},
	} {
		i++
		root := newFixtureRoot("aaaa00000000000"+strconv.Itoa(i), "ovn-inv-"+strconv.Itoa(i))
		owner := fixtureOwner{OwnerID: root.LoadBalancerID, ClaimedAt: h.clock.now(), OwnerRevision: c.revision, Sealed: c.sealed, Root: root}
		if c.listener {
			owner.Listener = fixtureListenerOf(root)
		}
		raw, err := json.Marshal(owner)
		require.NoError(t, err, name)
		key := LBNameKey(root.Name, testAccountID)
		rev, err := h.appliance.kv.Put(ctx, key, raw)
		require.NoError(t, err, name)

		_, err = owners.GetSealedOwner(ctx, root.Name, testAccountID)
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)
		var calls int
		_, err = owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root), &calls))
		require.ErrorIs(t, err, errFixtureClaimMalformed, name)
		entry := kvEntry(t, h, key)
		assert.Equal(t, rev, entry.Revision(), name)
		assert.Equal(t, raw, entry.Value(), name)
	}
}

// Two writers read the same revision. The loser's CAS conflicts and its edit
// runs again on the winner's snapshot, so the caller revalidates before retry.
func TestFixtureOwner_ConflictingEditRerunsOnLatest(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	other, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	key := sealedFixture(t, owners, root)
	var calls int
	_, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root), &calls))
	require.NoError(t, err)

	var otherCalls int
	raced := false
	kv.beforeUpdate = func(string) {
		if raced {
			return
		}
		raced = true
		won, err := other.Edit(ctx, key, root.LoadBalancerID, addTargets(&otherCalls, nil, fixtureTarget{"i-0000000b", 8080}))
		require.NoError(t, err)
		require.Equal(t, uint64(3), won.OwnerRevision)
	}
	calls = 0
	var seen []fixtureOwner
	edited, err := owners.Edit(ctx, key, root.LoadBalancerID, addTargets(&calls, &seen, fixtureTarget{"i-0000000a", 8080}))
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "a definite conflict reruns the edit")
	require.Len(t, seen, 2)
	assert.Equal(t, uint64(2), seen[0].OwnerRevision)
	assert.Equal(t, uint64(3), seen[1].OwnerRevision, "the rerun sees the winner's snapshot")
	assert.Equal(t, []fixtureTarget{{"i-0000000b", 8080}}, seen[1].Listener.Targets)
	assert.Equal(t, uint64(4), edited.OwnerRevision)
	assert.Equal(t, []fixtureTarget{{"i-0000000a", 8080}, {"i-0000000b", 8080}}, rawOwner(t, h.appliance, root.Name).Listener.Targets)
}

// A stored edit whose response is lost is adopted when the re-read shows the
// exact intended snapshot, with no second edit.
func TestFixtureOwner_EditLostAckAdoptsIntendedSnapshot(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	key := sealedFixture(t, owners, root)

	kv.afterUpdate = func(_ string, err error) error {
		require.NoError(t, err, "the edit must reach the server")
		return nats.ErrTimeout
	}
	var calls int
	edited, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root, fixtureTarget{"i-0000000a", 8080}), &calls))
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, uint64(2), edited.OwnerRevision)
	stored := rawOwner(t, h.appliance, root.Name)
	assert.Equal(t, *edited, stored)
}

// An edit with an unknown outcome whose re-read is not its intended snapshot,
// or cannot be read, is a retryable error. It is never rerun on the newer
// revision, and the durable owner stays as the re-read found it.
func TestFixtureOwner_UncertainEditIsNotRetried(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cases := map[string]struct {
		hook     func(t *testing.T, h *fixtureHarness, kv *ownerHookKV, other *fixtureOwnerStore, key, owner string)
		readFail bool
		// wantStored is the target set the durable owner holds afterwards.
		wantStored []fixtureTarget
		wantRev    uint64
	}{
		"not stored, newer writer": {
			hook: func(t *testing.T, _ *fixtureHarness, kv *ownerHookKV, other *fixtureOwnerStore, key, owner string) {
				kv.beforeUpdate = func(string) {
					kv.beforeUpdate = nil
					var n int
					_, err := other.Edit(ctx, key, owner, addTargets(&n, nil, fixtureTarget{"i-0000000b", 8080}))
					require.NoError(t, err)
				}
				kv.afterUpdate = func(_ string, err error) error {
					require.True(t, definiteKVConflict(err), "the stale write must not be stored")
					return nats.ErrTimeout
				}
			},
			wantStored: []fixtureTarget{{"i-0000000b", 8080}},
			wantRev:    3,
		},
		"stored, then superseded": {
			hook: func(t *testing.T, _ *fixtureHarness, kv *ownerHookKV, other *fixtureOwnerStore, key, owner string) {
				kv.afterUpdate = func(_ string, err error) error {
					require.NoError(t, err)
					kv.afterUpdate = nil
					var n int
					_, err = other.Edit(ctx, key, owner, addTargets(&n, nil, fixtureTarget{"i-0000000b", 8080}))
					require.NoError(t, err)
					return nats.ErrTimeout
				}
			},
			wantStored: []fixtureTarget{{"i-0000000a", 8080}, {"i-0000000b", 8080}},
			wantRev:    4,
		},
		"stored, re-read fails": {
			hook: func(t *testing.T, _ *fixtureHarness, kv *ownerHookKV, _ *fixtureOwnerStore, _, _ string) {
				kv.afterUpdate = func(_ string, err error) error {
					require.NoError(t, err)
					kv.beforeGet = func(string) error { return errInjectedRead }
					return nats.ErrTimeout
				}
			},
			readFail:   true,
			wantStored: []fixtureTarget{{"i-0000000a", 8080}},
			wantRev:    3,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFixtureHarness(t)
			owners, kv := h.owners()
			other, _ := h.owners()
			root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
			key := sealedFixture(t, owners, root)
			var calls int
			_, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root), &calls))
			require.NoError(t, err)

			c.hook(t, h, kv, other, key, root.LoadBalancerID)
			calls = 0
			_, err = owners.Edit(ctx, key, root.LoadBalancerID, addTargets(&calls, nil, fixtureTarget{"i-0000000a", 8080}))
			require.ErrorIs(t, err, errFixtureEditUncertain)
			if c.readFail {
				require.ErrorIs(t, err, errInjectedRead)
			}
			assert.Equal(t, 1, calls, "an uncertain edit is never rerun")

			stored := rawOwner(t, h.appliance, root.Name)
			assert.Equal(t, c.wantRev, stored.OwnerRevision)
			assert.Equal(t, c.wantStored, stored.Listener.Targets)
		})
	}
}

var errInjectedRead = errors.New("injected read failure")

// A create whose seal response is lost after an edit already advanced the same
// root resolves to that root, and the edit survives.
func TestFixtureOwner_LostSealAckAfterAcceptedEdit(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, kv := h.owners()
	other, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	claim, _, err := owners.Begin(ctx, root)
	require.NoError(t, err)

	listener := fixtureListenerOf(root, fixtureTarget{"i-0000000a", 8080})
	kv.afterUpdate = func(_ string, err error) error {
		require.NoError(t, err, "the seal must reach the server")
		var n int
		edited, err := other.Edit(ctx, claim.Key, root.LoadBalancerID, setListener(listener, &n))
		require.NoError(t, err)
		require.Equal(t, uint64(2), edited.OwnerRevision)
		return nats.ErrTimeout
	}
	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err)
	assert.Equal(t, root, out)
	kv.afterUpdate = nil

	again, sealed, err := owners.Begin(ctx, newFixtureRoot(root.LoadBalancerID, root.Name))
	require.NoError(t, err)
	assert.Nil(t, again)
	assert.Equal(t, root, sealed)
	stored := rawOwner(t, h.appliance, root.Name)
	assert.Equal(t, uint64(2), stored.OwnerRevision)
	assert.Equal(t, listener, stored.Listener)
}

// The parent dependency check reads an owner advanced past revision one and
// still blocks deletion of its VPC and subnet, also after the claim TTL.
func TestFixtureOwner_ParentGuardReadsAdvancedOwner(t *testing.T) {
	t.Parallel()
	h := newFixtureHarness(t)
	owners, _ := h.owners()
	ctx := t.Context()
	root := newFixtureRoot("aaaa000000000001", "ovn-nlb")
	key := sealedFixture(t, owners, root)
	slot := fixtureSlot{ClaimKey: key, OwnerID: root.LoadBalancerID, VpcID: root.VpcId, SubnetID: root.Subnets[0]}
	for _, k := range slot.keys(testAccountID) {
		require.NoError(t, owners.acquireSlot(ctx, testAccountID, k, slot))
	}
	var calls int
	edited, err := owners.Edit(ctx, key, root.LoadBalancerID, setListener(fixtureListenerOf(root, fixtureTarget{"i-0000000a", 8080}), &calls))
	require.NoError(t, err)
	require.Equal(t, uint64(2), edited.OwnerRevision)
	h.clock.advance(h.appliance.claimTTL + time.Second)

	blocks, err := owners.vpcBlocksDeletion(ctx, testAccountID, root.VpcId)
	require.NoError(t, err)
	assert.True(t, blocks, "the VPC stays a dependency of the advanced owner")
	blocks, err = owners.subnetBlocksDeletion(ctx, testAccountID, root.Subnets[0])
	require.NoError(t, err)
	assert.True(t, blocks, "the subnet stays a dependency of the advanced owner")
	for _, k := range slot.keys(testAccountID) {
		_, got, err := owners.readSlot(ctx, testAccountID, k)
		require.NoError(t, err)
		assert.Equal(t, &slot, got, "the dependency check keeps a live slot")
	}
}
