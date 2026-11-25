package tgw

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopAfterRecordWrite lets the first account record create through, then
// stops the request, as a crash before the account check does.
type stopAfterRecordWrite struct {
	jetstream.KeyValue

	stopped bool
}

type crashed struct{}

func (h *stopAfterRecordWrite) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	rev, err := h.KeyValue.Create(ctx, key, value, opts...)
	if err == nil && !h.stopped && len(key) > len(testAccount) && key[:len(testAccount)+1] == testAccount+"." {
		h.stopped = true
		panic(crashed{})
	}
	return rev, err
}

func crash(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		r := recover()
		require.IsType(t, crashed{}, r)
	}()
	call()
}

func state(t *testing.T, f *fixture, kv jetstream.KeyValue, id string) string {
	t.Helper()
	if kv == f.svc.tgws {
		rec, _, err := f.svc.transitGateway(t.Context(), testAccount, id)
		require.NoError(t, err)
		require.NotNil(t, rec)
		return rec.State
	}
	rec, _, err := f.svc.attachment(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.NotNil(t, rec)
	return rec.State
}

func onlyRecordID(t *testing.T, kv jetstream.KeyValue) string {
	t.Helper()
	var ids []string
	for _, k := range keys(t, kv) {
		if _, id, ok := recordID(k); ok {
			ids = append(ids, id)
		}
	}
	require.Len(t, ids, 1)
	return ids[0]
}

// A create that stopped after its record write, past account deletion's final
// reread, leaves a pending gateway; once the account record is gone the sweep
// moves it to deleting after the grace.
func TestSweep_CreateStoppedBeforeAccountCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hook := &stopAfterRecordWrite{KeyValue: f.svc.tgws}
	f.svc.tgws = hook
	crash(t, func() {
		_, _ = f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	})
	f.svc.tgws = hook.KeyValue
	id := onlyRecordID(t, f.svc.tgws)
	require.Equal(t, StatePending, state(t, f, f.svc.tgws, id))
	f.accounts.remove(testAccount)

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.Orphaned, "a record younger than the grace may still be checking its account")
	f.clock.Advance(claimGrace)
	res, err = f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.Orphaned)
	assert.Equal(t, StateDeleting, state(t, f, f.svc.tgws, id))
}

// The same for an attachment, which also moves to deleting.
func TestSweep_AttachmentOfRemovedAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	hook := &stopAfterRecordWrite{KeyValue: f.svc.atts}
	f.svc.atts = hook
	crash(t, func() { _, _ = f.attach(tgwID, vpcID, subnetID) })
	f.svc.atts = hook.KeyValue
	attID := onlyRecordID(t, f.svc.atts)
	f.accounts.remove(testAccount)
	f.clock.Advance(claimGrace)

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, res.Orphaned)
	assert.Equal(t, StateDeleting, state(t, f, f.svc.atts, attID))
	assert.Equal(t, StateDeleting, state(t, f, f.svc.tgws, tgwID))
}

// The sweep never deletes a resource whose account record exists, in any
// status: a suspended or terminating account keeps it for its owner or for
// account deletion.
func TestSweep_KeepsResourcesOfExistingAccounts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	f.accounts.mu.Lock()
	f.accounts.active = false
	f.accounts.mu.Unlock()
	f.clock.Advance(transitGatewayWindow + claimGrace)

	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, res.Orphaned)
	assert.Equal(t, StateAvailable, state(t, f, f.svc.tgws, tgwID))
	assert.Equal(t, StatePending, state(t, f, f.svc.atts, aws.StringValue(att.TransitGatewayAttachmentId)))
}
