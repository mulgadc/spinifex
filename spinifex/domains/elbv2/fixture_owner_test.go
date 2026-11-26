package elbv2

import (
	"context"
	"encoding/json"
	"errors"
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
	owners.kv = kv
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
	claim.Owner.Root.Subnets[0] = "subnet-changed"
	claim.Owner.Root.VpcId = "vpc-changed"

	out, err := owners.Seal(ctx, claim)
	require.NoError(t, err)
	stored := rawOwner(t, h.appliance, "ovn-nlb")
	for name, got := range map[string]*LoadBalancerRecord{"returned": out, "stored": stored.Root} {
		assert.Equal(t, []string{"subnet-fixture"}, got.Subnets, name)
		assert.Equal(t, []string{"sg-fixture"}, got.SecurityGroups, name)
		assert.Equal(t, "vpc-fixture", got.VpcId, name)
	}
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
