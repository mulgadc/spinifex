package vpcendpoint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// countingKV records every read, listing and write a handler makes, runs
// stall once at the first stallOp of a key with stallPrefix ("updated" is
// after an Update lands), fails reads
// of failGet, and answers an error after applying updates while lostUpdates.
type countingKV struct {
	jetstream.KeyValue

	mu          sync.Mutex
	gets, lists []string
	writes      []string
	stallOp     string
	stallPrefix string
	stall       func()
	failGet     string
	lostUpdates bool
}

var errInjected = errors.New("injected KV failure")

func (k *countingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	k.gets = append(k.gets, key)
	fail := key == k.failGet
	k.mu.Unlock()
	if fail {
		return nil, errInjected
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *countingKV) ListKeysFiltered(ctx context.Context, filters ...string) (jetstream.KeyLister, error) {
	k.mu.Lock()
	k.lists = append(k.lists, filters...)
	k.mu.Unlock()
	return k.KeyValue.ListKeysFiltered(ctx, filters...)
}

func (k *countingKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	k.mu.Lock()
	k.lists = append(k.lists, ">")
	k.mu.Unlock()
	return k.KeyValue.Keys(ctx, opts...)
}

func (k *countingKV) wrote(op, key string) {
	k.mu.Lock()
	k.writes = append(k.writes, op+" "+key)
	k.mu.Unlock()
	k.maybeStall(op, key)
}

func (k *countingKV) maybeStall(op, key string) {
	k.mu.Lock()
	stall := k.stall
	if stall != nil && op == k.stallOp && strings.HasPrefix(key, k.stallPrefix) {
		k.stall = nil
	} else {
		stall = nil
	}
	k.mu.Unlock()
	if stall != nil {
		stall()
	}
}

func (k *countingKV) Put(ctx context.Context, key string, v []byte) (uint64, error) {
	k.wrote("put", key)
	return k.KeyValue.Put(ctx, key, v)
}

func (k *countingKV) Create(ctx context.Context, key string, v []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.wrote("create", key)
	return k.KeyValue.Create(ctx, key, v, opts...)
}

func (k *countingKV) Update(ctx context.Context, key string, v []byte, rev uint64) (uint64, error) {
	k.wrote("update", key)
	newRev, err := k.KeyValue.Update(ctx, key, v, rev)
	if err == nil {
		k.maybeStall("updated", key)
	}
	k.mu.Lock()
	lost := k.lostUpdates
	k.mu.Unlock()
	if err == nil && lost {
		return 0, errInjected
	}
	return newRev, err
}

func (k *countingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.wrote("delete", key)
	return k.KeyValue.Delete(ctx, key, opts...)
}

func (k *countingKV) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets, k.lists, k.writes = nil, nil, nil
}

func instrument(f *fixture) *countingKV {
	kv := &countingKV{KeyValue: f.svc.kv}
	f.svc.kv = kv
	return kv
}

// stallBefore pauses the next op on a key with prefix while during runs, as
// a request stalled between its writes would.
func stallBefore(kv *countingKV, op, prefix string, during func()) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	kv.stallOp, kv.stallPrefix, kv.stall = op, prefix, during
}

// stallBeforeRecordUpdate pauses the next record Update of the caller's account.
func stallBeforeRecordUpdate(kv *countingKV, during func()) {
	stallBefore(kv, "update", testAccount+".", during)
}

// seedUnrelated writes n available endpoints of another account, each with a
// table claim, so a handler that scans the bucket reads them.
func seedUnrelated(t *testing.T, f *fixture, n int) {
	t.Helper()
	g, ctx := errgroup.WithContext(t.Context())
	g.SetLimit(32)
	for i := range n {
		rec := &EndpointRecord{
			ID: fmt.Sprintf("vpce-%017x", 9000+i), AccountID: otherAccount, VpcID: fmt.Sprintf("vpc-%017x", 9000+i),
			ServiceName: s3Service, Type: TypeGateway, RouteTableIds: []string{fmt.Sprintf("rtb-%017x", 9000+i)},
			State: StateAvailable, Generation: 1, CreatedAt: f.clock.now(),
		}
		g.Go(func() error {
			if err := putJSON(ctx, f.svc.kv, recordKey(rec.AccountID, rec.ID), rec); err != nil {
				return err
			}
			return putJSON(ctx, f.svc.kv, claimKey(rec.RouteTableIds[0]), claim{VpceID: rec.ID, AccountID: rec.AccountID, ClaimedAt: rec.CreatedAt})
		})
	}
	require.NoError(t, g.Wait())
}

// Delete fences and removes the record, whatever the number of associations.
// Verifies: ADR-0003:S8.
func TestADR0003S8_DeleteWritesTwiceWhateverTheTables(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA2, rtbA3)
	kv := instrument(f)

	f.del(t, id)
	assert.Equalf(t, []string{"update " + recordKey(testAccount, id), "delete " + recordKey(testAccount, id)}, kv.writes,
		"ADR-0003:S8 a request never waits for work that scales with the resource: Delete must make two writes, not one per association")
	for _, rtb := range []string{rtbA1, rtbA2, rtbA3} {
		assert.Emptyf(t, f.routeOn(t, rtb), "ADR-0003:S8 the fence alone ends the association on %s", rtb)
	}
}

// With thousands of unrelated endpoints in the bucket, Create lists only the
// caller's records, and reads a bounded number of keys.
// Verifies: ADR-0003:S8.
func TestADR0003S8_CreateReadsOnlyCallerKeys(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	kv := instrument(f)

	f.create(t, rtbA1, rtbA2)
	for _, l := range kv.lists {
		assert.Equalf(t, testAccount+".*", l, "ADR-0003:S8 Create must list only the caller's records")
	}
	assert.Lessf(t, len(kv.gets), 20, "ADR-0003:S8 Create read %d keys with 2000 unrelated endpoints in the bucket", len(kv.gets))
}

// Describe lists only the caller's records, and by ID lists nothing.
// Verifies: ADR-0003:S8.
func TestADR0003S8_DescribeListsOnlyCallerRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	id := f.create(t, rtbA1)
	kv := instrument(f)

	out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.VpcEndpoints, 1)
	assert.Equalf(t, []string{testAccount + ".*"}, kv.lists, "ADR-0003:S8 Describe must list only the caller's records")
	assert.Lessf(t, len(kv.gets), 10, "ADR-0003:S8 Describe read %d keys for one endpoint", len(kv.gets))

	kv.reset()
	f.describe(t, id)
	assert.Empty(t, kv.lists, "ADR-0003:S8 Describe by ID lists nothing")
}

// DeleteVpc's endpoint check reads only the owner's records and writes nothing.
// Verifies: ADR-0003:S8.
func TestADR0003S8_DeleteVpcCheckReadsOnlyOwnerRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	id := f.create(t, rtbA1)
	kv := instrument(f)

	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcA)
	require.NoError(t, err)
	assert.True(t, blocked)
	assert.Equalf(t, []string{testAccount + ".*"}, kv.lists, "ADR-0003:S8 the DeleteVpc check must list only the owner's records")
	assert.Equalf(t, []string{recordKey(testAccount, id)}, kv.gets, "ADR-0003:S8 the DeleteVpc check must read only the owner's records")
	assert.Emptyf(t, kv.writes, "ADR-0003:S8 the DeleteVpc check must write nothing")
}

// Residue holds quota until swept, so an account's records, which every
// owner read visits, cannot grow past the cap.
// Verifies: ADR-0003:S8.
func TestADR0003S8_ResidueHoldsQuota(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := range accountLimit {
		state := StatePending
		if i%2 == 1 {
			state = StateDeleting
		}
		rec := &EndpointRecord{ID: fmt.Sprintf("vpce-%017x", 7000+i), AccountID: testAccount, VpcID: vpcA, ServiceName: s3Service,
			Type: TypeGateway, State: state, Generation: 1, CreatedAt: f.clock.now()}
		require.NoError(t, putJSON(t.Context(), f.svc.kv, recordKey(testAccount, rec.ID), rec))
	}

	_, err := f.createIn(t, vpcA)
	requireCode(t, err, awserrors.ErrorVpcEndpointLimitExceeded, "")
	assert.Lenf(t, f.keys(t), accountLimit, "ADR-0003:S8 residue must count against the quota so the owner's records stay bounded")
}

// DescribeRouteTables' lookup reads one claim and one record per table,
// whatever else the bucket holds.
// Verifies: ADR-0003:S8.
func TestADR0003S8_DescribeRouteTablesReadsTwicePerTable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	id := f.create(t, rtbA1)
	kv := instrument(f)

	got, err := f.svc.EffectiveEndpoints(t.Context(), []string{rtbA1, rtbA2})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{rtbA1: id}, got)
	assert.Emptyf(t, kv.lists, "ADR-0003:S8 the route lookup must list nothing")
	assert.LessOrEqualf(t, len(kv.gets), 4, "ADR-0003:S8 the route lookup read %d keys for two tables", len(kv.gets))
}

// A create stalled between its claims and its final write is fenced by a taker
// once its claim is stale, and then fails rather than reporting a route it lost.
// Verifies: ADR-0003:S5.
func TestADR0003S5_StalledCreateIsFencedByTaker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	kv := instrument(f)
	var taker string
	stallBeforeRecordUpdate(kv, func() {
		f.clock.advance(claimGrace)
		taker = f.create(t, rtbA1)
	})

	_, err := f.createIn(t, vpcA, rtbA1)
	requireCode(t, err, awserrors.ErrorRouteAlreadyExists, "")
	assert.Equalf(t, taker, f.routeOn(t, rtbA1), "ADR-0003:S5 stale work must never publish an authoritative result: the taker holds the table")
	out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.VpcEndpoints, 1, "ADR-0003:S5 the fenced create leaves no visible endpoint")
	assert.Equal(t, taker, aws.StringValue(out.VpcEndpoints[0].VpcEndpointId))
}

// Within the grace, a create in flight keeps its claim against another create.
// Verifies: ADR-0003:S5.
func TestADR0003S5_YoungClaimProtectsCreateInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	kv := instrument(f)
	stallBeforeRecordUpdate(kv, func() {
		_, err := f.createIn(t, vpcA, rtbA1)
		requireCode(t, err, awserrors.ErrorRouteAlreadyExists, "")
	})

	e, err := f.createIn(t, vpcA, rtbA1)
	require.NoError(t, err, "ADR-0003:S5 the stalled create completes once nothing took its claim")
	assert.Equal(t, aws.StringValue(e.VpcEndpointId), f.routeOn(t, rtbA1))
}

// A modify stalled after claiming an added table, whose claim a taker takes
// once stale, retries against the new holder and fails.
// Verifies: ADR-0003:S5.
func TestADR0003S5_StalledModifyIsFencedByTaker(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	kv := instrument(f)
	var taker string
	stallBeforeRecordUpdate(kv, func() {
		f.clock.advance(claimGrace)
		taker = f.create(t, rtbA2)
	})

	err := f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA2})})
	requireCode(t, err, awserrors.ErrorRouteAlreadyExists, "")
	assert.Equal(t, taker, f.routeOn(t, rtbA2))
	assert.Equalf(t, []string{rtbA1}, aws.StringValueSlice(f.describe(t, id).RouteTableIds),
		"ADR-0003:S5 the fenced modify never lists the table it lost")
}

// A modify stalled before its record write, whose endpoint is deleted meanwhile,
// answers not found, and the claim it took is stale at once.
// Verifies: ADR-0003:S5.
func TestADR0003S5_DeleteFenceStopsModifyInFlight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1)
	kv := instrument(f)
	stallBeforeRecordUpdate(kv, func() { f.del(t, id) })

	err := f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA2})})
	requireCode(t, err, awserrors.ErrorInvalidVpcEndpointIdNotFound, "")
	assert.Empty(t, f.routeOn(t, rtbA2))
	other := f.create(t, rtbA2)
	assert.Equalf(t, other, f.routeOn(t, rtbA2), "ADR-0003:S5 the abandoned modify's claim names a deleted endpoint, so another takes it at once")
}

// A removal paused after its record write, while a second modify re-adds the
// table, leaves the re-added association effective.
// Verifies: ADR-0003:S5.
func TestADR0003S5_DelayedRemovalKeepsReAdd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA2)
	kv := instrument(f)
	stallBefore(kv, "updated", testAccount+".", func() {
		require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA2})}))
	})

	require.NoError(t, f.modify(t, &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), RemoveRouteTableIds: aws.StringSlice([]string{rtbA2})}))
	assert.Equal(t, []string{rtbA1, rtbA2}, aws.StringValueSlice(f.describe(t, id).RouteTableIds))
	assert.Equalf(t, id, f.routeOn(t, rtbA2), "ADR-0003:S5 the delayed release must not erase the re-add's claim")
}

// Two modifies adding one table, the loser failing after the winner
// publishes, leave the published association effective.
// Verifies: ADR-0003:S5.
func TestADR0003S5_FailedModifyKeepsWinnersClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA3)
	kv := instrument(f)
	both := &ec2.ModifyVpcEndpointInput{VpcEndpointId: aws.String(id), AddRouteTableIds: aws.StringSlice([]string{rtbA2}), RemoveRouteTableIds: aws.StringSlice([]string{rtbA3})}
	stallBeforeRecordUpdate(kv, func() { require.NoError(t, f.modify(t, both)) })

	requireCode(t, f.modify(t, both), awserrors.ErrorInvalidParameter, "")
	assert.Equal(t, []string{rtbA1, rtbA2}, aws.StringValueSlice(f.describe(t, id).RouteTableIds))
	assert.Equalf(t, id, f.routeOn(t, rtbA2), "ADR-0003:S5 a failed modify must not release a claim the winner published")
}

// A create whose VPC is deleted while it publishes abandons and leaves nothing.
// Verifies: ADR-0003:S5.
func TestADR0003S5_CreateAbandonsWhenVpcGoes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	kv := instrument(f)
	stallBefore(kv, "create", testAccount+".", func() {
		require.NoError(t, f.svc.vpcKV.Delete(t.Context(), kvstore.AccountKey(testAccount, vpcA)))
	})

	_, err := f.createIn(t, vpcA, rtbA1)
	requireCode(t, err, awserrors.ErrorInvalidVpcIdNotFound, "The Vpc Id "+vpcA+" does not exist")
	assert.Empty(t, f.keys(t), "the abandoned create leaves no record or claim")
}

// A final write that landed but answered an error is the create's success.
// Verifies: ADR-0003:S5.
func TestADR0003S5_CreateResolvesAnUnknownFinalWrite(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	kv := instrument(f)
	kv.lostUpdates = true

	e, err := f.createIn(t, vpcA, rtbA1)
	require.NoError(t, err)
	kv.lostUpdates = false
	assert.Equal(t, aws.StringValue(e.VpcEndpointId), f.routeOn(t, rtbA1), "the landed create keeps its claim")
}

// What a crashed request leaves is never effective, does not block DeleteVpc
// past the grace, and is removed by the sweep only once older than the grace.
// Verifies: ADR-0003:S5.
func TestADR0003S5_CrashResidueIsInertAndSwept(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	now := f.clock.now()
	pending := &EndpointRecord{ID: "vpce-0c000000000000001", AccountID: testAccount, VpcID: vpcA, ServiceName: s3Service,
		Type: TypeGateway, RouteTableIds: []string{rtbA1}, State: StatePending, Generation: 1, CreatedAt: now}
	deleting := &EndpointRecord{ID: "vpce-0c000000000000002", AccountID: testAccount, VpcID: vpcB, ServiceName: s3Service,
		Type: TypeGateway, RouteTableIds: []string{rtbB1}, State: StateDeleting, Generation: 1, CreatedAt: now}
	for _, rec := range []*EndpointRecord{pending, deleting} {
		require.NoError(t, putJSON(ctx, f.svc.kv, recordKey(rec.AccountID, rec.ID), rec))
		require.NoError(t, putJSON(ctx, f.svc.kv, claimKey(rec.RouteTableIds[0]), claim{VpceID: rec.ID, AccountID: testAccount, ClaimedAt: now}))
	}

	out, err := f.svc.DescribeVpcEndpoints(ctx, &ec2.DescribeVpcEndpointsInput{}, testAccount)
	require.NoError(t, err)
	assert.Empty(t, out.VpcEndpoints, "ADR-0003:S5 pending and deleting records read as absent")
	assert.Empty(t, f.routeOn(t, rtbA1))
	assert.Empty(t, f.routeOn(t, rtbB1))
	blocked, err := f.svc.VpcBlocksDeletion(ctx, testAccount, vpcB)
	require.NoError(t, err)
	assert.False(t, blocked, "a deleting record does not block DeleteVpc")

	res := f.sweep(t)
	assert.Equal(t, SweepResult{Records: 1}, res, "before the grace only the deleting record goes")

	f.clock.advance(claimGrace)
	blocked, err = f.svc.VpcBlocksDeletion(ctx, testAccount, vpcA)
	require.NoError(t, err)
	assert.False(t, blocked, "a pending record past the grace does not block DeleteVpc")
	f.sweep(t)
	assert.Emptyf(t, f.keys(t), "ADR-0003:S5 the sweep removes every residue once past the grace")
	f.create(t, rtbA1, rtbA2)
}

// The sweep never removes an effective association, and removes a claim
// whose table is gone.
// Verifies: ADR-0003:S5.
func TestADR0003S5_SweepKeepsEffectiveAssociations(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.create(t, rtbA1, rtbA2)
	f.tables.drop(testAccount, rtbA2)
	f.clock.advance(claimGrace)

	res := f.sweep(t)
	assert.Equal(t, SweepResult{Keys: 1}, res, "only the deleted table's claim goes")
	assert.Equal(t, id, f.routeOn(t, rtbA1))
	assert.Equal(t, []string{rtbA1}, aws.StringValueSlice(f.describe(t, id).RouteTableIds))
}
