package tgw

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// recordWriteHook runs onCreate once, before the first create of an account
// record key, which is admission's record write.
type recordWriteHook struct {
	jetstream.KeyValue

	onCreate func()
}

func (h *recordWriteHook) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if h.onCreate != nil && strings.HasPrefix(key, testAccount+".") {
		f := h.onCreate
		h.onCreate = nil
		f()
	}
	return h.KeyValue.Create(ctx, key, value, opts...)
}

// sweepView is what a sweep paused after its absent-owner read holds.
type sweepView map[string]uint64

// pausedSweep reads every index key under prefixes past the grace and checks
// that the sweep would judge each ownerless, as sweepKey does before deleting.
func pausedSweep(t *testing.T, f *fixture, kv jetstream.KeyValue, prefixes ...string) sweepView {
	t.Helper()
	f.clock.Advance(claimGrace)
	view := sweepView{}
	for _, key := range keys(t, kv) {
		if !hasAnyPrefix(key, prefixes) {
			continue
		}
		entry, rev, err := getJSON[indexEntry](t.Context(), kv, key)
		require.NoError(t, err)
		require.NotNil(t, entry)
		live, err := f.svc.ownerLive(t.Context(), kv, entry.AccountID, idFromIndexKey(key))
		require.NoError(t, err)
		require.False(t, live, "owner of %s is not written yet", key)
		view[key] = rev
	}
	require.NotEmpty(t, view)
	return view
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// A sweep that judged the index keys ownerless before the record write loses
// its delete once admission succeeds, so the attachment still blocks every
// parent delete and stays in the account index.
func TestAttachmentAdmission_PausedSweepLosesIndexDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	var view sweepView
	hook := &recordWriteHook{KeyValue: f.svc.atts}
	hook.onCreate = func() { view = pausedSweep(t, f, hook.KeyValue, "acct.", "tgw.", "vpc.", "subnet.") }
	f.svc.atts = hook

	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	require.Len(t, view, 4)
	for key, rev := range view {
		require.ErrorIs(t, deleteAt(t.Context(), hook.KeyValue, key, rev), errConflict, key)
	}

	attID := aws.StringValue(att.TransitGatewayAttachmentId)
	blocked, err := f.svc.VpcBlocksDeletion(t.Context(), testAccount, vpcID)
	require.NoError(t, err)
	require.True(t, blocked)
	blocked, err = f.svc.SubnetBlocksDeletion(t.Context(), testAccount, subnetID)
	require.NoError(t, err)
	require.True(t, blocked)
	_, err = f.svc.DeleteTransitGateway(t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(tgwID)}, testAccount)
	require.Contains(t, requireCode(t, err, awserrors.ErrorIncorrectState), attID)
	ids, err := listIndex(t.Context(), hook.KeyValue, "acct."+testAccount+".")
	require.NoError(t, err)
	require.Equal(t, []string{attID}, ids)
}

// A sweep that removed the index keys during the pause makes admission fail
// and undo, so no attachment exists without its keys.
func TestAttachmentAdmission_SweptIndexFailsAdmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	hook := &recordWriteHook{KeyValue: f.svc.atts}
	hook.onCreate = func() {
		f.clock.Advance(claimGrace)
		var res SweepResult
		require.NoError(t, f.svc.sweepLive(t.Context(), &res))
		require.Positive(t, res.KeysRemoved)
	}
	f.svc.atts = hook

	_, err := f.attach(tgwID, vpcID, subnetID)
	requireCode(t, err, awserrors.ErrorServerInternal)
	for _, key := range keys(t, hook.KeyValue) {
		require.False(t, hasAnyPrefix(key, []string{testAccount + ".", "acct.", "vpc.", "subnet.", "tgw."}), "left %s", key)
	}
}

// The account key of a Transit Gateway has the same fence.
func TestCreateTransitGateway_PausedSweepLosesAccountKeyDelete(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var view sweepView
	hook := &recordWriteHook{KeyValue: f.svc.tgws}
	hook.onCreate = func() { view = pausedSweep(t, f, hook.KeyValue, "acct.") }
	f.svc.tgws = hook

	out, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
	require.Len(t, view, 1)
	for key, rev := range view {
		require.ErrorIs(t, deleteAt(t.Context(), hook.KeyValue, key, rev), errConflict, key)
	}
	ids, err := listIndex(t.Context(), hook.KeyValue, "acct."+testAccount+".")
	require.NoError(t, err)
	require.Equal(t, []string{aws.StringValue(out.TransitGateway.TransitGatewayId)}, ids)
}
