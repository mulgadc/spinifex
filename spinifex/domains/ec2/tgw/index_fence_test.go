package tgw

import (
	"context"
	"fmt"
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

	onCreate    func()
	afterCreate func()
}

func (h *recordWriteHook) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if !strings.HasPrefix(key, testAccount+".") {
		return h.KeyValue.Create(ctx, key, value, opts...)
	}
	if h.onCreate != nil {
		f := h.onCreate
		h.onCreate = nil
		f()
	}
	rev, err := h.KeyValue.Create(ctx, key, value, opts...)
	if h.afterCreate != nil {
		f := h.afterCreate
		h.afterCreate = nil
		f()
	}
	return rev, err
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

// An admission that stopped after its record write, while a sweep removed its
// keys, leaves a pending attachment that no parent delete sees. The reconciler
// moves it to failed and never takes a link for it.
func TestPrepareAttachment_LostKeysFailAttachment(t *testing.T) {
	t.Parallel()
	for _, lost := range []string{"acct.", "tgw.", "vpc.", "subnet.", "pair."} {
		t.Run(lost, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			tgwID := f.availableTGW()
			vpcID, subnetID := f.network("vpc-00000001")
			att, err := f.attach(tgwID, vpcID, subnetID)
			require.NoError(t, err)
			attID := aws.StringValue(att.TransitGatewayAttachmentId)
			for _, key := range keys(t, f.svc.atts) {
				if strings.HasPrefix(key, lost) {
					require.NoError(t, deleteKey(t.Context(), f.svc.atts, key))
				}
			}

			rec, err := f.svc.PrepareAttachment(t.Context(), testAccount, attID)
			require.NoError(t, err)
			require.Nil(t, rec)
			term, err := f.svc.terminalAttachment(t.Context(), testAccount, attID)
			require.NoError(t, err)
			require.NotNil(t, term)
			require.Equal(t, StateFailed, term.State)
			for _, key := range keys(t, f.svc.atts) {
				require.False(t, hasAnyPrefix(key, []string{"link.", testAccount + "."}), "left %s", key)
			}
		})
	}
}

// A pair claim taken over by another attachment fails the reconcile the same way.
func TestPrepareAttachment_PairOwnedElsewhereFailsAttachment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	attID := aws.StringValue(att.TransitGatewayAttachmentId)
	_, err = putJSON(t.Context(), f.svc.atts, pairKey(tgwID, vpcID), claim{Owner: "tgw-attach-0000000000000000f", AccountID: testAccount})
	require.NoError(t, err)

	rec, err := f.svc.PrepareAttachment(t.Context(), testAccount, attID)
	require.NoError(t, err)
	require.Nil(t, rec)
	term, err := f.svc.terminalAttachment(t.Context(), testAccount, attID)
	require.NoError(t, err)
	require.Equal(t, StateFailed, term.State)
	held, _, err := getJSON[claim](t.Context(), f.svc.atts, pairKey(tgwID, vpcID))
	require.NoError(t, err)
	require.Equal(t, "tgw-attach-0000000000000000f", held.Owner, "the other holder keeps its claim")
}

// A reconcile that prepares the attachment between the record write and the
// admission's key check does not fail the admission.
func TestAttachmentAdmission_PrepareDuringAdmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tgwID := f.availableTGW()
	vpcID, subnetID := f.network("vpc-00000001")
	var prepared *AttachmentRecord
	hook := &recordWriteHook{KeyValue: f.svc.atts}
	hook.afterCreate = func() {
		ids, err := listIndex(t.Context(), hook.KeyValue, "acct."+testAccount+".")
		require.NoError(t, err)
		require.Len(t, ids, 1)
		prepared, err = f.svc.PrepareAttachment(t.Context(), testAccount, ids[0])
		require.NoError(t, err)
	}
	f.svc.atts = hook

	att, err := f.attach(tgwID, vpcID, subnetID)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.NotEmpty(t, prepared.LinkPrefix)
	rec, _, err := f.svc.attachment(t.Context(), testAccount, aws.StringValue(att.TransitGatewayAttachmentId))
	require.NoError(t, err)
	require.Equal(t, StatePending, rec.State)
	require.Equal(t, prepared.LinkPrefix, rec.LinkPrefix)
}

// A refused delete reads only as many index keys as it names, however many
// attachments the gateway has.
func TestLiveAttachmentsUnder_StopsAtLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, id := range []string{"tgw-attach-00000001", "tgw-attach-00000002", "tgw-attach-00000003"} {
		_, err := putJSON(t.Context(), f.svc.atts, tgwIndexKey("tgw-00000001", id), indexEntry{AccountID: testAccount, CreatedAt: f.svc.now()})
		require.NoError(t, err)
	}
	got, err := f.svc.liveAttachmentsUnder(t.Context(), "tgw.tgw-00000001.", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	got, err = f.svc.liveAttachmentsUnder(t.Context(), "tgw.tgw-00000001.", maxBlockingListed)
	require.NoError(t, err)
	require.Len(t, got, 3)
}

// Stale keys count against the read budget: a check that reads its budget of
// stale keys answers an error and never reads past it, and a blocker found
// within the budget still refuses the delete.
func TestLiveAttachmentsUnder_StaleKeysBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const prefix = "tgw.tgw-00000001."
	stale := f.svc.now().Add(-2 * claimGrace)
	for i := range maxIndexReads {
		_, err := putJSON(t.Context(), f.svc.atts, tgwIndexKey("tgw-00000001", fmt.Sprintf("tgw-attach-%08x", i+2)), indexEntry{AccountID: testAccount, CreatedAt: stale})
		require.NoError(t, err)
	}
	_, err := putJSON(t.Context(), f.svc.atts, tgwIndexKey("tgw-00000001", "tgw-attach-ffffffff"), indexEntry{AccountID: testAccount, CreatedAt: f.svc.now()})
	require.NoError(t, err)
	_, err = f.svc.liveAttachmentsUnder(t.Context(), prefix, maxBlockingListed)
	require.ErrorIs(t, err, errIndexScanBudget, "the young key is past the budget and is never read")

	g := newFixture(t)
	_, err = putJSON(t.Context(), g.svc.atts, tgwIndexKey("tgw-00000001", "tgw-attach-00000001"), indexEntry{AccountID: testAccount, CreatedAt: g.svc.now()})
	require.NoError(t, err)
	for i := range maxIndexReads {
		_, err := putJSON(t.Context(), g.svc.atts, tgwIndexKey("tgw-00000001", fmt.Sprintf("tgw-attach-%08x", i+2)), indexEntry{AccountID: testAccount, CreatedAt: stale})
		require.NoError(t, err)
	}
	got, err := g.svc.liveAttachmentsUnder(t.Context(), prefix, maxBlockingListed)
	require.NoError(t, err)
	require.Equal(t, []string{"tgw-attach-00000001"}, got)
}
