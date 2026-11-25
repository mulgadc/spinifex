package vpcpeering

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// countingKV records every key read and every listing a handler makes.
type countingKV struct {
	jetstream.KeyValue

	mu    sync.Mutex
	gets  []string
	lists []string
}

func (k *countingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	k.gets = append(k.gets, key)
	k.mu.Unlock()
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

func (k *countingKV) ListKeys(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	k.mu.Lock()
	k.lists = append(k.lists, ">")
	k.mu.Unlock()
	return k.KeyValue.ListKeys(ctx, opts...)
}

func (k *countingKV) reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets, k.lists = nil, nil
}

// seedUnrelated writes n active connections between VPCs the test never names,
// directly with their keys, so a handler that scans the bucket reads them.
// The writes are independent, so several run at once to cut the round trips.
func seedUnrelated(t *testing.T, f *fixture, n int) {
	t.Helper()
	g, ctx := errgroup.WithContext(t.Context())
	g.SetLimit(32)
	for i := range n {
		g.Go(func() error {
			rec := &PeeringRecord{
				ID:                 fmt.Sprintf("pcx-%017x", 5000+i),
				RequesterAccountID: "999999999999", RequesterVpcID: vpcID(10000 + 2*i), RequesterCIDR: "10.50.0.0/16",
				AccepterAccountID: "999999999999", AccepterVpcID: vpcID(10001 + 2*i), AccepterCIDR: "10.51.0.0/16",
				Status: StateActive, Generation: 2, RealizedGeneration: 2, CreatedAt: f.clock.now(),
			}
			if err := f.svc.writeIndexes(ctx, rec); err != nil {
				return err
			}
			if err := putJSON(ctx, f.svc.live, recordKey(rec.ID), rec); err != nil {
				return err
			}
			return putJSON(ctx, f.svc.live, pairKey(rec.RequesterVpcID, rec.AccepterVpcID), claim{PcxID: rec.ID, ClaimedAt: f.clock.now()})
		})
	}
	require.NoError(t, g.Wait())
}

func instrument(f *fixture) (live, term *countingKV) {
	live = &countingKV{KeyValue: f.svc.live}
	term = &countingKV{KeyValue: f.svc.term}
	f.svc.live, f.svc.term = live, term
	return live, term
}

// Accept records the link obligation and returns provisioning without waiting for the link.
// Verifies: ADR-0003:S8.
func TestADR0003S8_PeeringAcceptReturnsBeforeRealisation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId

	out, err := f.accept(t, id)
	require.NoError(t, err)
	require.Equalf(t, StateProvisioning, *out.Status.Code,
		"ADR-0003:S8 a request never waits for work that scales with the resource: Accept must return provisioning, not wait for the link")
	rec := f.liveRecord(t, id)
	assert.NotEmpty(t, rec.LinkPrefix, "ADR-0003:S8 the obligation carries the link the reconciler realises")
	assert.Greater(t, rec.Generation, rec.RealizedGeneration, "ADR-0003:S8 nothing is realised when Accept returns")
}

// Delete of a linked connection records CleanupPending and returns; the record
// stays until the reconciler has removed the link.
// Verifies: ADR-0003:S8.
func TestADR0003S8_PeeringDeleteReturnsBeforeCleanup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	f.activate(t, id)

	require.NoError(t, f.del(t, id))
	rec := f.liveRecord(t, id)
	require.NotNilf(t, rec, "ADR-0003:S8 Delete records the obligation and returns: the record must stay until MarkCleaned")
	assert.True(t, rec.CleanupPending, "ADR-0003:S8 the retained record carries the cleanup obligation")
	assert.NotEmpty(t, rec.LinkPrefix, "ADR-0003:S8 the link stays claimed until the reconciler removes it")
}

// With thousands of unrelated connections in the bucket, Create reads only the
// two VPCs' index keys, the records and claims they name, and one request claim.
// Verifies: ADR-0003:S8.
func TestADR0003S8_PeeringCreateReadsOnlyIndexes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	live, term := instrument(f)

	f.create(t, vpcA, vpcB)

	for _, l := range append(live.lists, term.lists...) {
		assert.Truef(t, strings.HasPrefix(l, "vpc."+vpcA+".") || strings.HasPrefix(l, "vpc."+vpcB+"."),
			"ADR-0003:S8 Create must list only the two VPCs' index keys, listed %q", l)
	}
	assert.Lessf(t, len(live.gets), 20, "ADR-0003:S8 Create read %d keys with 2000 unrelated connections in the bucket", len(live.gets))
	assert.Empty(t, term.gets)
}

// Describe reads only the caller's account index in both buckets and the records it names.
// Verifies: ADR-0003:S8.
func TestADR0003S8_PeeringDescribeReadsOnlyCallerIndex(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	f.create(t, vpcA, vpcB)
	live, term := instrument(f)

	out, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.VpcPeeringConnections, 1)
	for _, l := range append(live.lists, term.lists...) {
		assert.Equalf(t, "acct."+testAccount+".*", l, "ADR-0003:S8 Describe must list only the caller's account index, listed %q", l)
	}
	assert.Lessf(t, len(live.gets)+len(term.gets), 20, "ADR-0003:S8 Describe read %d keys for one connection", len(live.gets)+len(term.gets))

	live.reset()
	_, err = f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{
		VpcPeeringConnectionIds: aws.StringSlice([]string{*out.VpcPeeringConnections[0].VpcPeeringConnectionId}),
	}, testAccount)
	require.NoError(t, err)
	assert.Empty(t, live.lists, "ADR-0003:S8 Describe by ID lists nothing")
}

// writingKV fails the test on any write, so a read-only path proves it.
type writingKV struct {
	*countingKV

	t *testing.T
}

func (k *writingKV) Put(ctx context.Context, key string, v []byte) (uint64, error) {
	k.t.Errorf("ADR-0003:S8 DeleteVpc's peering check wrote %q", key)
	return k.countingKV.Put(ctx, key, v)
}

func (k *writingKV) Update(ctx context.Context, key string, v []byte, rev uint64) (uint64, error) {
	k.t.Errorf("ADR-0003:S8 DeleteVpc's peering check updated %q", key)
	return k.countingKV.Update(ctx, key, v, rev)
}

func (k *writingKV) Create(ctx context.Context, key string, v []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.t.Errorf("ADR-0003:S8 DeleteVpc's peering check created %q", key)
	return k.countingKV.Create(ctx, key, v, opts...)
}

func (k *writingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.t.Errorf("ADR-0003:S8 DeleteVpc's peering check deleted %q", key)
	return k.countingKV.Delete(ctx, key, opts...)
}

// With thousands of unrelated connections in the bucket, DeleteVpc's peering
// check reads only the VPC's index and the records it names, writes nothing,
// and a pending connection does not block; it reads rejected once the VPC goes.
// Verifies: ADR-0003:S8.
func TestADR0003S8_DeleteVpcRecordsNoPeeringFanOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedUnrelated(t, f, 2000)
	pending := *f.create(t, vpcA, vpcB).VpcPeeringConnectionId
	linked := *f.create(t, vpcA, vpcC).VpcPeeringConnectionId
	f.activate(t, linked)
	live, term := instrument(f)
	f.svc.live = &writingKV{countingKV: live, t: t}
	f.svc.term = &writingKV{countingKV: term, t: t}

	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), vpcA)
	require.NoError(t, err)
	assert.True(t, blocked, "an active connection blocks DeleteVpc")
	blocked, err = f.svc.VpcBlocksDeletion(t.Context(), vpcB)
	require.NoError(t, err)
	assert.False(t, blocked, "a pending connection does not block DeleteVpc")
	for _, l := range append(live.lists, term.lists...) {
		assert.Truef(t, strings.HasPrefix(l, "vpc."+vpcA+".") || strings.HasPrefix(l, "vpc."+vpcB+"."),
			"ADR-0003:S8 the DeleteVpc check must list only the VPC's index keys, listed %q", l)
	}
	assert.Lessf(t, len(live.gets), 20, "ADR-0003:S8 the DeleteVpc check read %d keys with 2000 unrelated connections", len(live.gets))

	f.deleteVPC(t, testAccount, vpcB)
	assert.Equal(t, StateRejected, f.status(t, pending), "a pending connection reads rejected once its VPC record is gone")
}
